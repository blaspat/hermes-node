// Tests for the `exec` call handler (Task 1.7).
//
// Strategy: stand up a real conn pair the same way dispatch_test.go
// does, register an ExecHandler backed by a mockExecuter, drive a
// single `exec` call from the server side, and assert:
//   - the response shape matches PROTOCOL.md §3.7
//   - the request id is echoed
//   - the handler invoked the shell with the right (target, cmd)
//   - the audit log captured the call
//   - the 10MB cap truncates long output
//
// One subtest per concern keeps failures easy to read.
package wire

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaspat/hermes-nodes/internal/audit"
	"github.com/blaspat/hermes-nodes/internal/exec"
	"github.com/blaspat/hermes-nodes/internal/fs"
)

// recordingAudit is an AuditWriter that captures every entry the
// handler writes. Tests assert against entries. We don't use
// audit.Writer directly because we want the test to be hermetic
// (no temp dirs, no fs flush) and we want to assert on the entries
// the handler constructed, not on round-trip JSON.
type recordingAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (r *recordingAudit) Write(e audit.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return nil
}

func (r *recordingAudit) snapshot() []audit.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]audit.Entry, len(r.entries))
	copy(out, r.entries)
	return out
}

// newTestExecHandler builds an ExecHandler with the given shell + a
// no-op allowed list (the cwd-allowlisted tests replace it). The
// audit log defaults to a fresh recordingAudit; pass nil to disable.
func newTestExecHandler(t *testing.T, shell Executer, allowed []string, log AuditWriter) *ExecHandler {
	t.Helper()
	if log == nil {
		// Sensible default; tests that want to assert on audit
		// pass a real recordingAudit and get it back via the
		// returned handler's AuditLog.
		log = &recordingAudit{}
	}
	return NewExecHandler(shell, allowed, log)
}

// readEnvelope decodes the next JSON message the server side of the
// pair receives into a map. Test helper.
func readEnvelope(t *testing.T, pair *connPair) map[string]any {
	t.Helper()
	return readServerJSON(t, pair.server)
}

// TestExecHandler_HappyPath drives one `exec` call whose mock
// returns stdout="hi\n", exit=0. The handler must produce a
// response of type exec_result, echo the request id, report
// status="ok", exit=0, and capture stdout verbatim. The audit log
// must contain exactly one entry.
func TestExecHandler_HappyPath(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shell := newMockExecuter("hi\n", "", 0, nil)
	shell.cwd = "/"
	rec := &recordingAudit{}
	h := newTestExecHandler(t, shell, []string{"/"}, rec)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-exec-ok",
		"type":    "exec",
		"command": "echo hi",
	})

	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result", resp["type"])
	}
	if resp["id"] != "req-exec-ok" {
		t.Errorf("response id: got %q, want req-exec-ok", resp["id"])
	}
	if resp["status"] != "ok" {
		t.Errorf("response status: got %q, want ok", resp["status"])
	}
	if code, _ := resp["exit_code"].(float64); int(code) != 0 {
		t.Errorf("response exit_code: got %v, want 0", resp["exit_code"])
	}
	if stdout, _ := resp["stdout"].(string); stdout != "hi\n" {
		t.Errorf("response stdout: got %q, want hi\\n", stdout)
	}
	if trunc, _ := resp["truncated"].(bool); trunc {
		t.Errorf("response truncated: got true, want false")
	}

	// Mock shell saw exactly one call with the right arguments.
	if got := shell.calls.Load(); got != 1 {
		t.Errorf("shell invocations: got %d, want 1", got)
	}
	if got, _ := shell.target.Load().(string); got != "/" {
		t.Errorf("shell target: got %q, want / (resolved cwd)", got)
	}
	if got, _ := shell.cmd.Load().(string); got != "echo hi" {
		t.Errorf("shell cmd: got %q, want echo hi", got)
	}

	// Audit log captured the call.
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries: got %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Action != "exec" {
		t.Errorf("audit action: got %q, want exec", e.Action)
	}
	if e.Target != "/ :: echo hi" {
		t.Errorf("audit target: got %q, want \"/ :: echo hi\"", e.Target)
	}
	if e.ExitCode != 0 {
		t.Errorf("audit exit_code: got %d, want 0", e.ExitCode)
	}
	if e.Status != "ok" {
		t.Errorf("audit status: got %q, want ok", e.Status)
	}
}

// TestExecHandler_NonZeroExit verifies that an exit-1 command from
// the shell is surfaced as status="error", exit_code=1, NOT a
// protocol-level error envelope. The dispatcher only converts to
// `error` envelopes for protocol-level problems (unknown type,
// handler returned error); an exec that "ran fine but failed" is
// still a successful round-trip.
func TestExecHandler_NonZeroExit(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shell := newMockExecuter("oops\n", "stack trace\n", 1, nil)
	shell.cwd = "/"
	h := newTestExecHandler(t, shell, []string{"/"}, nil)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-exec-fail",
		"type":    "exec",
		"command": "false",
	})

	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result", resp["type"])
	}
	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if code, _ := resp["exit_code"].(float64); int(code) != 1 {
		t.Errorf("response exit_code: got %v, want 1", resp["exit_code"])
	}
	if stdout, _ := resp["stdout"].(string); stdout != "oops\n" {
		t.Errorf("response stdout: got %q, want oops\\n", stdout)
	}
	if stderr, _ := resp["stderr"].(string); stderr != "stack trace\n" {
		t.Errorf("response stderr: got %q, want stack trace\\n", stderr)
	}
}

// TestExecHandler_ShellError covers the case where the shell
// itself reports an error (e.g. session is closed). status=error,
// exit=-1 is the protocol's "couldn't even launch" sentinel.
func TestExecHandler_ShellError(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shell := newMockExecuter("", "", -1, errors.New("shell: session is closed"))
	h := newTestExecHandler(t, shell, nil, nil)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-exec-err",
		"type":    "exec",
		"command": "anything",
	})

	resp := readEnvelope(t, pair)
	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if code, _ := resp["exit_code"].(float64); int(code) != -1 {
		t.Errorf("response exit_code: got %v, want -1 (could-not-launch)", resp["exit_code"])
	}
}

// TestExecHandler_Timeout covers the case where the call's
// per-command timeout (default 60s, max 600s) fires before the
// shell returns. The handler must surface status=timeout,
// exit=-1, and the response must still be a valid exec_result —
// not a protocol error.
func TestExecHandler_Timeout(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	// Mock shell blocks on the ctx, then returns DeadlineExceeded
	// when the handler's per-call ctx fires. We use 100ms timeout
	// so the test runs in well under a second.
	shell := &ctxBlockingMock{cwd: "/"}
	h := newTestExecHandler(t, shell, []string{"/"}, nil)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":         "req-exec-timeout",
		"type":       "exec",
		"command":    "sleep 30",
		"timeout_ms": 100,
	})

	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result", resp["type"])
	}
	if resp["status"] != "timeout" {
		t.Errorf("response status: got %q, want timeout", resp["status"])
	}
	if code, _ := resp["exit_code"].(float64); int(code) != -1 {
		t.Errorf("response exit_code: got %v, want -1", resp["exit_code"])
	}
}

// TestExecHandler_CwdAllowlistAccepted sets a temp dir as the
// only allowed root, sends an exec with cwd=that_dir, and asserts
// the shell was called (the pre-flight allowlist check passed).
func TestExecHandler_CwdAllowlistAccepted(t *testing.T) {
	dir := t.TempDir()
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shell := newMockExecuter("ok\n", "", 0, nil)
	h := newTestExecHandler(t, shell, []string{dir}, nil)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-cwd-ok",
		"type":    "exec",
		"command": "pwd",
		"cwd":     dir,
	})

	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result (allowlist must accept %q)", resp["type"], dir)
	}
	if resp["status"] != "ok" {
		t.Errorf("response status: got %q, want ok", resp["status"])
	}
	if got := shell.calls.Load(); got != 1 {
		t.Errorf("shell invocations: got %d, want 1", got)
	}
	// Cwd passed through to the shell.
	if got, _ := shell.target.Load().(string); got != dir {
		t.Errorf("shell target (cwd): got %q, want %q", got, dir)
	}
}

// TestExecHandler_CwdAllowlistRejected sets a temp dir as the only
// allowed root, sends an exec with cwd pointing *outside* that
// root, and asserts the shell was never invoked. The response is
// status=error, exit=-1. This is the security guardrail: a server
// cannot trick the node into running a command outside the
// operator-approved roots.
func TestExecHandler_CwdAllowlistRejected(t *testing.T) {
	allowedDir := t.TempDir()
	rejectedDir := t.TempDir() // outside allowedDir
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shell := newMockExecuter("", "", 0, nil)
	rec := &recordingAudit{}
	h := newTestExecHandler(t, shell, []string{allowedDir}, rec)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-cwd-bad",
		"type":    "exec",
		"command": "rm -rf /",
		"cwd":     rejectedDir,
	})

	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result (denial still gets a result, not a protocol error)", resp["type"])
	}
	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if code, _ := resp["exit_code"].(float64); int(code) != -1 {
		t.Errorf("response exit_code: got %v, want -1 (denial, no shell call)", resp["exit_code"])
	}
	// Shell must NOT have been called.
	if got := shell.calls.Load(); got != 0 {
		t.Errorf("shell invocations: got %d, want 0 (pre-flight must block the call)", got)
	}
	// Audit must have recorded the denied attempt.
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries: got %d, want 1 (the denial)", len(entries))
	}
	if entries[0].Status != "error" {
		t.Errorf("audit status: got %q, want error", entries[0].Status)
	}
}

// TestExecHandler_CwdAllowlistEmpty_RejectsAll verifies that a
// handler with no allowed list (or an empty one) rejects every
// cwd (deny-by-default). Operators who want wide-open access must
// configure an explicit root, e.g. allowed_paths = ["/"].
func TestExecHandler_CwdAllowlistEmpty_RejectsAll(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shell := newMockExecuter("pwd output\n", "", 0, nil)
	h := newTestExecHandler(t, shell, nil, nil) // nil allowed roots = reject all cwds
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-no-cwd-policy",
		"type":    "exec",
		"command": "pwd",
		"cwd":     "/some/path",
	})

	resp := readEnvelope(t, pair)
	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error (empty allowlist = reject all cwds)", resp["status"])
	}
	// Shell should never have been invoked — rejection is pre-flight.
	if got := shell.calls.Load(); got != 0 {
		t.Errorf("shell invocations: got %d, want 0 (rejected before launch)", got)
	}
}

// TestExecHandler_RejectsImplicitCwdOutsideAllowlist verifies that
// an exec with no cwd field is still checked against the allowlist
// by resolving the cwd from the shell. If the shell's current cwd
// is outside the allowlist, the call must be rejected.
func TestExecHandler_RejectsImplicitCwdOutsideAllowlist(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shell := newMockExecuter("", "", 0, nil)
	shell.cwd = "/etc"                              // shell is in /etc
	h := newTestExecHandler(t, shell, []string{"/home/user"}, nil) // allowlist = /home
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	// No cwd in payload — handler must resolve from shell.
	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-implicit-cwd-reject",
		"type":    "exec",
		"command": "pwd",
	})

	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result (denial still gets a result)", resp["type"])
	}
	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error (implicit cwd %q outside allowlist)", resp["status"], "/etc")
	}
	if code, _ := resp["exit_code"].(float64); int(code) != -1 {
		t.Errorf("response exit_code: got %v, want -1 (denial, no shell call)", resp["exit_code"])
	}
	if got := shell.calls.Load(); got != 0 {
		t.Errorf("shell invocations: got %d, want 0 (pre-flight must block the call)", got)
	}
}

// TestExecHandler_AllowsImplicitCwdInsideAllowlist verifies that
// an exec with no cwd field works when the shell's current cwd is
// inside the allowlist.
func TestExecHandler_AllowsImplicitCwdInsideAllowlist(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shell := newMockExecuter("ok\n", "", 0, nil)
	shell.cwd = "/home/user/projects"
	h := newTestExecHandler(t, shell, []string{"/home/user"}, nil)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-implicit-cwd-ok",
		"type":    "exec",
		"command": "echo ok",
	})

	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result", resp["type"])
	}
	if resp["status"] != "ok" {
		t.Errorf("response status: got %q, want ok (implicit cwd %q inside allowlist)", resp["status"], "/home/user/projects")
	}
	if got := shell.calls.Load(); got != 1 {
		t.Errorf("shell invocations: got %d, want 1", got)
	}
	// The handler must pass the resolved cwd to Run.
	if got, _ := shell.target.Load().(string); got != "/home/user/projects" {
		t.Errorf("shell target (resolved cwd): got %q, want %q", got, "/home/user/projects")
	}
}

// TestExecHandler_AuditResolvesImplicitCwd verifies that the audit
// entry's Target field contains the resolved (shell) cwd when the
// exec payload omits cwd.
func TestExecHandler_AuditResolvesImplicitCwd(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shell := newMockExecuter("ok\n", "", 0, nil)
	shell.cwd = "/var/log"
	rec := &recordingAudit{}
	h := newTestExecHandler(t, shell, []string{"/var"}, rec)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-implicit-cwd-audit",
		"type":    "exec",
		"command": "tail -n5 syslog",
	})

	resp := readEnvelope(t, pair)
	if resp["status"] != "ok" {
		t.Fatalf("response status: got %q, want ok (precondition for audit assertion)", resp["status"])
	}

	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries: got %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Action != "exec" {
		t.Errorf("audit action: got %q, want exec", e.Action)
	}
	wantTarget := "/var/log :: tail -n5 syslog"
	if e.Target != wantTarget {
		t.Errorf("audit target: got %q, want %q (resolved cwd :: command)", e.Target, wantTarget)
	}
	if e.Status != "ok" {
		t.Errorf("audit status: got %q, want ok", e.Status)
	}
}

// TestExecHandler_10MBCapStdout builds a stdout string of >10MB and
// asserts the response truncates it, sets truncated=true, and
// appends the marker. We don't go through the real wire here (a
// 10MB JSON message is wasteful); we call Handle directly.
func TestExecHandler_10MBCapStdout(t *testing.T) {
	shell := newMockExecuter(string(make([]byte, MaxOutputBytes+1024)), "", 0, nil)
	shell.cwd = "/"
	h := newTestExecHandler(t, shell, []string{"/"}, nil)

	env, err := h.Handle(context.Background(), "req-big", map[string]any{
		"command": "yes",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var result ExecResultPayload
	if err := reMarshalInto(env.Payload, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.Truncated {
		t.Errorf("Truncated: got false, want true (>10MB input)")
	}
	if len(result.Stdout) <= MaxOutputBytes {
		t.Errorf("stdout length: got %d, want > MaxOutputBytes (%d)", len(result.Stdout), MaxOutputBytes)
	}
	// Marker must be present so operators can grep for the cut-off.
	if !contains(result.Stdout, TruncationMarker) {
		t.Errorf("stdout missing truncation marker: %q", result.Stdout[len(result.Stdout)-100:])
	}
	// The cap is a per-stream bound; stderr should be empty here.
	if result.Stderr != "" {
		t.Errorf("stderr: got %q, want empty", result.Stderr)
	}
}

// TestExecHandler_10MBCapStderr does the same for stderr, so we
// know each stream is capped independently — a command that
// produces 5MB stdout + 11MB stderr should report truncated=true
// and stderr should be capped while stdout passes through.
func TestExecHandler_10MBCapStderr(t *testing.T) {
	shell := newMockExecuter(
		"small stdout",
		string(make([]byte, MaxOutputBytes+2048)),
		0, nil,
	)
	shell.cwd = "/"
	h := newTestExecHandler(t, shell, []string{"/"}, nil)

	env, err := h.Handle(context.Background(), "req-big-err", map[string]any{
		"command": "noisy",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var result ExecResultPayload
	if err := reMarshalInto(env.Payload, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.Truncated {
		t.Errorf("Truncated: got false, want true (stderr > 10MB)")
	}
	if result.Stdout != "small stdout" {
		t.Errorf("stdout: got %q, want %q (should pass through unchanged)", result.Stdout, "small stdout")
	}
	if !contains(result.Stderr, TruncationMarker) {
		t.Errorf("stderr missing truncation marker")
	}
}

// TestExecHandler_NoTruncationWhenFits checks the false-positive
// path: a command whose output is exactly at or under the cap
// must NOT set truncated=true and must NOT have the marker
// appended. Easy regression to introduce when the cap logic
// uses a "<" instead of "<=" comparison.
func TestExecHandler_NoTruncationWhenFits(t *testing.T) {
	// Exactly at the cap — should NOT be considered truncated.
	shell := newMockExecuter(string(make([]byte, MaxOutputBytes)), "", 0, nil)
	shell.cwd = "/"
	h := newTestExecHandler(t, shell, []string{"/"}, nil)

	env, err := h.Handle(context.Background(), "req-exact", map[string]any{
		"command": "noisy",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var result ExecResultPayload
	if err := reMarshalInto(env.Payload, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Truncated {
		t.Errorf("Truncated: got true, want false (input == cap)")
	}
	if contains(result.Stdout, TruncationMarker) {
		t.Errorf("stdout has marker despite fitting in cap")
	}
}

// TestExecHandler_TimeoutClamp demonstrates the policy: a 5s
// timeout gets clamped to 600s; a 0ms timeout gets clamped to
// 1s. We can't easily assert the actual ctx duration in a
// mock, so this test just asserts the call still completes and
// returns a valid result — proving the clamp logic doesn't
// break the round-trip.
func TestExecHandler_TimeoutClamp(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	shell := newMockExecuter("ok\n", "", 0, nil)
	shell.cwd = "/"
	h := newTestExecHandler(t, shell, []string{"/"}, nil)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	// 0ms — should clamp up to 1s, not 0, so the call still
	// completes and we get a valid result back.
	writeServerJSON(t, pair.server, map[string]any{
		"id":         "req-clamp-zero",
		"type":       "exec",
		"command":    "true",
		"timeout_ms": 0,
	})
	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Errorf("response type: got %q, want exec_result", resp["type"])
	}
	if resp["status"] != "ok" {
		t.Errorf("response status: got %q, want ok", resp["status"])
	}
}

// TestExecHandler_MissingCommand returns a bad_request error
// envelope — the dispatcher's `error` envelope path is for
// protocol-level issues, and an exec with no command is
// unambiguously malformed.
func TestExecHandler_MissingCommand(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	shell := newMockExecuter("", "", 0, nil)
	h := newTestExecHandler(t, shell, nil, nil)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-empty",
		"type": "exec",
	})
	resp := readEnvelope(t, pair)
	if resp["type"] != "error" {
		t.Errorf("response type: got %q, want error", resp["type"])
	}
	if resp["reason"] != "bad_request" {
		t.Errorf("response reason: got %q, want bad_request", resp["reason"])
	}
	// Shell must NOT have been called.
	if got := shell.calls.Load(); got != 0 {
		t.Errorf("shell invocations: got %d, want 0", got)
	}
}

// TestExecHandler_AuditFailureDoesNotFailCall verifies that an
// audit.Write that returns an error does not propagate to the
// caller. The command still ran; the operator can recover from a
// missing audit row by replaying the exec_result row directly.
func TestExecHandler_AuditFailureDoesNotFailCall(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	shell := newMockExecuter("ok\n", "", 0, nil)
	shell.cwd = "/"
	failingAudit := &failingAuditWriter{}
	h := newTestExecHandler(t, shell, []string{"/"}, failingAudit)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-audit-fail",
		"type":    "exec",
		"command": "true",
	})
	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result (audit failure must not change result shape)", resp["type"])
	}
	if resp["status"] != "ok" {
		t.Errorf("response status: got %q, want ok", resp["status"])
	}
}

// TestExecHandler_DurationReported sanity-checks that the
// duration_ms field is populated. The mock shell sleeps briefly
// so the handler has a measurable window to record. We assert a
// floor of 5ms — enough to prove the field is wired (not just
// zero-filled) without being so tight that it flakes on a
// slow CI box. The exact upper bound depends on the host, so
// we don't assert one.
func TestExecHandler_DurationReported(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	shell := &sleepingMock{delay: 20 * time.Millisecond, cwd: "/"}
	h := newTestExecHandler(t, shell, []string{"/"}, nil)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-dur",
		"type":    "exec",
		"command": "true",
	})
	resp := readEnvelope(t, pair)
	dur, ok := resp["duration_ms"].(float64)
	if !ok {
		t.Fatalf("duration_ms missing or wrong type: %v", resp["duration_ms"])
	}
	if dur < 5 {
		t.Errorf("duration_ms: got %v, want >= 5 (shell slept 20ms; handler must record a non-zero duration)", dur)
	}
}

// TestExecHandler_RoundTripMarshal does a full end-to-end:
// build the envelope from Handle, MarshalJSON it, decode the
// resulting bytes, and assert every field the protocol promises.
// This is the contract test — anything the server relies on must
// survive the wire encoding.
func TestExecHandler_RoundTripMarshal(t *testing.T) {
	shell := newMockExecuter("hi\n", "warn\n", 0, nil)
	shell.cwd = "/"
	h := newTestExecHandler(t, shell, []string{"/"}, nil)

	env, err := h.Handle(context.Background(), "req-marshal", map[string]any{
		"command": "echo hi",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v (raw=%s)", err, raw)
	}
	for _, field := range []string{"type", "id", "status", "exit_code", "stdout", "stderr", "duration_ms", "truncated"} {
		if _, ok := m[field]; !ok {
			t.Errorf("missing field %q in marshalled envelope: %s", field, raw)
		}
	}
}

// TestExecHandler_FsCheckIntegration confirms the handler's cwd
// validation produces the same verdict as fs.Check called
// directly. This is a guardrail against the handler bypassing
// or duplicating the allowlist logic in a way that diverges from
// the canonical check.
func TestExecHandler_FsCheckIntegration(t *testing.T) {
	dir := t.TempDir()
	// A nested subdir should also be accepted (sits inside an
	// allowed root).
	subdir := filepath.Join(dir, "nested")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	shell := newMockExecuter("", "", 0, nil)
	h := newTestExecHandler(t, shell, []string{dir}, nil)

	// Cwd outside the allowed root: handler must reject and the
	// result must be status=error, exit=-1.
	env, err := h.Handle(context.Background(), "req-int-bad", map[string]any{
		"command": "ls",
		"cwd":     "/etc/secret",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var r ExecResultPayload
	if err := reMarshalInto(env.Payload, &r); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if r.Status != "error" {
		t.Errorf("status: got %q, want error (cwd /etc/secret is outside %q)", r.Status, dir)
	}

	// fs.Check must agree: /etc/secret is NOT under dir.
	if ok, _, err := fs.Check([]string{dir}, "/etc/secret"); err == nil && ok {
		t.Errorf("fs.Check gave the wrong verdict (allowed /etc/secret under %q)", dir)
	}

	// Cwd inside the allowed root (the subdir): handler accepts
	// and the shell is called.
	shell2 := newMockExecuter("ok\n", "", 0, nil)
	h2 := newTestExecHandler(t, shell2, []string{dir}, nil)
	env2, err := h2.Handle(context.Background(), "req-int-good", map[string]any{
		"command": "ls",
		"cwd":     subdir,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var r2 ExecResultPayload
	_ = reMarshalInto(env2.Payload, &r2)
	if r2.Status != "ok" {
		t.Errorf("status: got %q, want ok (cwd %q is inside %q)", r2.Status, subdir, dir)
	}
	if got := shell2.calls.Load(); got != 1 {
		t.Errorf("shell invocations: got %d, want 1", got)
	}
}

// ----------------------------------------------------------------------------
// End-to-end: dispatch loop + real shell + real audit log.
// ----------------------------------------------------------------------------

// TestExecHandler_EndToEnd stands up a real conn pair, registers
// the ExecHandler backed by a real *exec.Session and a real
// audit.Writer, drives an `exec` call from the server side, and
// asserts that the dispatch loop, the shell, and the audit log
// all wire together correctly. This is the integration test the
// Task 1.7 acceptance criteria call for.
func TestExecHandler_EndToEnd(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	// Real shell session.
	shellCwd := t.TempDir()
	t.Setenv("HERMES_CWD", shellCwd)
	shell, err := exec.NewSession(context.Background())
	if err != nil {
		t.Fatalf("exec.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = shell.Close() })

	// Real audit writer pointing at a temp file.
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	auditLog, err := audit.New(auditPath)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })

	h := NewExecHandler(exec.NewSessionAdapter(shell), []string{shellCwd}, auditLog)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	// Drive a real command — the dispatch loop reads it,
	// the handler calls the shell, the shell runs echo,
	// the handler reports back, the audit log captures the
	// call. End-to-end.
	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-e2e-1",
		"type":    "exec",
		"command": "echo hermes-end-to-end",
	})

	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result", resp["type"])
	}
	if resp["status"] != "ok" {
		t.Errorf("response status: got %q, want ok (resp=%v)", resp["status"], resp)
	}
	if stdout, _ := resp["stdout"].(string); !contains(stdout, "hermes-end-to-end") {
		t.Errorf("response stdout: got %q, want substring hermes-end-to-end", stdout)
	}

	// Audit log must contain the row, with the real command as
	// the target. The audit writer Syncs after every Write, so
	// the file is durable by the time Write returns — we don't
	// need to sleep before reading.
	contents, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !contains(string(contents), `"action":"exec"`) {
		t.Errorf("audit log missing action=exec: %s", contents)
	}
	if !contains(string(contents), "echo hermes-end-to-end") {
		t.Errorf("audit log missing command target: %s", contents)
	}
	if !contains(string(contents), `"status":"ok"`) {
		t.Errorf("audit log missing status=ok: %s", contents)
	}
}

// TestExecHandler_EndToEnd_NonZeroExit does the same end-to-end
// wiring but with a command that exits non-zero, to confirm the
// status=error / exit=1 path survives the full round-trip.
func TestExecHandler_EndToEnd_NonZeroExit(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	shellCwd := t.TempDir()
	t.Setenv("HERMES_CWD", shellCwd)
	shell, err := exec.NewSession(context.Background())
	if err != nil {
		t.Fatalf("exec.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = shell.Close() })

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	auditLog, err := audit.New(auditPath)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })

	h := NewExecHandler(exec.NewSessionAdapter(shell), []string{shellCwd}, auditLog)
	if err := d.Register(TypeExec, h.Handle); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-e2e-fail",
		"type":    "exec",
		"command": "false",
	})

	resp := readEnvelope(t, pair)
	if resp["type"] != "exec_result" {
		t.Fatalf("response type: got %q, want exec_result", resp["type"])
	}
	if resp["status"] != "error" {
		t.Errorf("response status: got %q, want error", resp["status"])
	}
	if code, _ := resp["exit_code"].(float64); int(code) != 1 {
		t.Errorf("response exit_code: got %v, want 1", resp["exit_code"])
	}
}

// ----------------------------------------------------------------------------
// Test helpers.
// ----------------------------------------------------------------------------

// contains is the standard-library-free substring check.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// ctxBlockingMock is an Executer that blocks until the ctx is
// cancelled, then returns the ctx error. Used by the timeout test
// to exercise the handler's deadline-exceeded branch.
type ctxBlockingMock struct {
	calls atomic.Int32
	cwd   string
}

func (c *ctxBlockingMock) Run(ctx context.Context, _, _ string) (string, string, int, error) {
	c.calls.Add(1)
	<-ctx.Done()
	return "", "", -1, ctx.Err()
}

func (c *ctxBlockingMock) Cwd() string { return c.cwd }

// failingAuditWriter is an AuditWriter that always errors. Used
// by the "audit failure does not fail the call" test.
type failingAuditWriter struct{}

func (failingAuditWriter) Write(audit.Entry) error {
	return errors.New("simulated disk full")
}

// sleepingMock is an Executer that sleeps for `delay` before
// returning a successful response. Used by DurationReported to
// confirm the handler records a non-zero duration even when the
// shell returns quickly.
type sleepingMock struct {
	delay time.Duration
	cwd   string
}

func (s *sleepingMock) Run(ctx context.Context, _, _ string) (string, string, int, error) {
	select {
	case <-time.After(s.delay):
		return "ok\n", "", 0, nil
	case <-ctx.Done():
		return "", "", -1, ctx.Err()
	}
}

func (s *sleepingMock) Cwd() string { return s.cwd }
