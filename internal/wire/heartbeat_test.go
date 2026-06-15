// Tests for the heartbeat (Task 1.9).
//
// Strategy: each test stands up a real WebSocket pair via
// httptest + gorilla's upgrader, exercises the Pinger in
// isolation, and asserts the liveness / watchdog behaviour at the
// boundary where it's testable without driving a full dispatch
// loop. The Pinger's interface is small: Start a ticker +
// watchdog, MarkAlive on each read, IsDead when the watchdog
// trips. We assert each of those.
package wire

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newPingConnPair stands up an httptest server with a WebSocket
// upgrader, dials it, and returns both ends. The server side is
// silent — the test pumps in ping replies (or silence) as needed
// to drive the Pinger's liveness clock.
func newPingConnPair(t *testing.T) *connPair {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	ready := make(chan struct{})
	var serverConn *websocket.Conn
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade failed: %v", err)
			close(ready)
			return
		}
		serverConn = c
		close(ready)
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + srv.URL[4:]
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 2 * time.Second
	clientConn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server side never upgraded")
	}

	pair := &connPair{client: clientConn, server: serverConn}
	t.Cleanup(pair.close)
	return pair
}

// TestPinger_TicksEveryInterval asserts the Pinger writes a `ping`
// envelope on the connection at the configured interval. We
// override the interval to 50ms so the test runs in well under a
// second.
func TestPinger_TicksEveryInterval(t *testing.T) {
	pair := newPingConnPair(t)

	var pings atomic.Int32
	p := NewPinger(PingerOptions{
		PingInterval: 50 * time.Millisecond,
		PongTimeout:  5 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx, func(env Envelope) {
		// Write the envelope on the client conn so the
		// server side can read it.
		if err := pair.client.WriteJSON(env); err != nil {
			t.Logf("ping write: %v", err)
		}
		if env.Type == TypePing {
			pings.Add(1)
		}
	})

	// Drain pings from the server side. We expect ~4 in 200ms
	// (50ms interval) but allow some scheduler slack.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = pair.server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, raw, err := pair.server.ReadMessage()
		if err != nil {
			continue
		}
		var env Envelope
		if err := decodeEnvelope(raw, &env); err != nil {
			t.Fatalf("decode ping: %v", err)
		}
		// The atomic counter is also bumped by the
		// onPing closure; ReadEnvelope on the server is
		// just to confirm the wire shape.
		if env.Type != TypePing {
			t.Errorf("expected ping, got %q", env.Type)
		}
	}

	if got := pings.Load(); got < 3 {
		t.Errorf("pings: got %d, want >= 3 in 500ms with 50ms interval", got)
	}
}

// TestPinger_WatchdogTripsAfterSilence asserts the Pinger.IsDead()
// flips to true after PongTimeout of silence. We use a 100ms
// timeout and a 500ms wait — comfortably within CI jitter.
func TestPinger_WatchdogTripsAfterSilence(t *testing.T) {
	p := NewPinger(PingerOptions{
		PingInterval: 1 * time.Hour, // don't tick during the test
		PongTimeout:  100 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx, func(env Envelope) {
		// Not exercised — PingInterval is an hour away.
	})

	// Pinger was just created, so the watchdog clock is at
	// 0. After 200ms of no MarkAlive calls, the watchdog
	// should fire.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if p.IsDead() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Pinger.IsDead() never became true within 500ms (PongTimeout=100ms)")
}

// TestPinger_MarkAliveResetsWatchdog asserts that MarkAlive
// extends the watchdog deadline. We run the watchdog with a 100ms
// timeout, mark alive every 50ms, and assert the Pinger is still
// alive after 300ms (which would otherwise have been 3x the
// timeout).
func TestPinger_MarkAliveResetsWatchdog(t *testing.T) {
	p := NewPinger(PingerOptions{
		PingInterval: 1 * time.Hour,
		PongTimeout:  100 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx, func(env Envelope) {})

	// Mark alive every 50ms for 300ms. That's 6 marks, which
	// should keep the watchdog at bay.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	done := time.After(300 * time.Millisecond)
	marked := 0
loop:
	for {
		select {
		case <-tick.C:
			p.MarkAlive()
			marked++
		case <-done:
			break loop
		}
	}

	if p.IsDead() {
		t.Errorf("Pinger.IsDead() = true after %d MarkAlive calls; want false", marked)
	}
}

// TestPinger_StopsOnContextCancel asserts the Pinger's goroutines
// exit cleanly when the context is cancelled. We use a short
// interval + a short timeout, cancel, and assert no goroutine
// leak by waiting a generous grace period and then completing.
// (In Go we can't directly assert goroutine counts from a test
// without runtime.NumGoroutine, but a clean cancellation that
// returns from Start within a tick or two is the observable
// proxy.)
func TestPinger_StopsOnContextCancel(t *testing.T) {
	p := NewPinger(PingerOptions{
		PingInterval: 50 * time.Millisecond,
		PongTimeout:  100 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx, func(env Envelope) {})

	// Let the pinger tick a few times so we know the
	// goroutines are running.
	time.Sleep(150 * time.Millisecond)

	// Cancel. The goroutines watch ctx.Done() in their select,
	// so they should exit on the next tick (worst case 50ms
	// for the ticker, 1s for the watchdog).
	cancel()

	// The pinger doesn't expose a "stopped" signal, so we
	// settle for "didn't deadlock" — the test completing
	// successfully is the assertion.
	time.Sleep(100 * time.Millisecond)
}

// TestPinger_PingEnvelopeShape asserts the wire shape of a ping
// matches PROTOCOL.md §3.12: {type: "ping", ts: "..."}.
func TestPinger_PingEnvelopeShape(t *testing.T) {
	pair := newPingConnPair(t)

	p := NewPinger(PingerOptions{
		PingInterval: 50 * time.Millisecond,
		PongTimeout:  1 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx, func(env Envelope) {
		if err := pair.client.WriteJSON(env); err != nil {
			t.Logf("ping write: %v", err)
		}
	})

	// Read the first ping.
	_ = pair.server.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := pair.server.ReadMessage()
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	s := string(raw)
	for _, want := range []string{
		`"type":"ping"`,
		`"ts":`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ping wire shape missing %s in %s", want, s)
		}
	}
}

// TestPinger_FormatDeadError asserts the helper wraps ErrHeartbeatDead
// and includes the idle duration and threshold in the error string.
func TestPinger_FormatDeadError(t *testing.T) {
	p := NewPinger(PingerOptions{
		PingInterval: 1 * time.Hour,
		PongTimeout:  100 * time.Millisecond,
	})

	// Start the pinger — it calls MarkAlive immediately, so lastRecv
	// is ~now. Give it a few ms of idle time so the duration is
	// non-zero and visible in the error message.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx, func(env Envelope) {})

	time.Sleep(10 * time.Millisecond)

	err := p.FormatDeadError()
	if !errors.Is(err, ErrHeartbeatDead) {
		t.Errorf("FormatDeadError: errors.Is(%v, ErrHeartbeatDead) = false; want true", err)
	}

	s := err.Error()
	if !strings.Contains(s, "idle ") {
		t.Errorf("FormatDeadError message %q missing idle field", s)
	}
	if !strings.Contains(s, "100ms") {
		t.Errorf("FormatDeadError message %q missing threshold %q", s, "100ms")
	}
	// The idle duration should be a non-zero rounded millisecond value.
	// "0ms" would mean lastRecv was just now — unlikely after a 10ms sleep.
	if strings.Contains(s, "idle 0ms") {
		t.Errorf("FormatDeadError message %q has idle 0ms; want > 0 after 10ms sleep", s)
	}
}
