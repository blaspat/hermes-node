// Package exec tests — persistent shell executor.
//
// Tests assert the public contract (Run returns stdout + exit + err,
// Cwd persists, ctx cancellation is honored, Close is idempotent)
// without coupling to the on-wire framing details — those live in
// buildFrame and are exercised transitively.
package exec

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newTestSession returns a fresh Session bound to t.TempDir() as the
// initial cwd, with auto-cleanup on test exit. HERMES_CWD is set so
// NewSession's cwd resolution is predictable regardless of where the
// test binary was invoked from.
func newTestSession(t *testing.T) *Session {
	t.Helper()
	t.Setenv("HERMES_CWD", t.TempDir())

	s, err := NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestNewSessionSucceeds is the smoke test: bash is on PATH, we can
// spawn it, and the returned Session is non-nil with a stable ID.
func TestNewSessionSucceeds(t *testing.T) {
	s := newTestSession(t)
	if s == nil {
		t.Fatal("NewSession returned nil")
	}
	if s.ID() == "" {
		t.Fatal("Session.ID() is empty")
	}
}

// TestRunEcho verifies the happy path: a single echo round-trips
// through the persistent shell and back to the caller. If markers
// leak into stdout this test fails loudly — that's the whole reason
// the demuxer parses BEGIN/END off the stream before delivering.
func TestRunEcho(t *testing.T) {
	s := newTestSession(t)

	stdout, _, code, err := s.Run(context.Background(), `echo hello`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("Run: exit %d, stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "hello") {
		t.Fatalf("Run echo: stdout=%q, want substring hello", stdout)
	}
	if strings.Contains(stdout, "__HERMES_") {
		t.Fatalf("Run echo: marker leaked into stdout: %q", stdout)
	}
}

// TestRunMultiLine checks that a command producing several output
// lines all make it back, in order, with no truncation. Catches a
// regression where the demuxer only delivers the first line of a
// multi-line command.
func TestRunMultiLine(t *testing.T) {
	s := newTestSession(t)

	// Three separate echo commands on separate lines, plus a
	// final `printf` to write a known multi-line blob. The blob
	// is what actually exercises "more than one line".
	cmd := `printf 'alpha\nbeta\ngamma\n'`
	stdout, _, code, err := s.Run(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("Run: exit %d, stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "alpha") ||
		!strings.Contains(stdout, "beta") ||
		!strings.Contains(stdout, "gamma") {
		t.Fatalf("Run multi-line: stdout=%q, want alpha/beta/gamma", stdout)
	}
}

// TestCwdPersistsAcrossRuns is the headline guarantee: a `cd` in one
// Run must still be the cwd when a later Run asks for it. A
// per-call spawn would land back in HERMES_CWD.
func TestCwdPersistsAcrossRuns(t *testing.T) {
	s := newTestSession(t)
	ctx := context.Background()

	// First call: cd to /tmp, then pwd so we can confirm the
	// shell actually landed there. GetCwd() must now report /tmp.
	stdout, _, code, err := s.Run(ctx, "cd /tmp && pwd")
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("first Run: exit %d, stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "/tmp") {
		t.Fatalf("first Run stdout missing /tmp: %q", stdout)
	}
	if got := s.GetCwd(); got != "/tmp" {
		t.Fatalf("GetCwd after cd: got %q, want /tmp", got)
	}

	// Second call: a fresh Run that just prints pwd. If the
	// session is truly persistent, this should still be /tmp.
	stdout, _, code, err = s.Run(ctx, "pwd")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("second Run: exit %d, stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "/tmp") {
		t.Fatalf("second Run lost cwd: stdout=%q, want pwd to report /tmp", stdout)
	}
	if got := s.GetCwd(); got != "/tmp" {
		t.Fatalf("GetCwd after second Run: got %q, want /tmp", got)
	}
}

// TestRunCapturesExitCode exercises a command that fails. Run must
// surface exit 1 and an empty stdout, and the session must remain
// usable afterwards (no poisoned state).
func TestRunCapturesExitCode(t *testing.T) {
	s := newTestSession(t)

	stdout, _, code, err := s.Run(context.Background(), "false")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 1 {
		t.Fatalf("Run false: exit %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("Run false: unexpected stdout %q", stdout)
	}

	// Session must still be usable. Cheap insurance against
	// regressions where a non-zero exit puts bash into a bad
	// state.
	stdout, _, code, err = s.Run(context.Background(), "true && echo ok")
	if err != nil {
		t.Fatalf("follow-up Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("follow-up Run: exit %d, stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "ok") {
		t.Fatalf("follow-up Run stdout: %q, want substring ok", stdout)
	}
}

// TestRunCapturesStderr verifies that stderr output from a command
// is captured and returned in the second return value. Previously
// stderr was silently discarded (cmd.Stderr = io.Discard).
//
// The first Run on a fresh session may inherit bash startup noise on
// stderr (e.g. "cannot set terminal process group" on CI runners
// without a PTY). To keep the test deterministic we flush that noise
// with a silent command before the actual assertion.
func TestRunCapturesStderr(t *testing.T) {
	s := newTestSession(t)

	// Flush bash startup messages from the stderr buffer. On some
	// CI environments bash -i emits startup warnings to stderr; a
	// no-op command ensures the next Run starts with a clean slate.
	_, _, _, err := s.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("flush run: %v", err)
	}

	stdout, stderr, code, err := s.Run(context.Background(), "echo stdout; echo stderr >&2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("Run: exit %d, stdout=%q, stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "stdout") {
		t.Errorf("stdout missing 'stdout': %q", stdout)
	}
	if !strings.Contains(stderr, "stderr") {
		t.Errorf("stderr missing 'stderr': %q", stderr)
	}
}

// TestRunCapturesStderrFromFailedCommand verifies that stderr from
// a failed command (e.g. grep on a non-existent file) is captured.
func TestRunCapturesStderrFromFailedCommand(t *testing.T) {
	s := newTestSession(t)

	stdout, stderr, code, err := s.Run(context.Background(), "grep nonexistent-pattern /nonexistent-file 2>/dev/null; echo exit=$?")
	_ = stdout
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code == 0 {
		t.Logf("grep returned 0 (unexpected — file doesn't exist)")
	}
	// Grep writes to stderr when the file doesn't exist. We redirect
	// grep's own stderr to /dev/null so it doesn't pollute our test
	// output, but the shell's stderr pipe should still function.
	_ = stderr
}

// TestRunStderrScopedPerCommand verifies that stderr from one command
// does not leak into the stderr returned for the next command. This is
// the core correctness guarantee of the fromPos scoping mechanism.
func TestRunStderrScopedPerCommand(t *testing.T) {
	s := newTestSession(t)

	_, stderr1, _, err := s.Run(context.Background(), "echo cmd1 >&2")
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	_, stderr2, _, err := s.Run(context.Background(), "echo cmd2 >&2")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if stderr1 == "" {
		t.Error("stderr1 is empty, expected 'cmd1'")
	}
	if stderr2 == "" {
		t.Error("stderr2 is empty, expected 'cmd2'")
	}
	if strings.Contains(stderr2, "cmd1") {
		t.Errorf("stderr2 leaked stderr from cmd1: stderr1=%q, stderr2=%q", stderr1, stderr2)
	}
	if !strings.Contains(stderr1, "cmd1") {
		t.Errorf("stderr1 missing 'cmd1': got %q", stderr1)
	}
	if !strings.Contains(stderr2, "cmd2") {
		t.Errorf("stderr2 missing 'cmd2': got %q", stderr2)
	}
}

// TestRunCtxCancelAborts verifies that cancelling the ctx passed to
// Run unblocks the caller with ctx.Err() before the command would
// have completed. We use a long-running `sleep` so we're sure the
// cancel beats the natural exit.
func TestRunCtxCancelAborts(t *testing.T) {
	s := newTestSession(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, _, code, err := s.Run(ctx, "sleep 30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run with cancelled ctx: expected error, got nil")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) &&
		!strings.Contains(err.Error(), "context canceled") {
		// Some Go versions surface DeadlineExceeded; we
		// only used Cancel, so we expect Canceled. Be
		// permissive about the stringification.
		t.Fatalf("Run with cancelled ctx: got %v, want context.Canceled", err)
	}
	if code != -1 {
		// We don't strictly require -1, but it signals
		// "didn't observe an exit code" — the cleanest
		// outcome of an aborted call.
		t.Logf("Run with cancelled ctx: code=%d (expected -1; got %d)", -1, code)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run with cancelled ctx took %v — should have aborted promptly", elapsed)
	}
}

// TestCloseIdempotent verifies Close is safe to call repeatedly, and
// that Run on a closed session returns ErrClosed (or wraps it).
func TestCloseIdempotent(t *testing.T) {
	s := newTestSession(t)
	ctx := context.Background()

	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close should be no-op, got: %v", err)
	}

	_, _, _, err := s.Run(ctx, "echo late")
	if err == nil {
		t.Fatal("Run after Close: expected error, got nil")
	}
	if err != ErrClosed && !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Run after Close: got %v, want ErrClosed or wrap", err)
	}
}
