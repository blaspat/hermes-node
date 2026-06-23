// Tests for the message dispatch loop (Task 1.6).
//
// Strategy: skip the 1.5 handshake (it's covered exhaustively in
// client_test.go) and stand up a raw *websocket.Conn pair using the
// same httptest.WebSocket pattern. The test then registers handlers
// on a Dispatcher, starts Run in a goroutine, drives messages in
// from the server side, and asserts what comes back.
//
// Each subtest re-uses the same test rig \u2014 we just register
// different handlers and drive different inputs. This keeps the
// tests focused on dispatch behaviour rather than connection
// plumbing, and means a failure in the rig itself is obvious
// (every subtest would fail at the same line).
package wire

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// connPair holds a client *websocket.Conn and the corresponding
// server-side *websocket.Conn. The server conn is what tests use
// to drive messages into the dispatcher; the client conn is what
// the dispatcher reads from and writes to.
//
// Close both via t.Cleanup. The Close ordering matters: closing
// the client first would race with the dispatcher's read loop,
// so we close the server first (which causes a normal closure
// on the client), then the client.
type connPair struct {
	client *websocket.Conn
	server *websocket.Conn
}

func (c *connPair) close() {
	_ = c.server.Close()
	_ = c.client.Close()
}

// newConnPair stands up an httptest server with a WebSocket
// upgrader, dials it as a client, and returns both ends. This is
// the same pattern the 1.5 tests use, minus the handshake layer.
func newConnPair(t *testing.T) *connPair {
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

	wsURL := "ws" + srv.URL[4:] // http://... \u2192 ws://...
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 2 * time.Second
	clientConn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
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

// newTestDispatcher wraps a *websocket.Conn in a *Client (just for
// the .Conn() accessor) and a Dispatcher. The Dispatcher uses the
// test's read/write timeouts so the suite runs fast.
func newTestDispatcher(t *testing.T, conn *websocket.Conn) *Dispatcher {
	t.Helper()
	c := &Client{conn: conn}
	d := NewDispatcher(c)
	d.ReadTimeout = 2 * time.Second
	d.WriteTimeout = 2 * time.Second
	return d
}

// runDispatcher launches d.Run in a goroutine and returns a channel
// that will receive the loop's return error. Test cancels the
// context to signal shutdown, then closes the client conn so the
// dispatcher's blocked ReadMessage returns immediately rather than
// waiting for the read deadline.
func runDispatcher(t *testing.T, d *Dispatcher) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		// Closing the conn unblocks the dispatcher's read
		// with an error so Run returns within a few ms
		// instead of waiting for the read deadline.
		_ = d.conn.Close()
		// Drain the goroutine; ignore the error because
		// we expect a closed-conn error. Some tests
		// consume `done` themselves before this runs;
		// for those, the select below returns
		// immediately.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Log("dispatcher did not exit within 2s of cancel+close")
		}
	})
	return cancel, done
}

// runDispatcherNoCleanup is like runDispatcher but does not
// register a t.Cleanup that drains the goroutine. Used by tests
// that consume the done channel themselves and then call cancel()
// (e.g. ByeEndsLoop).
func runDispatcherNoCleanup(t *testing.T, d *Dispatcher) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	return cancel, done
}

// readServerJSON reads one JSON message from the server side of
// the pair and decodes it into a map. Test helper only. Uses a
// short 2s read deadline; for tests that intentionally move more
// than 2s of payload (e.g. TestWriteHandler_FileTooLarge's 10MB
// write), use readServerJSONWithDeadline directly.
func readServerJSON(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	return readServerJSONWithDeadline(t, conn, 2*time.Second)
}

// readServerJSONWithDeadline is the deadline-aware variant of
// readServerJSON. The default 2s server read deadline in
// readEnvelope/ReadMessage is right for handler tests, but the
// 10MB cap test streams ~13.3MB through the test websocket and
// needs a longer window. Production code is unaffected — the
// production dispatcher has its own ReadTimeout/WriteTimeout
// and the test rig is not a model of the production path.
func readServerJSONWithDeadline(t *testing.T, conn *websocket.Conn, timeout time.Duration) map[string]any {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set server read deadline: %v", err)
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("server decode: %v (raw=%s)", err, string(raw))
	}
	return out
}

// writeServerJSON writes one JSON message from the server side.
// We use map[string]any so the wire shape stays the simple
// flat-envelope form the protocol requires.
func writeServerJSON(t *testing.T, conn *websocket.Conn, payload map[string]any) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set server write deadline: %v", err)
	}
	if err := conn.WriteJSON(payload); err != nil {
		t.Fatalf("server write: %v", err)
	}
}

// ----------------------------------------------------------------------------
// Test: each of the three call types is routed to its registered
// handler and the response id echoes the call's id.
// ----------------------------------------------------------------------------

// TestDispatch_CallTypes drives one exec, one read, one write
// from the server side and asserts the dispatcher routes each
// to the registered handler. We use a table-driven pattern so
// adding a fourth call type later is one row, not a new test.
func TestDispatch_CallTypes(t *testing.T) {
	cases := []struct {
		name         string
		callType     MessageType
		callPayload  map[string]any
		handlerResp  Envelope
		expectedType MessageType
	}{
		{
			name:     "exec",
			callType: TypeExec,
			callPayload: map[string]any{
				"id":      "req-exec-1",
				"type":    "exec",
				"command": "echo hi",
			},
			handlerResp: NewExecResultEnvelope("req-exec-1", ExecResultPayload{
				Status:     "ok",
				ExitCode:   0,
				Stdout:     "hi\n",
				DurationMS: 5,
			}),
			expectedType: TypeExecResult,
		},
		{
			name:     "read",
			callType: TypeRead,
			callPayload: map[string]any{
				"id":   "req-read-1",
				"type": "read",
				"path": "/etc/hostname",
			},
			handlerResp: NewReadResultEnvelope("req-read-1", ReadResultPayload{
				Status:     "ok",
				ContentB64: "aGkK", // base64("hi\n")
				SizeBytes:  3,
			}),
			expectedType: TypeReadResult,
		},
		{
			name:     "write",
			callType: TypeWrite,
			callPayload: map[string]any{
				"id":          "req-write-1",
				"type":        "write",
				"path":        "/tmp/x",
				"content_b64": "aGVsbG8=",
				"mode":        "overwrite",
			},
			handlerResp: NewWriteResultEnvelope("req-write-1", WriteResultPayload{
				Status:       "ok",
				BytesWritten: 5,
			}),
			expectedType: TypeWriteResult,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pair := newConnPair(t)
			d := newTestDispatcher(t, pair.client)

			// The handler captures the request id and
			// payload it received so we can assert the
			// dispatcher passed them through correctly.
			var gotID string
			var gotPayload map[string]any
			var handlerCalls atomic.Int32
			handler := func(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
				handlerCalls.Add(1)
				gotID = requestID
				gotPayload = payload
				return tc.handlerResp, nil
			}
			if err := d.Register(tc.callType, handler); err != nil {
				t.Fatalf("Register %s: %v", tc.callType, err)
			}

			_, _ = runDispatcher(t, d)

			// Drive one call from the server side.
			writeServerJSON(t, pair.server, tc.callPayload)

			// Read the response.
			resp := readServerJSON(t, pair.server)
			if resp["type"] != string(tc.expectedType) {
				t.Errorf("response type: got %q, want %q", resp["type"], tc.expectedType)
			}
			if resp["id"] != tc.callPayload["id"] {
				t.Errorf("response id: got %q, want %q", resp["id"], tc.callPayload["id"])
			}
			if got := handlerCalls.Load(); got != 1 {
				t.Errorf("handler invocations: got %d, want 1", got)
			}
			if gotID != tc.callPayload["id"] {
				t.Errorf("handler requestID: got %q, want %q", gotID, tc.callPayload["id"])
			}
			if gotPayload == nil {
				t.Errorf("handler payload: got nil, want non-nil")
			}
		})
	}
}

// TestDispatch_HandlerFillsResponseID is the safety net for the
// "handler forgot to set the id" path. PROTOCOL.md \u00a73.7-3.11
// require every result envelope to carry the call's id; the
// dispatcher copies it across if the handler left it empty. We
// confirm that with a handler that explicitly returns the
// zero-value id.
func TestDispatch_HandlerFillsResponseID(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	handler := func(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
		return Envelope{
			Type:    TypeExecResult,
			Payload: ExecResultPayload{Status: "ok", ExitCode: 0},
			// ID intentionally left empty.
		}, nil
	}
	if err := d.Register(TypeExec, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-id-echo",
		"type":    "exec",
		"command": "true",
	})
	resp := readServerJSON(t, pair.server)
	if resp["id"] != "req-id-echo" {
		t.Errorf("response id: got %q, want %q (dispatcher should auto-fill)", resp["id"], "req-id-echo")
	}
}

// ----------------------------------------------------------------------------
// Test: unknown message type returns a structured error envelope.
// ----------------------------------------------------------------------------

func TestDispatch_UnknownType(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	_, _ = runDispatcher(t, d)

	// No handler registered for "frobnicate". Send one of those
	// and assert the dispatcher replies with a structured
	// error envelope echoing the request id.
	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-unknown-1",
		"type": "frobnicate",
		"data": "wat",
	})
	resp := readServerJSON(t, pair.server)

	if resp["type"] != "error" {
		t.Errorf("response type: got %q, want error", resp["type"])
	}
	if resp["id"] != "req-unknown-1" {
		t.Errorf("response id: got %q, want %q (request id must be echoed)", resp["id"], "req-unknown-1")
	}
	// ErrorPayload's fields ride on the envelope as
	// top-level keys (the protocol's flat-envelope shape).
	if got := resp["code"]; got != float64(5001) {
		t.Errorf("response code: got %v, want 5001 (unknown_message per PROTOCOL.md \u00a74)", got)
	}
	if got := resp["reason"]; got != "unknown_message" {
		t.Errorf("response reason: got %q, want unknown_message", got)
	}
	if got, ok := resp["detail"].(string); !ok || got == "" {
		t.Errorf("response detail: got %q, want non-empty string naming the unknown type", resp["detail"])
	}
}

// TestDispatch_HandlerError becomes an internal_error envelope.
// A handler that returns an error must not hang the dispatcher;
// the server must see a structured failure with reason=internal_error
// (PROTOCOL.md \u00a74 code 5000).
func TestDispatch_HandlerError(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	handlerErr := errors.New("shell session lost")
	handler := func(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
		return Envelope{}, handlerErr
	}
	if err := d.Register(TypeExec, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-err-1",
		"type":    "exec",
		"command": "anything",
	})
	resp := readServerJSON(t, pair.server)

	if resp["type"] != "error" {
		t.Errorf("response type: got %q, want error", resp["type"])
	}
	if resp["id"] != "req-err-1" {
		t.Errorf("response id: got %q, want %q", resp["id"], "req-err-1")
	}
	if got := resp["code"]; got != float64(5000) {
		t.Errorf("response code: got %v, want 5000", got)
	}
	if got := resp["reason"]; got != "internal_error" {
		t.Errorf("response reason: got %q, want internal_error", got)
	}
	// The handler's error message is the detail \u2014 this
	// gives the server enough to surface in logs.
	if got, ok := resp["detail"].(string); !ok || got != handlerErr.Error() {
		t.Errorf("response detail: got %q, want %q", resp["detail"], handlerErr.Error())
	}
}

// ----------------------------------------------------------------------------
// Test: reserved types (ping / pong / bye / error) are handled
// in-line and not routed to user handlers.
// ----------------------------------------------------------------------------

func TestDispatch_PingRepliesWithPong(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	_, _ = runDispatcher(t, d)

	writeServerJSON(t, pair.server, map[string]any{
		"type": "ping",
		"ts":   "2026-06-05T00:00:00.000Z",
	})
	resp := readServerJSON(t, pair.server)
	if resp["type"] != "pong" {
		t.Errorf("response type: got %q, want pong", resp["type"])
	}
	if got := resp["echo_ts"]; got != "2026-06-05T00:00:00.000Z" {
		t.Errorf("pong echo_ts: got %q, want 2026-06-05T00:00:00.000Z", got)
	}
}

func TestDispatch_ByeEndsLoop(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	// Use the no-cleanup variant because this test consumes
	// the done channel itself to assert Run returns promptly.
	cancel, done := runDispatcherNoCleanup(t, d)
	t.Cleanup(func() { cancel(); _ = d.conn.Close() })

	writeServerJSON(t, pair.server, map[string]any{
		"type":   "bye",
		"reason": "server_shutdown",
	})

	// Run() should return nil within a short window.
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after bye: got error %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of bye")
	}
}

func TestDispatch_ServerErrorContinuesLoop(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	// OnError must fire for the server's out-of-band error,
	// and the loop must keep going (a single error message
	// shouldn't tear down a working session).
	var (
		errMu   sync.Mutex
		errSeen error
	)
	d.OnError = func(err error, _ Envelope) {
		errMu.Lock()
		defer errMu.Unlock()
		errSeen = err
	}

	// Register an exec handler so we can prove the loop is
	// still routing after the server's error.
	var secondCall atomic.Int32
	if err := d.Register(TypeExec, func(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
		secondCall.Add(1)
		return NewExecResultEnvelope(requestID, ExecResultPayload{Status: "ok", ExitCode: 0}), nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _ = runDispatcher(t, d)

	// 1. Server sends an out-of-band error.
	writeServerJSON(t, pair.server, map[string]any{
		"type":   "error",
		"code":   5000,
		"reason": "internal_error",
		"detail": "ops",
	})

	// 2. Server follows up with a normal exec.
	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-after-err",
		"type":    "exec",
		"command": "true",
	})

	resp := readServerJSON(t, pair.server)
	if resp["type"] != "exec_result" {
		t.Errorf("after server error, exec response: got %q, want exec_result", resp["type"])
	}
	if resp["id"] != "req-after-err" {
		t.Errorf("after server error, exec response id: got %q, want req-after-err", resp["id"])
	}
	if got := secondCall.Load(); got != 1 {
		t.Errorf("handler invocations after server error: got %d, want 1", got)
	}

	errMu.Lock()
	defer errMu.Unlock()
	if errSeen == nil {
		t.Errorf("OnError was not called for server error")
	}
}

// ----------------------------------------------------------------------------
// Test: a panicking handler is recovered at the dispatchCall boundary;
// the dispatcher synthesises a 5000/internal_error envelope echoing the
// call's request id, fires OnError, keeps the connection open, and can
// service subsequent calls. PROTOCOL.md §3.15 codifies this contract.
// ----------------------------------------------------------------------------

// TestDispatch_HandlerPanicRecovers asserts the four post-conditions
// of the handler-panic recovery path (PROTOCOL.md §3.15):
//
//  1. The server receives a structured `error` envelope (code=5000,
//     reason=internal_error) whose id matches the call that panicked.
//  2. OnError is invoked with the panic value so the audit log sees it.
//  3. The connection is NOT closed — a second call (using a different
//     registered handler) round-trips successfully.
//  4. The panic value appears in the `detail` field so the operator
//     can diagnose from logs without a debugger attached.
func TestDispatch_HandlerPanicRecovers(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)

	// The panicking handler. A typed string panic value is the
	// easiest to assert against — strings survive
	// runtime/debug.Stack, reflect, fmt.Sprintf, and the
	// gorilla/websocket JSON encoder without surprises.
	panicValue := "kaboom: handler intentionally exploded"
	handler := func(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
		panic(panicValue)
	}
	if err := d.Register(TypeExec, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Register a second handler on a different type so we can
	// prove the dispatcher is still routing after the panic.
	// Same dispatcher, same goroutine, second call lands cleanly.
	var secondCall atomic.Int32
	if err := d.Register(TypeRead, func(ctx context.Context, requestID string, payload map[string]any) (Envelope, error) {
		secondCall.Add(1)
		return NewReadResultEnvelope(requestID, ReadResultPayload{
			Status:     "ok",
			ContentB64: "aGkK", // base64("hi\n")
			SizeBytes:  3,
		}), nil
	}); err != nil {
		t.Fatalf("Register read: %v", err)
	}

	// Capture OnError invocations.
	var (
		errMu   sync.Mutex
		errSeen error
	)
	d.OnError = func(err error, _ Envelope) {
		errMu.Lock()
		defer errMu.Unlock()
		errSeen = err
	}

	_, _ = runDispatcher(t, d)

	// 1. Drive the call that will panic.
	writeServerJSON(t, pair.server, map[string]any{
		"id":      "req-panic-1",
		"type":    "exec",
		"command": "true",
	})

	// 2. Read the error envelope the dispatcher should have
	// synthesised in place of the panicked handler's response.
	resp := readServerJSON(t, pair.server)

	if resp["type"] != "error" {
		t.Errorf("after panic, response type: got %q, want error", resp["type"])
	}
	if resp["id"] != "req-panic-1" {
		t.Errorf("after panic, response id: got %q, want req-panic-1 (call id must be echoed)", resp["id"])
	}
	if got := resp["code"]; got != float64(5000) {
		t.Errorf("after panic, response code: got %v, want 5000 (internal_error per PROTOCOL.md §4)", got)
	}
	if got := resp["reason"]; got != "internal_error" {
		t.Errorf("after panic, response reason: got %q, want internal_error", got)
	}
	// 4. The protocol contract (PROTOCOL.md §3.15) is "the wire
	// shape is the same 5000/internal_error a handler would get
	// by returning an error; the detail is the panic value".
	// The dispatcher's exact prefix isn't part of the contract
	// — only the panic value's presence is — so we assert
	// containment rather than equality, leaving room to evolve
	// the prefix (e.g. "handler panic: %v" → "panic: %v" → a
	// structured field) without breaking the test.
	if got, ok := resp["detail"].(string); !ok || !strings.Contains(got, panicValue) {
		t.Errorf("after panic, response detail: got %q, want it to contain %q (panic value must surface to the wire)", resp["detail"], panicValue)
	}

	// 3. The dispatcher must still be alive. Drive a second
	// call (different type, different handler) and assert
	// its normal response comes back. If the panic had torn
	// down the dispatch goroutine, this read would either
	// hang or return an error.
	writeServerJSON(t, pair.server, map[string]any{
		"id":   "req-after-panic",
		"type": "read",
		"path": "/etc/hostname",
	})
	resp2 := readServerJSON(t, pair.server)
	if resp2["type"] != "read_result" {
		t.Errorf("after panic, second response type: got %q, want read_result (dispatcher must still be running)", resp2["type"])
	}
	if resp2["id"] != "req-after-panic" {
		t.Errorf("after panic, second response id: got %q, want req-after-panic", resp2["id"])
	}
	if got := secondCall.Load(); got != 1 {
		t.Errorf("after panic, second handler invocations: got %d, want 1 (panic must not poison the loop)", got)
	}

	// 4. OnError was invoked with the panic value.
	errMu.Lock()
	defer errMu.Unlock()
	if errSeen == nil {
		t.Fatal("OnError was not called for handler panic")
	}
	if !strings.Contains(errSeen.Error(), panicValue) {
		t.Errorf("OnError error text: got %q, want it to contain %q (panic value must surface to the audit log)", errSeen.Error(), panicValue)
	}
}

// ----------------------------------------------------------------------------
// Test: Register rejects reserved types.
// ----------------------------------------------------------------------------

func TestRegister_RejectsReservedTypes(t *testing.T) {
	pair := newConnPair(t)
	d := newTestDispatcher(t, pair.client)
	noop := func(context.Context, string, map[string]any) (Envelope, error) {
		return Envelope{}, nil
	}
	for _, reserved := range []MessageType{TypePing, TypePong, TypeBye, TypeError} {
		if err := d.Register(reserved, noop); err == nil {
			t.Errorf("Register(%q): expected error, got nil", reserved)
		}
	}
	// Nil handler also rejected.
	if err := d.Register(TypeExec, nil); err == nil {
		t.Errorf("Register(TypeExec, nil): expected error, got nil")
	}
}
