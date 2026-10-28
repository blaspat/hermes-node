//go:build !darwin

package exec

import (
	"context"
	"os/exec"
)

// SandboxFor always returns nil off macOS: the seatbelt profile is
// darwin-only, and there is no equivalent wired up for Linux yet. Callers
// therefore get an unsandboxed shell, exactly as before.
func SandboxFor(allowedPaths, extraWrite, extraRead []string) *Sandbox {
	return nil
}

// shellCommand builds the plain bash command; sb is always nil on this
// platform (see SandboxFor).
func shellCommand(ctx context.Context, bashPath string, sb *Sandbox) (*exec.Cmd, bool) {
	return exec.CommandContext(ctx, bashPath, "--noprofile", "--norc", "-i"), false
}
