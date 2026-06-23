// Package wire: heartbeat (Task 1.9). PROTOCOL.md §6 says every 30s
// either side may send a `ping`; if no `pong` (or any other message)
// is received within 60s, the connection is considered dead.
package wire

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	DefaultPingInterval = 30 * time.Second
	DefaultPongTimeout  = 60 * time.Second
)

var ErrHeartbeatDead = errors.New("wire: heartbeat timed out (no message in pong timeout)")

type PingerOptions struct {
	PingInterval time.Duration
	PongTimeout  time.Duration
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

type Pinger struct {
	opts PingerOptions

	mu       sync.Mutex
	lastRecv time.Time

	onPing func(env Envelope)
	onDead func()
	dead   bool
}

func NewPinger(opts PingerOptions) *Pinger {
	return &Pinger{opts: opts.withDefaults()}
}

func (p *Pinger) MarkAlive() {
	p.mu.Lock()
	p.lastRecv = time.Now()
	p.mu.Unlock()
}

func (p *Pinger) SetOnDead(cb func()) {
	p.mu.Lock()
	p.onDead = cb
	p.mu.Unlock()
}

func (p *Pinger) Start(ctx context.Context, onPing func(env Envelope)) {
	p.onPing = onPing
	p.MarkAlive()

	go p.runTicker(ctx)
	go p.runWatchdog(ctx)
}

// runTicker fires onPing at PingInterval until ctx is cancelled or
// the watchdog has marked the connection dead. The IsDead() check
// guards against the race where the watchdog trips (and onDead
// closes the connection) in the gap between ctx cancellation and
// this goroutine's next tick: without the check, onPing could still
// fire once on an already-closed connection, producing a spurious
// "use of closed network connection" write error.
//
// This narrows the race to almost nothing but does not fully
// eliminate it (the watchdog could trip between the IsDead() check
// and the onPing call below). The remaining sliver is handled on
// the supervisor side by treating net.ErrClosed from a ping write
// as benign (see reconnect.go).
func (p *Pinger) runTicker(ctx context.Context) {
	t := time.NewTicker(p.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p.IsDead() {
				return
			}
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
				if p.onDead != nil {
					p.onDead()
				}
				p.markDead()
				return
			}
		}
	}
}

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

func (p *Pinger) FormatDeadError() error {
	p.mu.Lock()
	idle := time.Since(p.lastRecv)
	p.mu.Unlock()
	return fmt.Errorf("wire: %w (idle %s, threshold %s)",
		ErrHeartbeatDead, idle.Round(time.Millisecond), p.opts.PongTimeout)
}
