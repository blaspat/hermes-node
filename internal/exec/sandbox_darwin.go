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

// SandboxFor builds the sandbox for this machine: allowed paths plus the
// toolchain defaults, with $HOME denied except for what is listed. A nil
// return means "run unsandboxed" — no allowlist configured, so there is no
// boundary to enforce in the first place.
func SandboxFor(allowedPaths, extraWrite, extraRead []string) *Sandbox {
	if len(allowedPaths) == 0 {
		return nil
	}
	home, _ := os.UserHomeDir()

	write := make([]string, 0, len(allowedPaths)+len(extraWrite)+10)
	write = append(write, allowedPaths...)
	write = append(write, sandboxWriteDefaults()...)
	write = append(write, extraWrite...)

	read := make([]string, 0, len(extraRead)+8)
	read = append(read, sandboxReadDefaults()...)
	read = append(read, extraRead...)

	return &Sandbox{HomeDir: home, Read: read, Write: write}
}

// shellCommand builds the bash command, wrapped in sandbox-exec when sb is
// non-nil and the launcher is present. The bool reports whether the sandbox
// was actually applied.
func shellCommand(ctx context.Context, bashPath string, sb *Sandbox) (*exec.Cmd, bool) {
	plain := func() *exec.Cmd {
		return exec.CommandContext(ctx, bashPath, "--noprofile", "--norc", "-i")
	}
	if sb == nil {
		return plain(), false
	}
	if _, err := os.Stat(sandboxExecPath); err != nil {
		return plain(), false
	}
	cmd := exec.CommandContext(ctx, sandboxExecPath,
		"-p", SandboxProfile(*sb),
		bashPath, "--noprofile", "--norc", "-i")
	return cmd, true
}
