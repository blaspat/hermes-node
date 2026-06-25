// Package exec hosts the persistent shell executor used by hermes-node
// to evaluate `exec` calls from the brain.
//
// The shell is a long-lived `bash -i` subprocess held open over a
// stdin pipe; each Run writes one command terminated by an in-band
// framing block:
//
//	__HERMES_BEGIN_<seq>__
//	<user command>
//	__HERMES_END_<seq>__
//	EXIT <n>
//	__HERMES_CWD_<sid>__<absolute pwd>__HERMES_CWD_<sid>__
//
// The framing mirrors the marker convention used by Hermes Agent's
// `tools/environments/ssh.py` — the BEGIN/END pair lets the reader
// know when stdout has flushed, the EXIT line carries the real `$?`,
// the CWD pair carries the post-command working directory. State
// persists across calls because the bash process keeps its own
// environment, cwd, shell variables, and function table between
// invocations.
//
// Concurrency: a single goroutine owns the stdout pipe. It reads
// every line bash emits, demuxes them by sequence number, and
// delivers the per-call output to the Run that asked for it. This
// avoids the "two readers on one FD lose data" hazard that a naive
// split would have.
//
// Stderr is merged into the same pipe as stdout (cmd.Stderr = cmd.Stdout),
// so both streams arrive interleaved through the same demuxer. The
// Run signature's stderr return is always empty; stderr content is
// mixed into the stdout string.
package exec

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrClosed is returned by Run after Close has been called or the
// underlying bash process has exited. Callers can use errors.Is to
// distinguish "session is gone" from genuine command failures.
var ErrClosed = errors.New("shell: session is closed")

// maxStderrBytes is the cap on accumulated stderr per session. When
// exceeded, further stderr is silently dropped to prevent OOM. The
// 10 MB limit matches the existing output cap in handler_exec.go.
const maxStderrBytes = 10 * 1024 * 1024

// pendingCall is the in-flight state for a single Run. The reader
// goroutine populates result, then closes done; the Run goroutine
// blocks on done. aborted is set if Run gave up early (ctx cancel)
// so the reader can drop the buffered output on the floor instead
// of holding it forever.
type pendingCall struct {
	seq     uint64
	done    chan struct{}
	result  *runResult
	aborted bool
}

// runResult is what the reader hands back to Run once it has seen
// the END marker (or EOF) for a call.
type runResult struct {
	stdout  string
	stderr  string
	exitSet bool
	exit    int
	cwdSet  bool
	cwd     string
	err     error
}

// Session is a persistent bash subprocess. Construct with
// NewSession, drive with Run, tear down with Close. A zero-value
// Session is not usable; always go through NewSession.
type Session struct {
	mu sync.Mutex

	id    string
	cmd   *exec.Cmd
	stdin io.WriteCloser

	closed  bool
	closeCh chan struct{}

	// Cached CWD — last value the CWD marker reported.
	cwd string

	// Per-call counter for BEGIN/END sequence numbers. Monotonic
	// so out-of-order or stray markers from a previous
	// (now-dead) session can't be confused with the current
	// stream.
	seq uint64

	// pending maps sequence number to in-flight call's delivery
	// channel. The reader writes to it when it sees a matching
	// BEGIN/END pair and closes done when the call is complete;
	// Run blocks on done.
	pending map[uint64]*pendingCall

	// pendingMu guards pending. It is separate from s.mu so the
	// reader goroutine can demux without contending with Run.
	pendingMu sync.Mutex

	// readerErr receives the first read error from the demuxer
	// (nil on a clean shutdown). All in-flight and future calls
	// observe it and return ErrClosed instead of hanging.
	readerErr error

	// stderrBuf accumulates all stderr output from the bash
	// subprocess. The captureStderr goroutine writes to it
	// continuously; Run reads the delta since the last call.
	//
	// LOCK ORDERING: stderrMu must NEVER be acquired while holding
	// s.mu (captureStderr holds stderrMu while writing). Acquiring
	// s.mu while holding stderrMu would create a deadlock with any
	// code path that holds s.mu and then tries to acquire stderrMu.
	stderrBuf strings.Builder
	stderrMu  sync.Mutex
	stderrPos int

	// stderrDone is closed when the captureStderr goroutine exits.
	// Close() waits on it after killing bash to ensure all stderr
	// has been drained before returning.
	stderrDone chan struct{}
}

// NewSession starts a fresh interactive bash and returns a Session
// bound to it. Bash runs with --noprofile --norc so the host user's
// shell config can't change cwd, alias common commands, or
// otherwise interfere with our command framing. -i is required so
// bash keeps a job-control state machine, which makes it flush
// stdout promptly.
//
// The session's initial cwd is the value of the HERMES_CWD env var
// if set, otherwise the calling process's current working
// directory.
func NewSession(ctx context.Context) (*Session, error) {
	id, err := randomID(8)
	if err != nil {
		return nil, fmt.Errorf("shell: generate session id: %w", err)
	}

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return nil, fmt.Errorf("shell: bash not found in PATH: %w", err)
	}

	initialCwd := os.Getenv("HERMES_CWD")
	if initialCwd == "" {
		initialCwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("shell: determine initial cwd: %w", err)
		}
	}

	cmd := exec.CommandContext(ctx, bashPath, "--noprofile", "--norc", "-i")
	cmd.Env = append(os.Environ(), "PS1=") // keep the prompt empty
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("shell: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("shell: stdout pipe: %w", err)
	}
	// stderr is captured via a separate pipe (see captureStderr goroutine).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("shell: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("shell: start bash: %w", err)
	}

	s := &Session{
		id:         id,
		cmd:        cmd,
		stdin:      stdin,
		closeCh:    make(chan struct{}),
		cwd:        initialCwd,
		pending:    make(map[uint64]*pendingCall),
		stderrDone: make(chan struct{}),
	}

	go s.captureStderr(stderr)
	go s.demux(stdout)
	return s, nil
}

// ID returns the session identifier. Useful for log correlation;
// the value is stable for the lifetime of the Session.
func (s *Session) ID() string { return s.id }

// Close terminates the underlying bash subprocess. Safe to call
// multiple times; subsequent calls are a no-op. After Close, all
// Run / GetCwd calls return ErrClosed.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *Session) closeLocked() error {
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.closeCh)

	// Wake every in-flight Run with ErrClosed before we tear
	// down bash, so they don't wait the full ctx deadline.
	s.failPending(ErrClosed)

	var firstErr error
	if s.stdin != nil {
		// Closing stdin lets bash exit cleanly on EOF
		// rather than us SIGKILLing it.
		if err := s.stdin.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		// Wait for the captureStderr goroutine to drain and
		// exit. A blocked sc.Scan() on a full pipe is freed
		// once bash is killed (which closes stderr), so this
		// should complete promptly. The timeout is a safety
		// net against kernel-level edge cases.
		waitWithTimeout(s.stderrDone, 5*time.Second)
		// Wait asynchronously; don't block Close on bash
		// cleanup.
		go func() { _ = s.cmd.Wait() }()
	}
	return firstErr
}

// failPending aborts every in-flight pendingCall. Called from
// closeLocked and from the demuxer on EOF.
func (s *Session) failPending(why error) {
	s.pendingMu.Lock()
	pending := s.pending
	s.pending = make(map[uint64]*pendingCall)
	s.pendingMu.Unlock()

	for _, p := range pending {
		if p.aborted {
			continue
		}
		p.aborted = true
		p.result = &runResult{err: why}
		close(p.done)
	}
}

// Run executes cmd in the persistent bash and returns its captured
// stdout, captured stderr, the exit code (-1 if no exit was
// observed), and any transport error. The CWD state of the session
// is updated as a side effect, so subsequent Run calls observe the
// same shell the user just typed into.
//
// The ctx bounds the *waiting* phase (waiting for the END marker),
// not the bash process itself: bash is bound to whatever context
// was passed to NewSession and lives until Close. A per-command
// timeout should be implemented by the caller wrapping the call
// site.
//
// Stderr is captured via a separate goroutine (captureStderr) that
// writes into a session-level buffer. Run reads the delta produced
// since the previous call, so stderr is scoped per-command as long
// as calls are serialized (the mutex guarantees this).
func (s *Session) Run(ctx context.Context, cmd string) (string, string, int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", "", -1, ErrClosed
	}
	s.seq++
	seq := s.seq

	frame := s.buildFrame(seq, cmd)

	// Register the pending call *before* writing to stdin, so
	// the demuxer (which can run concurrently) can never see a
	// BEGIN marker for a seq it has no record of.
	pc := &pendingCall{
		seq:  seq,
		done: make(chan struct{}),
	}
	s.pendingMu.Lock()
	s.pending[seq] = pc
	s.pendingMu.Unlock()

	// Save the stderr buffer position before this command runs,
	// so we can extract only the stderr produced by this call.
	s.stderrMu.Lock()
	fromPos := s.stderrPos
	s.stderrMu.Unlock()

	if _, err := io.WriteString(s.stdin, frame); err != nil {
		s.pendingMu.Lock()
		delete(s.pending, seq)
		s.pendingMu.Unlock()
		s.mu.Unlock()
		return "", "", -1, fmt.Errorf("shell: write command: %w", err)
	}
	s.mu.Unlock()

	// Wait for the demuxer to either complete this call or
	// signal that the session is gone.
	var res *runResult
	select {
	case <-pc.done:
		res = pc.result
	case <-ctx.Done():
		// Mark this call aborted so the demuxer, when it
		// later sees the END marker, knows to drop the
		// result on the floor rather than blocking on
		// pc.done. The demuxer will delete from s.pending
		// when it eventually sees the END, to keep the
		// bookkeeping in one place.
		s.pendingMu.Lock()
		pc.aborted = true
		s.pendingMu.Unlock()
		return "", "", -1, ctx.Err()
	case <-s.closeCh:
		return "", "", -1, ErrClosed
	}

	if res == nil {
		return "", "", -1, errors.New("shell: demuxer returned empty result")
	}
	if res.err != nil {
		return "", "", -1, res.err
	}

	// Wait for the STDBUF marker to appear in stderrBuf.
	// This ensures captureStderr has flushed all stderr from the
	// command before we read, avoiding a race where stdout signals
	// completion before the stderr pipe has finished delivering.
	stdbufMarker := fmt.Sprintf("__HERMES_STDBUF_%d__", seq)
	for {
		s.stderrMu.Lock()
		done := strings.Contains(s.stderrBuf.String(), stdbufMarker)
		s.stderrMu.Unlock()
		if done {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Read the stderr produced since fromPos.
	s.stderrMu.Lock()
	stderrContent := s.stderrBuf.String()[fromPos:]
	s.stderrPos = s.stderrBuf.Len()
	s.stderrMu.Unlock()

	// Update cached CWD from the marker.
	if res.cwdSet {
		s.mu.Lock()
		s.cwd = res.cwd
		s.mu.Unlock()
	}

	exitCode := -1
	if res.exitSet {
		exitCode = res.exit
	}
	return res.stdout, stderrContent, exitCode, nil
}

// GetCwd returns the cwd last reported by the CWD marker. Before
// the first Run completes this is the initial value passed to
// NewSession.
func (s *Session) GetCwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

// buildFrame assembles the BEGIN/END/CWD block that wraps one user
// command. The caller must hold s.mu.
func (s *Session) buildFrame(seq uint64, userCmd string) string {
	var b strings.Builder

	// BEGIN marker on its own line so the reader can scan for
	// it unambiguously even if the user's command happened to
	// print something containing "__HERMES_END_".
	fmt.Fprintf(&b, "printf '%%s\\n' '__HERMES_BEGIN_%d__'\n", seq)

	// The user's command, single-quoted so a stray apostrophe
	// in `don't` doesn't break the frame. eval re-expands it
	// as the user typed it.
	fmt.Fprintf(&b, "eval '%s'\n", escapeSingleQuotes(userCmd))

	// Capture the real exit code, then print END on its own
	// line, the EXIT trailer, and the CWD marker. We
	// deliberately use $? *after* eval so a pipeline like
	// `false | true` reports 0.
	b.WriteString("__hermes_ec=$?\n")
	// STDBUF marker: written to stderr after the user command so Run
	// can wait for stderr to drain before reading the buffer.
	fmt.Fprintf(&b, "echo '__HERMES_STDBUF_%d__' >&2\n", seq)
	fmt.Fprintf(&b, "printf '%%s\\n' '__HERMES_END_%d__'\n", seq)
	b.WriteString("printf 'EXIT %d\\n' \"$__hermes_ec\"\n")
	fmt.Fprintf(&b, "printf '%%s%%s%%s\\n' '__HERMES_CWD_%s__' \"$(pwd -P)\" '__HERMES_CWD_%s__'\n", s.id, s.id)
	return b.String()
}

// captureStderr is the single-owner reader for bash's stderr. It runs
// in its own goroutine for the lifetime of the bash process and writes
// everything bash emits on stderr into the session-level stderrBuf.
// The Run method reads the delta since the last call after each command
// completes, so callers receive stderr output scoped to their command.
//
// When the buffer exceeds maxStderrBytes, new stderr is silently dropped
// to prevent OOM. On read error or EOF, the goroutine exits and closes
// stderrDone so Close() can wait for it.
func (s *Session) captureStderr(rd io.Reader) {
	defer close(s.stderrDone)

	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		s.stderrMu.Lock()
		// Drop new output if the buffer is already at the cap.
		if s.stderrBuf.Len() >= maxStderrBytes {
			s.stderrMu.Unlock()
			continue
		}
		s.stderrBuf.WriteString(sc.Text())
		s.stderrBuf.WriteByte('\n')
		s.stderrMu.Unlock()
	}
	if err := sc.Err(); err != nil {
		// Read error on stderr is not fatal — the stdout pipe may
		// still be healthy. The goroutine exits and stderr capture
		// stops, but the session continues to service exec calls
		// using whatever stderr was captured so far.
	}
}

// demux is the single-owner reader for bash's stdout. It runs in
// its own goroutine for the lifetime of the bash process.
//
// State machine: outside a call, every line is ignored (banner,
// prompts, stray echo from rc files). When we see a BEGIN marker
// for seq N, we switch to "in call N" mode and append every
// subsequent line to N's output until we see END, EXIT, and the
// CWD marker.
//
// We use a bufio.Scanner (not a Reader.ReadString) so the buffer
// grows as needed for commands that emit large output.
func (s *Session) demux(rd io.Reader) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	var (
		cur   *pendingCall
		res   *runResult
		buf   strings.Builder
		phase = "between" // "between" or "in_call"
	)

	resetCallState := func() {
		phase = "between"
		buf.Reset()
		cur = nil
		res = nil
	}

	failAll := func(why error) {
		s.readerErr = why
		s.failPending(why)
	}

	finish := func() {
		if cur != nil {
			if res == nil {
				res = &runResult{}
			}
			res.stdout = buf.String()
			s.pendingMu.Lock()
			delete(s.pending, cur.seq)
			s.pendingMu.Unlock()
			if !cur.aborted {
				cur.result = res
				close(cur.done)
			}
		}
		resetCallState()
	}

	for sc.Scan() {
		line := sc.Text()

		if phase == "between" {
			seq, ok := parseBeginMarker(line)
			if !ok {
				continue // noise
			}
			s.pendingMu.Lock()
			pc, found := s.pending[seq]
			s.pendingMu.Unlock()
			phase = "in_call"
			res = &runResult{}
			if found {
				cur = pc
			}
			// else: known-unknown seq (caller bailed).
			// We still need to consume END/EXIT/CWD so
			// the next BEGIN parses cleanly. cur stays
			// nil.
			continue
		}

		// phase == "in_call"
		if _, ok := parseEndMarker(line); ok {
			continue
		}
		if isExitLine(line) {
			if res != nil {
				if n, ok := parseExitLine(line); ok {
					res.exit = n
					res.exitSet = true
				}
			}
			continue
		}
		if cwd, ok := parseCwdMarker(line, s.id); ok {
			if res != nil {
				res.cwd = cwd
				res.cwdSet = true
			}
			// CWD is the last line of a call's trailer;
			// hand the result back and go back to
			// between-calls mode.
			finish()
			continue
		}

		// Normal output line: append to the active call's
		// buffer. We restore the trailing newline that
		// Scanner stripped so the caller's output looks
		// like a normal command produced it.
		if cur != nil {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
		// else: known-unknown call, drop the line.
	}

	if err := sc.Err(); err != nil {
		failAll(fmt.Errorf("shell: read stdout: %w", err))
		return
	}
	// Clean EOF: bash closed its end of the pipe. Any call
	// still in-flight never got its END marker.
	failAll(io.EOF)
}

// Markers are parsed as a single regex-free token so we don't pull
// in a regex dependency for what amounts to string slicing.

const beginPrefix = "__HERMES_BEGIN_"
const endPrefix = "__HERMES_END_"
const exitPrefix = "EXIT "
const cwdOpenPrefix = "__HERMES_CWD_"

func parseBeginMarker(line string) (uint64, bool) {
	return parseSeqMarker(line, beginPrefix)
}

func parseEndMarker(line string) (uint64, bool) {
	return parseSeqMarker(line, endPrefix)
}

func parseSeqMarker(line, prefix string) (uint64, bool) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "__") {
		return 0, false
	}
	body := line[len(prefix) : len(line)-2]
	var seq uint64
	for _, r := range body {
		if r < '0' || r > '9' {
			return 0, false
		}
		seq = seq*10 + uint64(r-'0')
	}
	return seq, true
}

func isExitLine(line string) bool {
	return strings.HasPrefix(line, exitPrefix)
}

func parseExitLine(line string) (int, bool) {
	body := strings.TrimPrefix(line, exitPrefix)
	var n int
	for _, r := range body {
		if r < '0' || r > '9' {
			if r == '-' && n == 0 {
				continue
			}
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

func parseCwdMarker(line, sid string) (string, bool) {
	open := cwdOpenPrefix + sid + "__"
	if !strings.HasPrefix(line, open) {
		return "", false
	}
	rest := line[len(open):]
	close := open
	if !strings.HasSuffix(rest, close) {
		return "", false
	}
	return rest[:len(rest)-len(close)], true
}

// escapeSingleQuotes is the standard POSIX shell single-quote
// escape: replace each ' with '\” (close, escape, reopen). This
// is the only safe way to embed a single-quote inside a
// single-quoted string.
func escapeSingleQuotes(s string) string {
	// POSIX single-quote escape: replace each ' with '\'' (close quote,
	// escaped quote, reopen quote). This produces the 4-character
	// sequence: ', \, ', '.
	return strings.ReplaceAll(s, "'", `'\''`)
}

// waitWithTimeout blocks until ch is closed or the timeout elapses,
// whichever comes first. Returns true if ch was closed (the event
// completed), false if the timeout fired first.
func waitWithTimeout(ch <-chan struct{}, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// randomID returns a hex-encoded n-byte random identifier. Used
// for the session id that goes into the CWD marker.
func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
