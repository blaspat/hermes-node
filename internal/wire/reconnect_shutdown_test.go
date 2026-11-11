package wire

// TestSupervisor_ExitsPromptlyOnCancel pins the shutdown behaviour of
// the reconnect supervisor.
//
// Cancelling the supervisor's context must stop it promptly. Until
// this test existed, it did not: the dispatch loop parks in
// conn.ReadMessage() with a read deadline of readTimeout (90s), and a
// blocked socket read ignores context cancellation. So `hermes-node
// stop` (SIGTERM, then SIGKILL after 5s) killed the daemon instead of
// letting it exit — and TestReconnect_RecoversAfterServerRestart
// flaked ~7% of runs with "supervisor did not exit after ctx cancel".
//
// The server here deliberately stays open forever: the previous probe
// was fooled by a test server that closed its own socket after ~1.7s,
// which made a hung supervisor look like a fast one.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestSupervisor_ExitsPromptlyOnCancel(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		var hello map[string]any
		if c.ReadJSON(&hello) != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{
			"type": "hello_ack", "protocol_version": "0.1.0",
			"session_id": "s", "server_time": "2026-06-05T00:00:00.000Z",
		})
		var auth map[string]any
		if c.ReadJSON(&auth) != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "auth_ok", "session_id": "s"})
		_ = c.SetReadDeadline(time.Time{}) // stay open forever
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	url := "ws" + srv.URL[4:] + "/ws/nodes"
	sup, err := NewSupervisor(SupervisorOptions{
		Dialer: func(ctx context.Context) (*Client, error) {
			return Connect(ctx, DialOptions{
				ServerURL: url, NodeName: "cancel-probe", Token: "t",
				NodeVersion: "test", Platform: "linux", Arch: "amd64",
				HandshakeTimeout: 5 * time.Second,
			})
		},
		Setup: func(_ context.Context, _ *Client, d *Dispatcher, p *Pinger) error {
			d.OnRead = p.MarkAlive
			return nil
		},
		BackoffInitial: 50 * time.Millisecond,
		BackoffMax:     200 * time.Millisecond,
		BackoffFactor:  2.0,
		// Production pinger defaults: 30s ping / 90s pong. The point of
		// this test is that shutdown does not wait for either.
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Give it time to connect and park in ReadMessage.
	time.Sleep(700 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not exit within 2s of ctx cancel; " +
			"it is blocked in ReadMessage and `hermes-node stop` would SIGKILL it")
	}
}
