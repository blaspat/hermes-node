// Tests for the SessionAdapter (the bridge between the 1.4a
// *Session.Run(ctx, cmd) signature and the wire package's
// forward-compatible Executer interface).
package exec

import (
	"context"
	"testing"
)

// TestSessionAdapter_ForwardsCommand confirms the adapter calls
// the underlying session's Run with the cmd argument verbatim.
// We don't spawn bash here — that path is covered by the shell's
// own tests. The adapter's only job is shape compatibility.
func TestSessionAdapter_ForwardsCommand(t *testing.T) {
	// We can't easily mock a *Session without an interface
	// indirection, so we do the next-best thing: stand up a
	// real session, exercise the adapter, and assert the call
	// round-trips. If the adapter dropped cmd or messed up
	// argument order, the shell would run the wrong command.
	t.Setenv("HERMES_CWD", t.TempDir())
	s, err := NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	a := NewSessionAdapter(s)
	stdout, _, code, err := a.Run(context.Background(), "/some/target", "echo adapter-ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Errorf("Run exit: got %d, want 0", code)
	}
	if stdout != "adapter-ok\n" {
		t.Errorf("Run stdout: got %q, want %q", stdout, "adapter-ok\n")
	}
}

// TestSessionAdapter_NilSessionDocumentsPanic documents the
// current behaviour of passing a nil session to the adapter.
// The constructor doesn't check (it can't — the field is set
// after NewSession), so a misuse would panic at the first
// method call. This test exists so a future change to
// NewSessionAdapter that does validate is intentional and
// visible in the diff.
func TestSessionAdapter_NilSessionDocumentsPanic(t *testing.T) {
	a := &SessionAdapter{S: nil}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from nil session, got nil")
		}
	}()
	_, _, _, _ = a.Run(context.Background(), "", "echo x")
}
