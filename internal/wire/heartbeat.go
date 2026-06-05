// Package wire: heartbeat (Task 1.9). PROTOCOL.md §6 says every 30s
// either side may send a `ping`; if no `pong` (or any other message)
// is received within 60s, the connection is considered dead.
//
// This file is the client-side pinger. It runs as a single goroutine
// alongside the dispatch loop and exposes two pieces of state:
//
//   - A ticker that asks the dispatcher to write a `ping` envelope
//     every PingInterval (default 30s).
//   - A "last received" timestamp that the dispatcher bumps on every
//     successful read (pong counts, but so does an exec call or any
//     other server-originated frame — the spec is "any message").
//     A separate watchdog goroutine fires a `dead` callback if no
//     liveness has been marked within PongTimeout (default 60s).
//
// The pinger is intentionally a *helper*, not the owner of the
// connection. The dispatch loop still drives reads; the pinger only
// asks for periodic writes and watches for liveness silence. The
// reconnect supervisor in reconnect.go owns the connect/run/cycle
// loop and treats a "dead" signal as the trigger to drop the
// connection and back off.
package wire

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Heartbeat defaults (PROTOCOL.md §6). Zero values in the caller are
// replaced with these by the WithHeartbeat option / heartbeat_test.
const (
	// DefaultPingInterval is how often the pinger sends a ping.
	DefaultPingInterval = 30 * time.Second
	// DefaultPongTimeout is how long the watchdog waits without
	// any received message before declaring the connection dead.
	DefaultPongTimeout = 60 * time.Second
)

// ErrHeartbeatDead is sent to the OnDead channel when the watchdog
// trips. The reconnect supervisor uses this as the signal to drop
// the connection and start the backoff cycle.
var ErrHeartbeatDead = errors.New("wire: heartbeat timed out (no message in pong timeout)")

// PingerOptions configures a Pinger. The defaults (PingInterval=30s,
// PongTimeout=60s) match PROTOCOL.md §6; tests override them with
// much shorter values so the suite runs in milliseconds.
type PingerOptions struct {
	// PingInterval is the cadence at which the pinger writes
	// `ping` envelopes. Zero = DefaultPingInterval.
	PingInterval time.Duration

	// PongTimeout is the silence threshold that trips the
	// watchdog. Zero = DefaultPongTimeout. Must be >=
	// PingInterval — the watchdog would otherwise always fire
	// before the first ping completes a round trip.
	PongTimeout time.Duration
}

func (o PingerOptions) withDefaults() PingerOptions {
	if o.PingInterval <= 0 {
		o.PingInterval = DefaultPingInterval
	}
	if o.PongTimeout <= 0 {
		o.PongTimeout = DefaultPongTimeout
	}
	return o
}

// Pinger runs the heartbeat state machine for a single connection.
// One Pinger per connection; a fresh Pinger is created for each
// (re)connect because the liveness clock resets on every reconnect.
//
// Lifecycle:
//   - new Pinger  →  Start(ctx, onPing)  →  goroutines running
//   - dispatcher.MarkAlive() bumps lastRecv on every received frame
//   - watchdog fires if (now - lastRecv) > PongTimeout
//   - caller cancels ctx when the connection drops, the run loop
//     returns, both goroutines exit cleanly.
//
// The Pinger does NOT own the *websocket.Conn. The onPing callback
// is the seam: it gives the pinger one envelope to write per tick
// without entangling the heartbeat state machine with the wire
// framing. The dispatch loop wires onPing to its existing writeOne.
//
// The OnDead callback is the inverse: it is invoked exactly once,
// from the watchdog goroutine, when the liveness threshold trips.
// The reconnect supervisor wires OnDead to close the connection
// so the dispatch loop's blocked read unblocks immediately
// (otherwise the loop would sit until its own ReadTimeout fires
// — 90s in production). OnDead must be safe to call from any
// goroutine.
type Pinger struct {
	opts PingerOptions

	// mu guards lastRecv and dead. MarkAlive and the watchdog
	// both touch them; the writes are infrequent (one per
	// received frame, one per watchdog tick) so a plain Mutex
	// is fine.
	mu       sync.Mutex
	lastRecv time.Time

	// onPing is the send-side callback. The dispatch loop
	// installs a closure that writes the envelope on the
	// connection. onPing is set once at Start and never
	// changed, so it is read-only after construction.
	onPing func(env Envelope)

	// onDead is invoked exactly once when the watchdog
	// trips. The reconnect supervisor installs a closure
	// that closes the connection so the dispatch loop
	// returns. nil = no-op (the caller's choice — the
	// Dispatcher's ReadTimeout is the fallback).
	onDead func()

	// dead is set by the watchdog when the liveness timer
	// trips. IsDead reads it under mu. Once true, the
	// watchdog goroutine has exited and a new Pinger is
	// needed to resume heartbeating (typically a fresh
	// Pinger per reconnect — see reconnect.go).
	dead bool
}

// NewPinger returns a Pinger configured with opts (zero fields get
// defaults). Call Start once per connection.
func NewPinger(opts PingerOptions) *Pinger {
	return &Pinger{opts: opts.withDefaults()}
}

// MarkAlive records that one message was just received from the
// server. The watchdog timer resets relative to this call. The
// dispatcher invokes MarkAlive on every successful read — `pong`
// counts, but so does an `exec` call or any other frame (PROTOCOL.md
// §6: "no `pong` (or any other message)").
//
// MarkAlive is safe to call from any goroutine.
func (p *Pinger) MarkAlive() {
	p.mu.Lock()
	p.lastRecv = time.Now()
	p.mu.Unlock()
}

// SetOnDead installs the callback that fires when the watchdog
// trips. Must be called before Start (the watchdog reads the
// callback under its own goroutine; setting it after Start would
// race with the watchdog's first tick). Pass nil to remove a
// previously-set callback (useful in tests that want to assert
// IsDead without closing the conn).
func (p *Pinger) SetOnDead(cb func()) {
	p.mu.Lock()
	p.onDead = cb
	p.mu.Unlock()
}

// Start launches the ping ticker and the watchdog. It returns
// immediately; the two goroutines exit when ctx is cancelled. The
// onPing callback is invoked once per PingInterval with a `ping`
// envelope ready to write to the connection.
//
// onPing is invoked from a single goroutine (the ticker), so it
// does not need to be goroutine-safe. The dispatch loop's writeOne
// is not safe to call from multiple goroutines; that constraint is
// preserved by the pinger's single-goroutine design.
//
// The optional onDead callback is invoked exactly once from the
// watchdog goroutine when the liveness threshold trips. It is
// the supervisor's lever for unblocking the dispatch loop's read
// without waiting for ReadTimeout. nil is a valid onDead — the
// Dispatcher will eventually time out and the watchdog's
// IsDead() will still be true.
func (p *Pinger) Start(ctx context.Context, onPing func(env Envelope)) {
	p.onPing = onPing
	p.MarkAlive() // start the watchdog clock from "now"

	go p.runTicker(ctx)
	go p.runWatchdog(ctx)
}

// runTicker fires onPing at PingInterval until ctx is cancelled. The
// first tick is after PingInterval, not immediately — we just
// declared the connection alive, so a ping at t=0 would be noise.
func (p *Pinger) runTicker(ctx context.Context) {
	t := time.NewTicker(p.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			env := Envelope{
				Type: TypePing,
				TS:   nowRFC3339(),
				Payload: PingPayload{
					TS: nowRFC3339(),
				},
			}
			if p.onPing != nil {
				p.onPing(env)
			}
		}
	}
}

// runWatchdog checks lastRecv on a tick and trips the dead
// callback if it's older than PongTimeout. The tick adapts to
// the timeout: at the production 60s threshold, a 1s tick is
// fine and bounds detection latency to within ~1s. In tests
// with millisecond timeouts, we tick more often so a 100ms
// timeout still trips within ~125ms.
//
// The cap at 1s prevents pathological re-ticking on a 1ms
// timeout (which would otherwise run the watchdog goroutine at
// 1000Hz for no reason).
func (p *Pinger) runWatchdog(ctx context.Context) {
	tickInterval := p.opts.PongTimeout / 4
	if tickInterval > time.Second {
		tickInterval = time.Second
	}
	if tickInterval < time.Millisecond {
		tickInterval = time.Millisecond
	}
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.mu.Lock()
			last := p.lastRecv
			p.mu.Unlock()
			if time.Since(last) > p.opts.PongTimeout {
				// Fire onDead BEFORE marking dead so
				// the supervisor's close-on-dead
				// closure runs while the watchdog is
				// still the "current" goroutine. This
				// is observable if the closure logs
				// a stack — the watchdog frame is
				// more informative than a random
				// later frame.
				if p.onDead != nil {
					p.onDead()
				}
				p.markDead()
				return
			}
		}
	}
}

// IsDead reports whether the watchdog has tripped. Safe to call
// from any goroutine. Once true, it stays true for the lifetime of
// this Pinger (the watchdog goroutine has exited).
func (p *Pinger) IsDead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dead
}

func (p *Pinger) markDead() {
	p.mu.Lock()
	p.dead = true
	p.mu.Unlock()
}

// FormatDeadError returns a wrapped error suitable for the
// supervisor's "drop reason" log. It's a small helper so the
// reconnect loop doesn't have to know ErrHeartbeatDead's name.
func (p *Pinger) FormatDeadError() error {
	return fmt.Errorf("wire: %w (idle %s, threshold %s)",
		ErrHeartbeatDead, p.opts.PongTimeout, p.opts.PongTimeout)
}
