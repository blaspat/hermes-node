// Regression: a panic on a wire goroutine must cost one reconnect,
// not the process.
//
// gorilla/websocket panics ("concurrent write to websocket
// connection") instead of returning an error. Before the guards
// added alongside these tests, such a panic on the pinger's ticker
// goroutine aborted the whole node: dispatchCall's recover only
// covers handler calls on the dispatch goroutine, and recover is
// per-goroutine. Observed live as a node crash mid-transfer that
// the server could only report as "node disconnected".
package wire

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestPinger_PanicInOnPingDoesNotCrashNode asserts a panicking ping
// write marks the connection dead and fires onDead (which closes the
// conn and unwinds the dispatch loop) instead of killing the process.
func TestPinger_PanicInOnPingDoesNotCrashNode(t *testing.T) {
	p := NewPinger(PingerOptions{
		PingInterval: 5 * time.Millisecond,
		PongTimeout:  time.Hour, // keep the watchdog out of this test
	})

	var deadCalls atomic.Int32
	p.SetOnDead(func() { deadCalls.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.Start(ctx, func(Envelope) { panic("simulated concurrent write") })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.IsDead() && deadCalls.Load() > 0 {
			return // survived the panic, tripped the connection
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("panic in onPing did not trip the connection: dead=%v onDead=%d",
		p.IsDead(), deadCalls.Load())
}

// TestSupervisor_PanicInRunOnceBecomesReconnectError asserts a panic
// on the supervisor's own goroutine (here from the dialer) is turned
// into a retryable error by Run's loop rather than escaping to main
// and aborting the node.
func TestSupervisor_PanicInRunOnceBecomesReconnectError(t *testing.T) {
	var calls atomic.Int32
	sup, err := NewSupervisor(SupervisorOptions{
		Dialer: func(context.Context) (*Client, error) {
			calls.Add(1)
			panic("simulated supervisor panic")
		},
		Setup:          func(context.Context, *Client, *Dispatcher, *Pinger) error { return nil },
		BackoffInitial: 10 * time.Millisecond,
		BackoffMax:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run: got nil error, want ctx.Err()")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	if got := calls.Load(); got < 2 {
		t.Fatalf("dialer calls: got %d, want >= 2 (panics stopped the retry loop?)", got)
	}
}
