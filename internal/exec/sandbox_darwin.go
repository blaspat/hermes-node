//go:build darwin

package exec

import (
	"context"
	"os"
	"os/exec"
)

// sandboxExecPath is the macOS seatbelt launcher. It ships with every macOS
// install (deprecated by Apple but functional) and needs no entitlement.
const sandboxExecPath = "/usr/bin/sandbox-exec"

// SandboxRoots returns the write roots to hand to NewSession: the allowed
// paths, the toolchain cache defaults, then operator extras. A nil return
// means "run unsandboxed" — no allowlist configured, so there is no
// boundary to enforce in the first place.
func SandboxRoots(allowedPaths, extra []string) []string {
	if len(allowedPaths) == 0 {
		return nil
	}
	roots := make([]string, 0, len(allowedPaths)+len(extra)+10)
	roots = append(roots, allowedPaths...)
	roots = append(roots, sandboxWriteDefaults()...)
	roots = append(roots, extra...)
	return roots
}

// shellCommand builds the bash command, wrapped in sandbox-exec when roots
// is non-empty and the launcher is present. The bool reports whether the
// sandbox was actually applied.
func shellCommand(ctx context.Context, bashPath string, roots []string) (*exec.Cmd, bool) {
	plain := func() *exec.Cmd {
		return exec.CommandContext(ctx, bashPath, "--noprofile", "--norc", "-i")
	}
	if len(roots) == 0 {
		return plain(), false
	}
	if _, err := os.Stat(sandboxExecPath); err != nil {
		return plain(), false
	}
	cmd := exec.CommandContext(ctx, sandboxExecPath,
		"-p", SandboxProfile(roots),
		bashPath, "--noprofile", "--norc", "-i")
	return cmd, true
}
