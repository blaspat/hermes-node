// Package wire: reconnect supervisor (Task 1.9). PROTOCOL.md §6
// says a connection is dead after 60s without a message, and the
// node's recovery is "reconnect with exponential backoff (1s, 2s,
// 4s, 8s, ..., max 60s)" with every reconnect audit-logged.
package wire

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/blaspat/hermes-nodes/internal/audit"
)

const (
	DefaultBackoffInitial = 1 * time.Second
	DefaultBackoffMax     = 60 * time.Second
	DefaultBackoffFactor  = 2.0
)

type ReconnectAuditWriter interface {
	Write(e audit.Entry) error
}

type SupervisorOptions struct {
	Dialer func(ctx context.Context) (*Client, error)
	Setup  func(ctx context.Context, c *Client, d *Dispatcher, p *Pinger) error

	BackoffInitial time.Duration
	BackoffMax     time.Duration
	BackoffFactor  float64

	AuditLog ReconnectAuditWriter

	PingerOptions PingerOptions

	Now func() time.Time
}

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

type Supervisor struct {
	opts SupervisorOptions

	attempt int
}

func NewSupervisor(opts SupervisorOptions) (*Supervisor, error) {
	if opts.Dialer == nil {
		return nil, fmt.Errorf("wire: SupervisorOptions.Dialer is required")
	}
	if opts.Setup == nil {
		return nil, fmt.Errorf("wire: SupervisorOptions.Setup is required")
	}
	return &Supervisor{opts: opts.withDefaults()}, nil
}

func (s *Supervisor) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		s.attempt++
		connectErr := s.runOnce(ctx)
		if connectErr == nil {
			return ctx.Err()
		}

		if errors.Is(connectErr, errFatal) {
			s.auditReconnect(connectErr)
			return errors.Unwrap(connectErr)
		}

		s.auditReconnect(connectErr)
		if err := s.sleepBackoff(ctx); err != nil {
			return err
		}
	}
}

var errFatal = errors.New("wire: fatal supervisor error (not retried)")

func (s *Supervisor) runOnce(ctx context.Context) error {
	c, err := s.opts.Dialer(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", errFatal, ctx.Err())
		}
		return fmt.Errorf("wire: dial attempt %d: %w", s.attempt, err)
	}

	closed := false
	defer func() {
		if !closed {
			_ = c.Conn().Close()
		}
	}()

	pinger := NewPinger(s.opts.PingerOptions)

	d := NewDispatcher(c)
	if err := s.opts.Setup(ctx, c, d, pinger); err != nil {
		return fmt.Errorf("wire: setup (attempt %d): %w", s.attempt, err)
	}

	pinger.SetOnDead(func() {
		_ = c.Conn().Close()
	})

	pingCtx, cancelPinger := context.WithCancel(ctx)
	defer cancelPinger()
	pinger.Start(pingCtx, func(env Envelope) {
		// onPing is invoked from the pinger's ticker goroutine.
		//
		// Race with shutdown: the watchdog (onDead) or the
		// supervisor's own cancelPinger/Close sequence can
		// close the underlying conn concurrently with this
		// write. heartbeat.go's IsDead() check in runTicker
		// narrows that window but cannot close it entirely —
		// a tick can land between the check and this call.
		//
		// When that happens, WriteEnvelope returns a "use of
		// closed network connection" error (net.ErrClosed,
		// possibly wrapped by gorilla/websocket as a
		// *net.OpError). This is NOT a real dispatch failure:
		// the dispatch loop's blocked Read on the same conn is
		// about to return the identical error and drive the
		// normal reconnect path in runOnce below. Surfacing it
		// again here via OnError would produce a duplicate,
		// alarming "dispatch error" with a goroutine dump for
		// what is just a benign shutdown race.
		//
		// So: swallow close-related errors silently here, and
		// let d.Run's return value (handled below) carry the
		// real reconnect reason. Any other write error (e.g. a
		// genuine I/O error on a connection the dispatcher
		// doesn't yet know is bad) is still reported via
		// OnError as before.
		if err := d.WriteEnvelope(pingCtx, env); err != nil {
			if isClosedConnErr(err) {
				return
			}
			if d.OnError != nil {
				d.OnError(fmt.Errorf("wire: ping write: %w", err), env)
			}
		}
	})

	runErr := d.Run(ctx)

	cancelPinger()
	_ = c.Conn().Close()
	closed = true

	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", errFatal, ctx.Err())
	}

	if pinger.IsDead() {
		return fmt.Errorf("wire: connection lost on attempt %d: %w",
			s.attempt, pinger.FormatDeadError())
	}

	if runErr == nil {
		return fmt.Errorf("wire: server bye on attempt %d", s.attempt)
	}
	return fmt.Errorf("wire: dispatch exit on attempt %d: %w", s.attempt, runErr)
}

// isClosedConnErr reports whether err indicates the underlying
// connection was already closed — i.e. "use of closed network
// connection" (net.ErrClosed), possibly wrapped in a *net.OpError
// by the net/websocket layers. This is the specific, expected
// shape of the shutdown race described in the onPing closure
// above: a ping write losing the race against onDead/cancelPinger
// closing the conn.
//
// We deliberately do NOT treat other errors (timeouts, resets,
// generic I/O errors) as benign here — those are real signals that
// belong in the dispatch error path / OnError, not silently
// dropped.
func isClosedConnErr(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return errors.Is(opErr.Err, net.ErrClosed)
	}
	return false
}

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

func (s *Supervisor) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	mult := 1.0
	for i := 1; i < attempt; i++ {
		mult *= s.opts.BackoffFactor
	}
	d := time.Duration(float64(s.opts.BackoffInitial) * mult)
	if d > s.opts.BackoffMax {
		d = s.opts.BackoffMax
	}
	if d < 0 {
		d = s.opts.BackoffMax
	}
	return d
}

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
		log.Printf("wire: audit reconnect entry failed: %v", err)
	}
}
