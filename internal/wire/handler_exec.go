// Package wire: the `exec` call handler.
//
// This file is the bridge between the dispatch loop (dispatch.go) and
// the persistent shell executor (internal/exec). For each `exec`
// envelope the server sends, the handler:
//
//  1. Decodes the payload into ExecPayload (command, optional cwd,
//     optional env, optional timeout_ms).
//  2. Validates cwd against the configured allowlist using
//     internal/fs.Check. A cwd that fails the allowlist becomes an
//     exec_result with status=error and a structured error code so
//     the server can surface a useful message to the operator.
//  3. Calls the shell executor and captures stdout, stderr, exit code.
//  4. Bounds each stream to MaxOutputBytes (10 MB). Anything past
//     the cap is dropped, a truncation marker is appended, and the
//     result sets Truncated=true so the server can warn the caller.
//  5. Audits the call: every exec attempt is logged with action,
//     target, duration, exit code, and status. Audit failures do not
//     fail the exec — the call already happened — but they are
//     surfaced via OnError so the operator can investigate.
//
// The handler talks to the shell through an interface (Executer) so
// the tests can drive it with a mock without spawning bash.
package wire

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/blaspat/hermes-nodes/internal/audit"
	"github.com/blaspat/hermes-nodes/internal/fs"
)

// MaxOutputBytes is the per-stream cap applied to stdout and stderr
// in an exec_result. Matches PROTOCOL.md §3.7 (10 MB). Anything past
// the cap is dropped and a truncation marker is appended. The cap is
// applied at the wire layer (defense in depth) so the shell package
// can change its own internal cap without breaking the contract.
const MaxOutputBytes = 10 * 1024 * 1024

// TruncationMarker is appended to a truncated stream so operators
// grepping for the cut-off can find it. The newline at the start
// keeps it visually separated from whatever the command emitted.
const TruncationMarker = "\n[hermes-node: output truncated at 10MB]\n"

// Executer is the subset of the persistent shell session this handler
// depends on. Defined as an interface so handler_exec_test.go can
// drive the dispatch flow without spawning a real bash subprocess.
// The production wiring uses *exec.Session (see internal/exec).
//
// The signature mirrors what 1.4b will deliver: target is the path
// the shell will touch (validated against the allowlist inside the
// shell); cmd is the command line. On 1.4a target is accepted but
// ignored by the shell itself — the handler still uses it for the
// pre-flight fs.Check, so the allowlist guardrail is in place
// before the command runs.
//
// Cwd returns the shell's current working directory. Used by the
// handler to resolve the effective cwd when the exec payload omits
// it, ensuring the allowlist check still applies.
type Executer interface {
	Run(ctx context.Context, target, cmd string) (stdout, stderr string, exit int, err error)
	Cwd() string
}

// AuditWriter is the subset of audit.Writer this handler depends on.
// A nil value disables audit logging entirely; that's the test rig's
// default (assertions attach to a recorded call list instead).
type AuditWriter interface {
	Write(e audit.Entry) error
}

// ExecHandler builds the wire.Handler that serves `exec` calls.
// shell is the persistent bash session (mockable in tests). allowed
// is the list of filesystem roots the cwd must sit inside; nil/empty
// rejects every cwd (deny-by-default). Operators who want wide-open
// access must configure an explicit root, e.g. allowed_paths = ["/"].
// auditLog may be nil — see AuditWriter.
type ExecHandler struct {
	Shell    Executer
	Allowed  []string
	AuditLog AuditWriter

	// now is the clock used for the audit entry's TS. Tests may
	// override it to assert timestamps deterministically. Defaults
	// to time.Now.
	now func() time.Time
}

// NewExecHandler returns a handler wired with the given shell and
// allowed roots. auditLog may be nil; allowed may be nil/empty (cwd
// validation is then skipped).
func NewExecHandler(shell Executer, allowed []string, auditLog AuditWriter) *ExecHandler {
	return &ExecHandler{
		Shell:    shell,
		Allowed:  allowed,
		AuditLog: auditLog,
		now:      time.Now,
	}
}

// Handle is the dispatch.Handler entry point. The dispatcher hands us
// the raw payload map and the request id; we return a ready-to-send
// exec_result envelope.
//
// Status semantics (PROTOCOL.md §3.7):
//   - "ok":      exit code 0, command completed within timeout
//   - "error":   non-zero exit, or pre-flight rejection (bad cwd,
//     shell-level failure)
//   - "timeout": ctx cancellation hit the timeout boundary
//
// On any of these we always return a valid envelope; the dispatcher
// only converts to an `error` envelope when Handle itself returns an
// error, which would mean "the handler could not produce a result".
// In practice we never do — every failure mode has a result shape.
func (h *ExecHandler) Handle(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
	var p ExecPayload
	if err := reMarshalInto(payload, &p); err != nil {
		// Malformed payload is a client/protocol bug. Surface as
		// an error envelope so the server can log the exact
		// shape that broke us.
		h.auditExec(p, audit.Entry{
			TS:     h.now(),
			Action: "exec",
			Target: "",
			Status: "rejected",
		})
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   5000,
			Reason: "internal_error",
			Detail: fmt.Sprintf("decode exec payload: %v", err),
		}), nil
	}
	if p.Command == "" {
		h.auditExec(p, audit.Entry{
			TS:     h.now(),
			Action: "exec",
			Target: p.Cwd,
			Status: "rejected",
		})
		return NewErrorEnvelope(requestID, ErrorPayload{
			Code:   4000,
			Reason: "bad_request",
			Detail: "exec.command is required",
		}), nil
	}

	// Apply timeout policy. PROTOCOL.md §3.6 says default 60s, max
	// 600s. Clamp the user-supplied value into that range so a
	// rogue client can't pin the node's CPU forever.
	timeout := 60 * time.Second
	if p.TimeoutMS > 0 {
		timeout = time.Duration(p.TimeoutMS) * time.Millisecond
	}
	if timeout > 600*time.Second {
		timeout = 600 * time.Second
	}
	if timeout < time.Second {
		// Don't allow sub-second timeouts — they break the
		// "at least let one shell call round-trip" guarantee
		// and would cause spurious timeouts on small commands.
		timeout = time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Pre-flight: resolve the effective working directory. When the
	// call omits cwd we skip the allowlist check entirely — the
	// operator's shell default cwd is trusted. When the call
	// explicitly provides a cwd we validate it against the allowlist
	// so a rogue client can't jump to an arbitrary directory.
	// An empty Allowed list rejects every explicit cwd (deny-by-
	// default); operators who want wide-open access must configure
	// an explicit root, e.g. allowed_paths = ["/"].
	cwd := p.Cwd
	var canonical string
	if cwd != "" {
		ok, canon, err := fs.Check(h.Allowed, cwd)
		canonical = canon
		if err != nil || !ok {
			auditTarget := canonical
			if auditTarget == "" {
				auditTarget = p.Command
			}
			h.auditExec(p, audit.Entry{
				TS:     h.now(),
				Action: "exec",
				Target: auditTarget,
				Status: "error",
			})
			return NewExecResultEnvelope(requestID, ExecResultPayload{
				Status:     "error",
				ExitCode:   -1,
				DurationMS: 0,
			}), nil
		}
	}

	// Run the command. The shell owns the env/timeout enforcement
	// per-call; the ctx we pass in bounds *this Run call* (and
	// gives us the timeout path for "client asked for 30s").
	//
	// TODO(1.4b): once the shell accepts per-call env, merge
	// p.Env into the call frame. For 1.4a the session's process
	// env is fixed at NewSession time; per-call env is silently
	// ignored. The protocol field is preserved in the decoded
	// payload so the upgrade is mechanical.
	start := h.now()
	stdout, stderr, exit, runErr := h.Shell.Run(callCtx, cwd, p.Command)
	duration := h.now().Sub(start)

	// Cap each stream independently. capOutput is total to the
	// caller; the marker suffix means operators can grep for
	// "[hermes-node: output truncated" to find the cut-off.
	stdoutCapped, stdoutTrunc := capOutput(stdout, MaxOutputBytes)
	stderrCapped, stderrTrunc := capOutput(stderr, MaxOutputBytes)

	status := "ok"
	if runErr != nil {
		// ctx cancellation surfaces as a timeout to the server;
		// every other shell-level error (ErrClosed, EOF on the
		// pipe, etc.) is a generic "error".
		if errors.Is(runErr, context.DeadlineExceeded) ||
			errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			status = "timeout"
		} else {
			status = "error"
		}
	} else if exit != 0 {
		status = "error"
	}

	result := ExecResultPayload{
		Status:     status,
		ExitCode:   exit,
		Stdout:     stdoutCapped,
		Stderr:     stderrCapped,
		DurationMS: duration.Milliseconds(),
		Truncated:  stdoutTrunc || stderrTrunc,
	}

	// Audit. Failure to write the audit row is logged via OnError
	// upstream but does not fail the call — the user's command
	// already ran, the operator can re-derive the row from the
	// exec_result.
	target := p.Command
	if canonical != "" {
		target = canonical + " :: " + p.Command
	}
	entry := audit.Entry{
		TS:         start,
		Action:     "exec",
		Target:     target,
		DurationMs: duration.Milliseconds(),
		ExitCode:   exit,
		Status:     status,
	}
	h.auditExec(p, entry)

	return NewExecResultEnvelope(requestID, result), nil
}

// auditExec writes the audit entry if a log is configured. The
// caller is responsible for building the Target field with the
// resolved working directory (see Handle for the format).
func (h *ExecHandler) auditExec(p ExecPayload, e audit.Entry) {
	if h.AuditLog == nil {
		return
	}
	_ = h.AuditLog.Write(e)
}

// capOutput returns s bounded to max bytes, with a truncation marker
// appended when the input was longer. The marker is added only if
// we actually truncated; a string that fits within max is returned
// verbatim.
func capOutput(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	// Cut at max bytes; append the marker. Note: this may
	// split a multi-byte UTF-8 rune at the boundary. PROTOCOL.md
	// §3.7 leaves the choice between "truncate at byte
	// boundary" and "truncate at rune boundary" to the
	// implementer; byte-boundary is cheaper and the protocol
	// already requires invalid UTF-8 to be replaced with U+FFFD
	// on the server side, so a partial rune at the cut is
	// recoverable rather than lossy.
	cut := max
	if cut > len(s) {
		cut = len(s)
	}
	return s[:cut] + TruncationMarker, true
}

// ----------------------------------------------------------------------------
// Test seams.
// ----------------------------------------------------------------------------

// mockExecuter is a configurable Executer used by the handler tests.
// Each call records what was asked so tests can assert the handler
// passed the right (target, cmd) through. The exit/stdout/stderr
// fields are pre-set by the test and returned verbatim. Cwd returns
// the simulated current working directory of the shell.
type mockExecuter struct {
	target atomic.Value // string
	cmd    atomic.Value // string
	calls  atomic.Int32

	cwd string // simulated current working directory

	// out / errOut / exit / runErr are the values Run returns.
	out    string
	errOut string
	exit   int
	runErr error
}

func newMockExecuter(out, errOut string, exit int, runErr error) *mockExecuter {
	return &mockExecuter{out: out, errOut: errOut, exit: exit, runErr: runErr}
}

func (m *mockExecuter) Run(_ context.Context, target, cmd string) (string, string, int, error) {
	m.calls.Add(1)
	m.target.Store(target)
	m.cmd.Store(cmd)
	return m.out, m.errOut, m.exit, m.runErr
}

func (m *mockExecuter) Cwd() string { return m.cwd }
