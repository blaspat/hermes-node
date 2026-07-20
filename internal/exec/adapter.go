// Package exec: adapter from *Session to whatever interface
// the wire package's ExecHandler expects. The session's Run
// merges stderr into stdout; the adapter preserves that contract.
package exec

import (
	"context"
	"strings"
)

// SessionAdapter is the bridge *Session → wire.Executer.
//
// The `target` argument is the validated, symlink-resolved working
// directory the caller intended the command to run in. When
// non-empty, the adapter prepends an explicit "cd <target>" to
// the command so it executes in that directory regardless of the
// bash session's current state. When empty, the command runs in
// whatever cwd the shell's previous command left behind (backward-
// compatible behaviour).
type SessionAdapter struct {
	S *Session
}

// NewSessionAdapter returns an adapter for the given session.
// The session must not be nil.
func NewSessionAdapter(s *Session) *SessionAdapter {
	return &SessionAdapter{S: s}
}

// shellQuote wraps s in single quotes, escaping any embedded
// single quotes per POSIX convention.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Run forwards to the underlying session, prepending an explicit
// `cd <target>` when target is non-empty so the command executes
// in the directory the caller asked for.
func (a *SessionAdapter) Run(ctx context.Context, target, cmd string) (string, string, int, error) {
	if target != "" {
		cmd = "cd " + shellQuote(target) + "\n" + cmd
	}
	return a.S.Run(ctx, cmd)
}

// Cwd returns the session's current working directory. This is the
// actual working directory of the bash process as reported by the
// CWD marker after each Run completes (or the initial cwd before
// the first Run).
func (a *SessionAdapter) Cwd() string {
	return a.S.GetCwd()
}
