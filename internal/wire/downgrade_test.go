// Tests for the E2E downgrade guard.
//
// The client advertises E2E and expects the server to answer hello_ack
// with an ECDH pubkey. If that key is missing the client used to fall
// back to plaintext token auth, which puts the pairing token on the
// wire. Anyone on the path can strip the key and collect it. These
// tests pin the refusal.
package wire

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// serveNoECDH starts a server that answers hello_ack WITHOUT ecdh_pub
// and records whatever the client sends next.
func serveNoECDH(t *testing.T) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	next := make(chan map[string]any, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var hello map[string]any
		if err := conn.ReadJSON(&hello); err != nil {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = conn.WriteJSON(map[string]any{
			"type":             "hello_ack",
			"protocol_version": "0.1.0",
			"session_id":       "attacker-session",
			"server_time":      "2026-06-05T00:00:00.000Z",
			// ecdh_pub deliberately absent.
		})

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var follow map[string]any
		if err := conn.ReadJSON(&follow); err != nil {
			return
		}
		next <- follow
	}))
	t.Cleanup(srv.Close)
	return srv, next
}

// TestConnect_RefusesPlaintextDowngrade is the regression: a server
// that omits ecdh_pub must not receive the token. Before the guard the
// client wrote an `auth` frame carrying opts.Token verbatim.
func TestConnect_RefusesPlaintextDowngrade(t *testing.T) {
	srv, next := serveNoECDH(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Connect(ctx, DialOptions{
		ServerURL:        wsURL(srv),
		NodeName:         "victim",
		Token:            "SUPER_SECRET_TOKEN",
		NodeVersion:      "0.1.0-test",
		Platform:         "linux",
		Arch:             "amd64",
		HandshakeTimeout: testHandshakeTimeout,
	})
	if client != nil {
		_ = client.Conn().Close()
		t.Fatal("Connect: expected nil client, got a live connection")
	}
	if err == nil {
		t.Fatal("Connect: expected ErrInsecureHandshake, got nil")
	}
	if !errors.Is(err, ErrInsecureHandshake) {
		t.Fatalf("Connect error: want errors.Is(err, ErrInsecureHandshake), got %v", err)
	}

	// The decisive assertion: no auth frame reached the server.
	select {
	case got := <-next:
		t.Fatalf("client sent %v after a keyless hello_ack; it must send nothing", got)
	case <-time.After(300 * time.Millisecond):
		// Nothing sent — correct.
	}
}

// TestConnect_InsecureHandshakeOptInKeepsLegacyPath proves the escape
// hatch still works, so an operator with a genuinely old server can
// connect deliberately rather than by accident.
func TestConnect_InsecureHandshakeOptInKeepsLegacyPath(t *testing.T) {
	srv, next := serveNoECDH(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Connect(ctx, DialOptions{
		ServerURL:              wsURL(srv),
		NodeName:               "victim",
		Token:                  "test-token",
		NodeVersion:            "0.1.0-test",
		Platform:               "linux",
		Arch:                   "amd64",
		HandshakeTimeout:       testHandshakeTimeout,
		AllowInsecureHandshake: true,
	})
	if client != nil {
		_ = client.Conn().Close()
	}
	// The frame under test: the client must send auth+token only
	// because the operator opted in.
	select {
	case got := <-next:
		if got["type"] != "auth" {
			t.Errorf("follow-up frame type = %v, want auth", got["type"])
		}
		if got["token"] != "test-token" {
			t.Errorf("follow-up frame token = %v, want test-token", got["token"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opt-in did not send an auth frame")
	}

	// Connect itself fails afterwards (the fake server sends no
	// auth_ok), which is expected; we only assert the frame.
	if err == nil {
		t.Log("Connect returned no error; acceptable for the frame assertion")
	} else if errors.Is(err, ErrInsecureHandshake) {
		t.Errorf("opt-in still refused: %v", err)
	}
}

// TestConnect_ErrorMentionsToken guards the operator-facing message:
// a node that suddenly cannot connect should say why, not just
// "unexpected message".
func TestConnect_ErrorMentionsToken(t *testing.T) {
	srv, _ := serveNoECDH(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Connect(ctx, DialOptions{
		ServerURL:        wsURL(srv),
		NodeName:         "victim",
		Token:            "t",
		NodeVersion:      "0.1.0-test",
		Platform:         "linux",
		Arch:             "amd64",
		HandshakeTimeout: testHandshakeTimeout,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("error should explain the plaintext risk; got %q", err)
	}
}
