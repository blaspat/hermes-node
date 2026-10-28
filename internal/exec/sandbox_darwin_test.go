//go:build darwin

package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewSession_SandboxBlocksWritesOutsideRoots is the end-to-end check for
// the seatbelt wrapper: a real bash session under sandbox-exec must write
// inside the allowlist and fail outside it.
func TestNewSession_SandboxBlocksWritesOutsideRoots(t *testing.T) {
	allowed := t.TempDir()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	// Under $HOME but not in the defaults (only .m2/.npm/.gradle/.cache/
	// .yarn/.cargo/go are), so this is genuinely "outside".
	outsideDir := filepath.Join(home, ".hermes-node-sandbox-probe")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	defer os.RemoveAll(outsideDir)
	outside := filepath.Join(outsideDir, "should-not-exist")

	roots := SandboxRoots([]string{allowed}, nil)
	if len(roots) == 0 {
		t.Fatal("SandboxRoots returned empty on darwin")
	}

	s, err := NewSession(context.Background(), []string{allowed}, DefaultMaxOutputBytes, roots)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	inside := filepath.Join(allowed, "inside.txt")
	stdout, stderr, code, err := s.Run(context.Background(),
		"echo ok > "+inside+"; echo outside > "+outside)
	if err != nil {
		t.Fatalf("run: %v (stdout=%q stderr=%q code=%d)", err, stdout, stderr, code)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Errorf("write inside allowlist failed: %v (stdout=%q stderr=%q)", err, stdout, stderr)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Errorf("write outside allowlist SUCCEEDED — sandbox not applied (stdout=%q)", stdout)
	}
	if !strings.Contains(stdout+stderr, "not permitted") && !strings.Contains(stdout+stderr, "denied") {
		t.Logf("note: no EPERM message in output; stdout=%q stderr=%q", stdout, stderr)
	}
}
