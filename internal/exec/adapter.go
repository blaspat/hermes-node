// Package exec: adapter from *Session to whatever interface
// the wire package's ExecHandler expects. Today (1.4a) the
// session's Run is Run(ctx, cmd) -> (stdout, stderr, exit, err)
// and does not take a target path; the adapter discards target
// and forwards cmd verbatim. When 1.4b lands, the underlying
// Run signature gains a target argument and the adapter
// becomes a one-line forward.
//
// Kept in this package so the wire package does not need to
// import the concrete shell type — it depends only on the
// Executer interface declared in internal/wire.
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
