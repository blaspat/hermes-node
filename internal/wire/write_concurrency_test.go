// Regression: concurrent post-handshake writes must not panic.
//
// gorilla/websocket allows exactly one writer at a time. Writing from
// two goroutines panics with "concurrent write to websocket
// connection", which kills the whole node process — not just the
// in-flight call. Chunked transfers made this routine: streamRead
// writes a frame per MiB from inside a handler while the pinger's
// ticker goroutine writes `ping` every 30s, so any transfer longer
// than the ping interval overlapped.
//
// Observed live before the fix: a 50 MB read over a 20 Mbit link
// panicked the node at ~26s, and the server reported
// "node disconnected mid-call". 20 MB (~12s, one ping interval)
// survived, which is why the bug only showed up on slow links and
// large files.
package wire

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestWriteE2E_ConcurrentWritersDoNotPanic spawns several goroutines
// hammering WriteE2E on one connection. Without the writeMu in
// client.go this panics; with it, every write lands intact.
func TestWriteE2E_ConcurrentWritersDoNotPanic(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	// The server side just drains frames; we only care that the
	// client does not blow up while multiple writers compete.
	received := make(chan struct{}, 256)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			select {
			case received <- struct{}{}:
			default:
			}
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL(srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	c := &Client{conn: conn, writeTimeout: 5 * time.Second}

	const writers = 8
	const perWriter = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				env := Envelope{
					Type: TypePing,
					TS:   nowRFC3339(),
					Payload: PingPayload{
						TS: nowRFC3339(),
					},
				}
				if err := c.WriteE2E(ctx, env); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}
}
