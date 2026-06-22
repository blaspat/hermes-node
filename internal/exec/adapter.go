// Package exec: adapter from *Session to whatever interface
// the wire package's ExecHandler expects. The session's Run
// merges stderr into stdout; the adapter preserves that contract.
package exec

import "context"

// SessionAdapter is the bridge *Session → wire.Executer. The
// `target` argument is accepted to match the forward-compatible
// Executer signature; on 1.4a the shell does its own cwd
// handling so target is dropped on the floor.
type SessionAdapter struct {
	S *Session
}

// NewSessionAdapter returns an adapter for the given session.
// The session must not be nil.
func NewSessionAdapter(s *Session) *SessionAdapter {
	return &SessionAdapter{S: s}
}

// Run forwards to the underlying session. See SessionAdapter
// for the target-discard rationale.
func (a *SessionAdapter) Run(ctx context.Context, _, cmd string) (string, string, int, error) {
	return a.S.Run(ctx, cmd)
}

// Cwd returns the session's current working directory. This is the
// actual working directory of the bash process as reported by the
// CWD marker after each Run completes (or the initial cwd before
// the first Run).
func (a *SessionAdapter) Cwd() string {
	return a.S.GetCwd()
}
