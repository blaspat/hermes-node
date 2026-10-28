//go:build !darwin

package exec

import (
	"context"
	"os/exec"
)

// SandboxRoots always returns nil off macOS: the seatbelt profile is
// darwin-only, and there is no equivalent wired up for Linux yet. Callers
// therefore get an unsandboxed shell, exactly as before.
func SandboxRoots(allowedPaths, extra []string) []string {
	return nil
}

// shellCommand builds the plain bash command; roots is always empty on this
// platform (see SandboxRoots).
func shellCommand(ctx context.Context, bashPath string, roots []string) (*exec.Cmd, bool) {
	return exec.CommandContext(ctx, bashPath, "--noprofile", "--norc", "-i"), false
}
