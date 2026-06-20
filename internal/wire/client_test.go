// Tests for the WSS client + auth handshake (Task 1.5).
//
// Strategy: stand up an httptest.NewServer with a real WebSocket
// upgrade handler. The handler implements a tiny state machine
// driven by the test scenario function: a happy-path server
// responds hello_ack \u2192 auth_ok, an auth-rejecting server sends
// auth_err and closes the connection, a protocol-mismatch server
// sends hello_err. The test asserts the client's view of the
// handshake in each case.
//
// We use plain `ws://` (not wss://) against httptest because
// httptest.NewServer is HTTP, not HTTPS. Production connects to
// wss://; the dial path is the same and is exercised by the
// integration test in Task 2.4.
package wire

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testScenario describes how a mock server should respond during
// the handshake. The handler walks this table per-connection.
type testScenario struct {
	// authOK is true if the server should reply auth_ok on a
	// well-formed auth message, false if it should reply
	// auth_err.
	authOK bool
	// authErrReason is included in the auth_err payload.
	authErrReason string
	// rejectProtocol, if true, makes the server reply
	// hello_err with reason "unsupported_protocol_version".
	rejectProtocol bool
	// handshakeTimeout is the per-call deadline the handler
	// uses for read/write. Tests pass a short value so the
	// suite runs fast.
	handshakeTimeout time.Duration
}

const (
	// Handshake timeout used by both client and server in
	// tests. Short so a misbehaving client doesn't pin the
	// suite, long enough to ride out CI jitter.
	testHandshakeTimeout = 2 * time.Second
)

// serveScenario starts an httptest.Server that performs the
// WebSocket upgrade and then runs the scenario's state machine
// against the resulting connection. The server returns the
// httptest.Server so the caller can pluck its URL (and
// Close() it via t.Cleanup).
func serveScenario(t *testing.T, sc testScenario) *httptest.Server {
	t.Helper()
	if sc.handshakeTimeout == 0 {
		sc.handshakeTimeout = testHandshakeTimeout
	}

	upgrader := websocket.Upgrader{
		// We're talking to a localhost test server; trust any
		// origin. Production would have a real one.
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		// 1. Read hello.
		_ = conn.SetReadDeadline(time.Now().Add(sc.handshakeTimeout))
		var hello map[string]any
		if err := conn.ReadJSON(&hello); err != nil {
			t.Logf("server: read hello: %v", err)
			return
		}
		if hello["type"] != "hello" {
			t.Logf("server: expected hello, got %v", hello["type"])
			return
		}

		// 2. Reply hello_ack or hello_err.
		if sc.rejectProtocol {
			_ = conn.SetWriteDeadline(time.Now().Add(sc.handshakeTimeout))
			_ = conn.WriteJSON(map[string]any{
				"type":               "hello_err",
				"reason":             "unsupported_protocol_version",
				"code":               4002,
				"server_max_version": "0.0.9",
			})
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(sc.handshakeTimeout))
		_ = conn.WriteJSON(map[string]any{
			"type":             "hello_ack",
			"protocol_version": "0.1.0",
			"session_id":       "test-session-abc",
			"server_time":      "2026-06-05T00:00:00.000Z",
		})

		// 3. Read auth.
		_ = conn.SetReadDeadline(time.Now().Add(sc.handshakeTimeout))
		var auth map[string]any
		if err := conn.ReadJSON(&auth); err != nil {
			t.Logf("server: read auth: %v", err)
			return
		}
		if auth["type"] != "auth" {
			t.Logf("server: expected auth, got %v", auth["type"])
			return
		}

		// 4. Reply auth_ok or auth_err.
		_ = conn.SetWriteDeadline(time.Now().Add(sc.handshakeTimeout))
		if sc.authOK {
			_ = conn.WriteJSON(map[string]any{
				"type":       "auth_ok",
				"session_id": "test-session-abc",
			})
		} else {
			_ = conn.WriteJSON(map[string]any{
				"type":   "auth_err",
				"reason": sc.authErrReason,
				"code":   4001,
			})
			// PROTOCOL.md \u00a73.5: server closes with
			// 4001 after sending auth_err. Use a
			// CloseMessage so the client sees the
			// proper close code if it reads.
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(4001, "auth failed"))
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// wsURL converts an httptest server's http:// URL into the
// equivalent ws:// URL for the gorilla dialer.
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestConnect_HappyPath exercises the full hello \u2192 hello_ack \u2192
// auth \u2192 auth_ok exchange and asserts the returned Client has
// the expected session id and node name.
func TestConnect_HappyPath(t *testing.T) {
	srv := serveScenario(t, testScenario{authOK: true})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Connect(ctx, DialOptions{
		ServerURL:        wsURL(srv),
		NodeName:         "test-node",
		Token:            "test-token",
		NodeVersion:      "0.1.0-test",
		Platform:         "linux",
		Arch:             "amd64",
		Capabilities:     []string{"exec", "read", "write"},
		HandshakeTimeout: testHandshakeTimeout,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Conn().Close() })

	if got := client.SessionID(); got != "test-session-abc" {
		t.Errorf("SessionID: got %q, want %q", got, "test-session-abc")
	}
	if got := client.NodeName(); got != "test-node" {
		t.Errorf("NodeName: got %q, want %q", got, "test-node")
	}
	if client.Conn() == nil {
		t.Errorf("Conn() is nil after successful handshake")
	}
}

// TestConnect_AuthRejected asserts that when the server replies
// auth_err, Connect returns a non-nil error wrapping
// ErrAuthFailed and carries the server's reason.
func TestConnect_AuthRejected(t *testing.T) {
	srv := serveScenario(t, testScenario{
		authOK:        false,
		authErrReason: "invalid_token",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Connect(ctx, DialOptions{
		ServerURL:        wsURL(srv),
		NodeName:         "test-node",
		Token:            "WRONG",
		NodeVersion:      "0.1.0-test",
		Platform:         "linux",
		Arch:             "amd64",
		Capabilities:     []string{"exec", "read", "write"},
		HandshakeTimeout: testHandshakeTimeout,
	})
	if err == nil {
		_ = client.Conn().Close()
		t.Fatal("Connect: expected error on auth_err, got nil")
	}
	if client != nil {
		t.Errorf("Connect: expected nil client on auth_err, got %+v", client)
	}
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("Connect error: want errors.Is(err, ErrAuthFailed), got %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_token") {
		t.Errorf("Connect error: want reason %q in error, got %v", "invalid_token", err)
	}
}

// TestConnect_ProtocolMismatch asserts the client surfaces a
// hello_err as ErrProtocolMismatch with the server's reason and
// advertised max version.
func TestConnect_ProtocolMismatch(t *testing.T) {
	srv := serveScenario(t, testScenario{
		rejectProtocol: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Connect(ctx, DialOptions{
		ServerURL:        wsURL(srv),
		NodeName:         "test-node",
		Token:            "test-token",
		NodeVersion:      "0.1.0-test",
		Platform:         "linux",
		Arch:             "amd64",
		Capabilities:     []string{"exec", "read", "write"},
		HandshakeTimeout: testHandshakeTimeout,
	})
	if err == nil {
		_ = client.Conn().Close()
		t.Fatal("Connect: expected error on hello_err, got nil")
	}
	if client != nil {
		t.Errorf("Connect: expected nil client on hello_err, got %+v", client)
	}
	if !errors.Is(err, ErrProtocolMismatch) {
		t.Errorf("Connect error: want errors.Is(err, ErrProtocolMismatch), got %v", err)
	}
	if !strings.Contains(err.Error(), "unsupported_protocol_version") {
		t.Errorf("Connect error: want reason %q, got %v", "unsupported_protocol_version", err)
	}
	if !strings.Contains(err.Error(), "0.0.9") {
		t.Errorf("Connect error: want server_max_version %q, got %v", "0.0.9", err)
	}
}

// TestEnvelope_RoundTrip exercises messages.go's marshal path: a
// built envelope should decode back into a payload with the
// expected fields at the top level (PROTOCOL.md \u00a72 duck-typed
// envelope). This guards the on-wire shape independently of the
// connection layer.
func TestEnvelope_RoundTrip(t *testing.T) {
	env := NewHelloEnvelope("0.1.0", "work-laptop", "0.1.0", "darwin", "arm64",
		[]string{"exec", "read", "write"})

	// We can't use Envelope.MarshalJSON directly (it returns
	// []byte, error); do the round trip via the codec path.
	raw, err := codecMarshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Spot-check the wire shape matches PROTOCOL.md \u00a73.1.
	for _, want := range []string{
		`"type":"hello"`,
		`"protocol_version":"0.1.0"`,
		`"node_name":"work-laptop"`,
		`"node_version":"0.1.0"`,
		`"platform":"darwin"`,
		`"arch":"arm64"`,
		`"capabilities":["exec","read","write"]`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("wire shape missing %s in %s", want, string(raw))
		}
	}

	// And the round trip: decode back into Envelope, then
	// into HelloPayload, and assert fields survived.
	var env2 Envelope
	if err := decodeEnvelope(raw, &env2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env2.Type != TypeHello {
		t.Errorf("decoded type: got %q, want hello", env2.Type)
	}
	var hp HelloPayload
	if err := reMarshalInto(env2.Payload, &hp); err != nil {
		t.Fatalf("re-decode payload: %v", err)
	}
	if hp.NodeName != "work-laptop" {
		t.Errorf("payload NodeName: got %q", hp.NodeName)
	}
	if hp.Platform != "darwin" {
		t.Errorf("payload Platform: got %q", hp.Platform)
	}
	if len(hp.Capabilities) != 3 || hp.Capabilities[0] != "exec" {
		t.Errorf("payload Capabilities: got %v", hp.Capabilities)
	}
}

// codecMarshal is a thin wrapper that calls the package-private
// Envelope.MarshalJSON. Tests live in the same package so they can
// reach unexported names; this helper keeps the call site readable.
func codecMarshal(e Envelope) ([]byte, error) {
	return e.MarshalJSON()
}

func TestNormaliseServerURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantPath string
	}{
		{
			name:     "bare host:port — no path",
			input:    "wss://vps.example.com:7000",
			wantPath: "/ws/nodes",
		},
		{
			name:     "bare host:port with slash only",
			input:    "wss://vps.example.com:7000/",
			wantPath: "/ws/nodes",
		},
		{
			name:     "already has /ws/nodes",
			input:    "wss://vps.example.com:7000/ws/nodes",
			wantPath: "/ws/nodes",
		},
		{
			name:     "has sub-path",
			input:    "wss://vps.example.com:7000/some/path",
			wantPath: "/some/path",
		},
		{
			name:     "invalid URL — Go parses it as relative; no path normalised",
			input:    "not-a-url",
			wantPath: "not-a-url",
		},
		{
			name:     "ws scheme",
			input:    "ws://127.0.0.1:7000",
			wantPath: "/ws/nodes",
		},
		{
			name:     "localhost",
			input:    "wss://localhost:7000",
			wantPath: "/ws/nodes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normaliseServerURL(tc.input)
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", got, err)
			}
			if u.Path != tc.wantPath {
				t.Errorf("url.Parse(%q).Path: got %q, want %q", got, u.Path, tc.wantPath)
			}
		})
	}
}
