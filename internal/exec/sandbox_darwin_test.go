//go:build darwin

package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewSession_SandboxBlocksOutsideAccess is the end-to-end check for the
// seatbelt wrapper: a real bash session under sandbox-exec must read and
// write inside the allowlist and fail outside it.
func TestNewSession_SandboxBlocksOutsideAccess(t *testing.T) {
	allowed := t.TempDir()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	// Under $HOME but in none of the defaults, so this is genuinely outside.
	outsideDir := filepath.Join(home, ".hermes-node-sandbox-probe")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	defer os.RemoveAll(outsideDir)

	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	sb := SandboxFor([]string{allowed}, nil, nil)
	if sb == nil {
		t.Fatal("SandboxFor returned nil on darwin with allowed paths set")
	}

	s, err := NewSession(context.Background(), []string{allowed}, DefaultMaxOutputBytes, sb)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	inside := filepath.Join(allowed, "inside.txt")
	cmd := strings.Join([]string{
		"echo ok > " + inside,
		"cat " + inside,
		"cat " + outsideFile,
		"echo pwned > " + filepath.Join(outsideDir, "written.txt"),
	}, "; ")

	stdout, stderr, code, err := s.Run(context.Background(), cmd)
	if err != nil {
		t.Fatalf("run: %v (stdout=%q stderr=%q code=%d)", err, stdout, stderr, code)
	}
	out := stdout + stderr

	// Write + read inside must work.
	if got, err := os.ReadFile(inside); err != nil || string(got) != "ok\n" {
		t.Errorf("write/read inside allowlist failed: %v %q (out=%q)", err, got, out)
	}
	// Read outside must be denied: the secret must not appear in output.
	if strings.Contains(out, "secret") {
		t.Errorf("READ OUTSIDE ALLOWLIST SUCCEEDED — secret leaked (out=%q)", out)
	}
	// Write outside must be denied.
	if _, err := os.Stat(filepath.Join(outsideDir, "written.txt")); err == nil {
		t.Errorf("WRITE OUTSIDE ALLOWLIST SUCCEEDED (out=%q)", out)
	}
	// Denials must be visible, not silent.
	if !strings.Contains(out, "not permitted") && !strings.Contains(out, "denied") {
		t.Logf("note: no EPERM message in output; out=%q", out)
	}
}

// TestNewSession_ReportsUnsandboxedWhenLauncherMissing covers the case the
// warning in runRun exists for: sandbox_exec is on, a sandbox was built, but
// the seatbelt launcher is absent — so the shell runs unrestricted and the
// session must say so rather than claim to be sandboxed.
func TestNewSession_ReportsUnsandboxedWhenLauncherMissing(t *testing.T) {
	dir := t.TempDir()

	orig := sandboxExecPath
	sandboxExecPath = filepath.Join(dir, "no-such-sandbox-exec")
	defer func() { sandboxExecPath = orig }()

	sb := SandboxFor([]string{dir}, nil, nil)
	if sb == nil {
		t.Fatal("SandboxFor returned nil with allowed paths set")
	}

	s, err := NewSession(context.Background(), []string{dir}, DefaultMaxOutputBytes, sb)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	if s.Sandboxed() {
		t.Error("launcher missing: got Sandboxed()=true, want false (this is the fail-open case)")
	}

	// And the shell must still be usable — degraded, not dead.
	stdout, _, code, err := s.Run(context.Background(), "echo alive")
	if err != nil || code != 0 || !strings.Contains(stdout, "alive") {
		t.Errorf("unsandboxed fallback shell unusable: err=%v code=%d out=%q", err, code, stdout)
	}
}
