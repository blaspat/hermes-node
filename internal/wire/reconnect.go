// Package wire: reconnect supervisor (Task 1.9). PROTOCOL.md §6
// says a connection is dead after 60s without a message, and the
// node's recovery is "reconnect with exponential backoff (1s, 2s,
// 4s, 8s, ..., max 60s)" with every reconnect audit-logged.
//
// This file is the cycle that owns the lifetime of a node's
// connection:
//
//  1. dial (or fail-fast)
//  2. start the heartbeat (Pinger) and the dispatch loop
//  3. wait for the connection to die — either the dispatcher's
//     Run() returns an I/O error, the heartbeat watchdog trips,
//     the server sent a `bye` (clean shutdown), or the caller
//     cancels ctx
//  4. close the connection, sleep for the backoff
//  5. goto 1
//
// The supervisor is the right place for the backoff because it
// covers BOTH error paths (I/O error from dispatch AND heartbeat
// dead). The dispatch loop doesn't know about heartbeats; the
// heartbeat doesn't know about backoff. The supervisor
// orchestrates them.
//
// The supervisor is also the right place for the audit log: every
// reconnect attempt is one entry, regardless of why the previous
// connection ended. The audit entry captures the reason (I/O
// error, heartbeat dead, bye, ctx cancelled) and the attempt
// number so operators can correlate logs with server-side events.
package wire

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/blaspat/hermes-nodes/internal/audit"
)

// Backoff defaults (PROTOCOL.md §6: 1s, 2s, 4s, 8s, ..., max 60s).
const (
	// DefaultBackoffInitial is the first reconnect delay.
	DefaultBackoffInitial = 1 * time.Second
	// DefaultBackoffMax is the cap on the exponential backoff.
	DefaultBackoffMax = 60 * time.Second
	// DefaultBackoffFactor is the multiplier applied per
	// failed attempt. PROTOCOL.md §6 implies 2x.
	DefaultBackoffFactor = 2.0
)

// ReconnectAuditWriter is the subset of audit.Writer the supervisor
// depends on for reconnect entries. We use a dedicated name
// (rather than sharing the AuditWriter type in handler_exec.go)
// because reconnect audit entries have a different shape — they
// have no exit code or call duration, just an attempt number and
// a reason. Sharing the type would invite a future refactor that
// gives one of the writers a method the other can't satisfy.
//
// Nil disables audit logging — useful in tests that just want to
// assert the backoff behaviour.
type ReconnectAuditWriter interface {
	Write(e audit.Entry) error
}

// SupervisorOptions configures a Supervisor. The zero value is
// usable (it gets backoff defaults) but the caller must supply
// Dialer, Setup, and PingerOptions.
type SupervisorOptions struct {
	// Dialer dials the server and returns a post-handshake
	// Client. Production wiring calls wire.Connect; tests
	// inject a custom Dialer that points at an httptest
	// server so they can kill/restart it across reconnect
	// attempts.
	Dialer func(ctx context.Context) (*Client, error)

	// Setup registers handlers on a freshly-built Dispatcher.
	// It is called once per (re)connect with the new
	// Dispatcher and the Client that produced it, so handlers
	// that need the conn (or session id) can capture it. The
	// same handler set is expected across reconnects so the
	// server sees the same capability surface, but the
	// supervisor does not enforce that — the caller's
	// closure is the source of truth.
	//
	// Setup also receives the per-reconnect Pinger so the
	// caller can wire it to the dispatcher (OnRead = MarkAlive,
	// onPing = d.WriteOne). Splitting the wiring from the
	// supervisor keeps the supervisor free of dispatcher
	// implementation details.
	Setup func(ctx context.Context, c *Client, d *Dispatcher, p *Pinger) error

	// BackoffInitial, BackoffMax, BackoffFactor control the
	// exponential-backoff schedule. Zero values get the
	// PROTOCOL.md §6 defaults (1s → 60s, factor 2).
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	BackoffFactor  float64

	// AuditLog receives a row on every reconnect attempt.
	// nil = no audit (the caller's choice — tests pass nil
	// because they assert on a recorded slice instead).
	AuditLog ReconnectAuditWriter

	// PingerOptions is forwarded to the per-connection
	// Pinger. Zero fields get the PROTOCOL.md §6 defaults
	// (30s ping, 60s pong).
	PingerOptions PingerOptions

	// Now is the time source. Production uses time.Now;
	// tests inject a deterministic clock if needed. Optional;
	// nil = time.Now.
	Now func() time.Time
}

// withDefaults fills in zero-valued fields with the PROTOCOL.md
// defaults. We don't mutate the caller's struct in case they
// reuse it.
func (o SupervisorOptions) withDefaults() SupervisorOptions {
	if o.BackoffInitial <= 0 {
		o.BackoffInitial = DefaultBackoffInitial
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = DefaultBackoffMax
	}
	if o.BackoffFactor <= 1.0 {
		o.BackoffFactor = DefaultBackoffFactor
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Supervisor runs the connect-dispatch-heartbeat-reconnect cycle.
// One Supervisor per node binary; it lives for the lifetime of the
// process. Run() returns when ctx is cancelled (clean shutdown) or
// when the dialer returns a non-recoverable error (e.g. the
// node's config is invalid — the caller should fail fast in that
// case rather than spin forever).
type Supervisor struct {
	opts SupervisorOptions

	// attempt counts how many times we've tried to (re)connect.
	// attempt=1 is the first connect, attempt=N is the Nth
	// reconnect. The audit log includes this so operators can
	// see whether a node has been flapping.
	attempt int
}

// NewSupervisor returns a Supervisor with opts applied. Validate
// returns a non-nil error if the caller forgot Dialer or Setup
// (those are mandatory); use it before starting Run.
func NewSupervisor(opts SupervisorOptions) (*Supervisor, error) {
	if opts.Dialer == nil {
		return nil, fmt.Errorf("wire: SupervisorOptions.Dialer is required")
	}
	if opts.Setup == nil {
		return nil, fmt.Errorf("wire: SupervisorOptions.Setup is required")
	}
	return &Supervisor{opts: opts.withDefaults()}, nil
}

// Run executes the reconnect cycle. It blocks until ctx is
// cancelled or the dialer returns an error that should not be
// retried (a "fatal" error — see errFatal below). On every
// reconnect, the audit log is invoked exactly once with the
// reason the previous connection ended.
//
// The error returned is the final ctx.Err() (cancellation) or the
// fatal dial error. Successful reconnects do not return — Run
// just keeps cycling. The caller is expected to cancel ctx from a
// signal handler.
func (s *Supervisor) Run(ctx context.Context) error {
	for {
		// Honour cancellation BEFORE we dial. If the caller
		// cancelled while we were sleeping in backoff, we
		// should not start a new connection just to tear it
		// down.
		if err := ctx.Err(); err != nil {
			return err
		}

		s.attempt++
		connectErr := s.runOnce(ctx)
		if connectErr == nil {
			// runOnce only returns nil on ctx cancel.
			return ctx.Err()
		}

		// Fatal dial error → no point retrying. Audit the
		// fatal event and surface to the caller.
		if errors.Is(connectErr, errFatal) {
			s.auditReconnect(connectErr)
			return errors.Unwrap(connectErr)
		}

		// Recoverable. Audit the reconnect, sleep the
		// backoff, try again.
		s.auditReconnect(connectErr)
		if err := s.sleepBackoff(ctx); err != nil {
			return err
		}
	}
}

// errFatal is the sentinel the supervisor wraps around dial
// errors that should not be retried. Today the only such error
// is context cancellation, but the wrapper exists so future
// "fatal" cases (e.g. a config validation error) can opt out of
// the retry loop without a flag day.
var errFatal = errors.New("wire: fatal supervisor error (not retried)")

// runOnce is one iteration of the cycle: dial, install the
// heartbeat, run dispatch, return when the connection ends. The
// returned error wraps the reason:
//
//   - context.Canceled / DeadlineExceeded wrapped with errFatal —
//     the caller asked us to stop;
//   - a non-nil error from dispatch.Run — the connection died
//     (network error, heartbeat watchdog, server bye, etc.); the
//     supervisor decides whether to retry.
//   - nil — reserved for future "graceful stop" return paths;
//     today Run always returns an error on exit.
func (s *Supervisor) runOnce(ctx context.Context) error {
	c, err := s.opts.Dialer(ctx)
	if err != nil {
		// Dial failure: same retry treatment as a dropped
		// connection, but we never installed a heartbeat so
		// we don't need to tear it down. Wrap with errFatal
		// only if ctx is cancelled; otherwise return as a
		// plain retryable error.
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", errFatal, ctx.Err())
		}
		return fmt.Errorf("wire: dial attempt %d: %w", s.attempt, err)
	}

	// From here on, every exit path must close the conn.
	// We tie that off with a single defer so the bookkeeping
	// is local to this function — easier to reason about than
	// a "did we close?" flag threaded through the loop.
	closed := false
	defer func() {
		if !closed {
			_ = c.Conn().Close()
		}
	}()

	// Build a fresh Pinger for this connection. The liveness
	// clock resets on every (re)connect — we don't want the
	// watchdog to "remember" liveness from the previous
	// session.
	pinger := NewPinger(s.opts.PingerOptions)

	// Build the dispatcher and hand it to Setup, which is
	// responsible for registering handlers AND wiring the
	// pinger into the dispatcher's OnRead hook. Splitting
	// this out keeps the supervisor free of dispatcher
	// implementation details.
	d := NewDispatcher(c)
	if err := s.opts.Setup(ctx, c, d, pinger); err != nil {
		// Setup failure: surfaced as a recoverable error so
		// the supervisor's outer Run loop retries it on the
		// next backoff tick. We do NOT cap the attempt count
		// here — a misconfigured handler (e.g. a filesystem
		// blip on a slow mount) deserves a chance to recover
		// across reconnects. The same applies to dial errors.
		// The only "give up" condition is context
		// cancellation (caller asked us to stop), which the
		// Run loop handles via errFatal.
		return fmt.Errorf("wire: setup (attempt %d): %w", s.attempt, err)
	}

	// Wire the pinger's onDead to close the conn. This is
	// what unblocks the dispatch loop's blocked read when
	// the liveness threshold trips — without this, the
	// loop would sit in ReadMessage until the dispatcher's
	// own ReadTimeout (90s) fires, and the reconnect cycle
	// would be 60+ seconds per dead connection. With this,
	// the cycle is PongTimeout (60s in production, 100ms in
	// tests) plus a backoff.
	pinger.SetOnDead(func() {
		_ = c.Conn().Close()
	})

	// Start the pinger now that Setup has wired it. We pass
	// onPing as a closure that uses the dispatcher's write
	// path. The pinger goroutines exit when pingCtx is
	// cancelled.
	pingCtx, cancelPinger := context.WithCancel(ctx)
	defer cancelPinger()
	pinger.Start(pingCtx, func(env Envelope) {
		// onPing is invoked from the pinger's ticker
		// goroutine; the dispatcher's write path is
		// the only safe place to write to the conn
		// from outside the dispatch loop, so we use
		// the public WriteEnvelope entry point
		// (which serialises against the dispatch
		// loop via gorilla/websocket's internal
		// write lock). The dispatch loop's own
		// read will return the same error shortly
		// and trigger the reconnect.
		//
		// Use pingCtx, NOT the outer ctx: this
		// closure is owned by the pinger, so the
		// write should die with the pinger, not
		// outlive it. Today the two contexts share
		// the same lifecycle (outer cancellation
		// cancels pingCtx via the defer above), so
		// the difference is invisible — but if a
		// future change decouples them (e.g. the
		// pinger gets its own deadline independent
		// of the connection's lifetime), the outer
		// ctx would let a write sneak through
		// against a conn the pinger has already
		// given up on.
		if err := d.WriteEnvelope(pingCtx, env); err != nil {
			if d.OnError != nil {
				d.OnError(fmt.Errorf("wire: ping write: %w", err), env)
			}
		}
	})

	// Block on the dispatch loop. Run returns when the
	// connection ends (I/O error, ctx cancel, bye). The
	// dispatcher's OnRead is wired to MarkAlive by Setup,
	// so the pinger's liveness clock gets bumped on every
	// received frame.
	runErr := d.Run(ctx)

	// Orderly shutdown: cancel the pinger so its goroutines
	// exit, then close the conn. We set closed=true so the
	// defer doesn't double-close.
	cancelPinger()
	_ = c.Conn().Close()
	closed = true

	// ctx cancellation is "fatal" — caller asked us to stop.
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", errFatal, ctx.Err())
	}

	// Heartbeat watchdog fired. The pinger helper wraps ErrHeartbeatDead
	// with the idle-duration detail. The outer %w preserves the wrapped
	// chain so errors.Is(returnedErr, ErrHeartbeatDead) works through
	// both layers.
	if pinger.IsDead() {
		return fmt.Errorf("wire: connection lost on attempt %d: %w",
			s.attempt, pinger.FormatDeadError())
	}

	// Transport error or graceful bye. Both are retryable.
	if runErr == nil {
		// Dispatcher returned nil without ctx cancel and
		// without watchdog → server sent a clean bye.
		// Treat as retryable: re-dial and resume.
		return fmt.Errorf("wire: server bye on attempt %d", s.attempt)
	}
	return fmt.Errorf("wire: dispatch exit on attempt %d: %w", s.attempt, runErr)
}

// sleepBackoff sleeps for the backoff duration computed from the
// current attempt count. Returns ctx.Err() if ctx is cancelled
// during the sleep so the supervisor exits cleanly without
// spinning through a series of cancelled sleeps.
func (s *Supervisor) sleepBackoff(ctx context.Context) error {
	d := s.backoffFor(s.attempt)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoffFor returns the sleep duration for the n-th attempt
// (1-indexed). The schedule is attempt=1→Initial, attempt=2→2×,
// attempt=3→4×, ..., capped at BackoffMax.
//
// Factor is a float so the test rig can use 1.5× or other
// non-binary schedules. PROTOCOL.md §6 says 2× specifically, but
// the supervisor doesn't enforce that — the caller can pick
// anything > 1.0.
func (s *Supervisor) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Compute the multiplier as factor^(attempt-1). For
	// attempt=1 that's factor^0=1 (the initial delay).
	mult := 1.0
	for i := 1; i < attempt; i++ {
		mult *= s.opts.BackoffFactor
	}
	d := time.Duration(float64(s.opts.BackoffInitial) * mult)
	if d > s.opts.BackoffMax {
		d = s.opts.BackoffMax
	}
	if d < 0 {
		// Overflow guard for huge attempt counts at high
		// factor. Cap at max.
		d = s.opts.BackoffMax
	}
	return d
}

// auditReconnect writes one audit entry per reconnect attempt. The
// entry uses Action="reconnect" so the operator can grep for the
// reconnect events distinctly from "exec" / "read" / "write". The
// status is "reconnecting" (we haven't succeeded yet) — the
// audit log doesn't try to be a connection-success log; the
// supervisor's job ends at "we tried to reconnect".
func (s *Supervisor) auditReconnect(reason error) {
	if s.opts.AuditLog == nil {
		return
	}
	entry := audit.Entry{
		TS:         s.opts.Now(),
		Action:     "reconnect",
		Target:     fmt.Sprintf("attempt=%d reason=%q", s.attempt, reason.Error()),
		DurationMs: 0,
		ExitCode:   0,
		Status:     "reconnecting",
	}
	if err := s.opts.AuditLog.Write(entry); err != nil {
		// Audit failure on a reconnect is not actionable
		// (the operator already has the reason in the
		// log message), so we surface it via the standard
		// logger rather than failing the cycle. This
		// mirrors handler_exec.go's "audit failure does
		// not fail the call" stance.
		log.Printf("wire: audit reconnect entry failed: %v", err)
	}
}
