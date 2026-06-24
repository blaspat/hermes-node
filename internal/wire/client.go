// Package wire implements the client side of the hermes-node WSS
// protocol. This file is the dialer + handshake state machine. The
// dispatch loop (server-initiated exec/read/write) is in dispatch.go
// (Task 1.6) and intentionally lives in a separate file so the
// handshake here is reviewable in isolation.
package wire

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// ProtocolVersion is the version we declare in `hello` (PROTOCOL.md
// \u00a75). Bump in lock-step with PROTOCOL.md; the server may downgrade
// the agreed version in its `hello_ack`.
const ProtocolVersion = "0.1.0"

// DefaultHandshakeTimeout bounds the time we'll wait for hello_ack
// and auth_ok. 10s is plenty for any honest network and short enough
// that a wedged server doesn't pin the start-up of hermes-node.
const DefaultHandshakeTimeout = 10 * time.Second

// ErrAuthFailed is returned by Connect when the server replies
// auth_err. Wrapped with the reason; callers can errors.Is against
// this sentinel to branch.
var ErrAuthFailed = errors.New("wire: authentication failed")

// ErrProtocolMismatch is returned by Connect when the server replies
// hello_err (version mismatch or other protocol-level rejection).
var ErrProtocolMismatch = errors.New("wire: protocol version rejected by server")

// ErrHandshakeTimeout is returned by Connect when the server doesn't
// respond within the configured handshake timeout.
var ErrHandshakeTimeout = errors.New("wire: handshake timed out")

// ErrUnexpectedMessage is returned by Connect when the server sends a
// message type we don't expect in the handshake (anything other than
// hello_ack/hello_err in response to hello, or auth_ok/auth_err in
// response to auth).
var ErrUnexpectedMessage = errors.New("wire: unexpected message during handshake")

// defaultWSPath is the wire server's WebSocket path.  Operators may omit
// it when configuring a node and normaliseServerURL will append it.
const defaultWSPath = "/ws/nodes"

// DialOptions configures one Connect call. The defaults are fine for
// production; the test server overrides HandshakeTimeout so the suite
// can run fast.
type DialOptions struct {
	// ServerURL is the WSS endpoint, e.g. wss://hermes.example.com/ws/nodes.
	// If the path is omitted normaliseServerURL appends defaultWSPath.
	ServerURL string

	// NodeName / Token identify the node to the server (PROTOCOL.md
	// \u00a73.1, \u00a73.4).
	NodeName string
	Token    string

	// NodeVersion is the build-time version stamp of the binary.
	NodeVersion string

	// Platform / Arch are filled in by main.go from runtime.GOOS /
	// runtime.GOARCH.
	Platform string
	Arch     string

	// Capabilities is the static list the node advertises in `hello`
	// (PROTOCOL.md §3.1). The server treats this as a hint, not a
	// contract, but the order is preserved on the wire.
	Capabilities []string

	// HandshakeTimeout bounds the wait for hello_ack and auth_ok.
	// Zero means DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration

	// TLSConfig, when non-nil, is attached to a copy of
	// websocket.DefaultDialer. The copy is used only for this one
	// Connect call so concurrent dials don't share TLS state. When
	// nil, DefaultDialer is used directly (its zero-valued
	// TLSClientConfig trusts the OS CA bundle, which is correct for
	// public CA-signed servers).
	//
	// Build it via config.BuildTLSConfig if you're wiring main.go
	// from a [server] section; that helper honours ca_cert (PEM
	// bundle) and pinned_cert_sha256 (leaf pin) per PROTOCOL.md §7.
	//
	// TLSConfig is read at dial time only; the same instance may be
	// safely shared across reconnect cycles. VerifyConnection
	// captures the pin bytes by closure, and RootCAs is a
	// *x509.CertPool the caller should treat as immutable once
	// handed over. A future maintainer should not construct a
	// per-call closure here.
	TLSConfig *tls.Config
}

// withDefaults returns a copy of opts with zero-valued fields filled
// in. We don't mutate the caller's struct in case they reuse it.
func (o DialOptions) withDefaults() DialOptions {
	if o.HandshakeTimeout == 0 {
		o.HandshakeTimeout = DefaultHandshakeTimeout
	}
	return o
}

// Client is the live, post-handshake WSS connection. It owns the
// underlying *websocket.Conn; the dispatch loop (1.6) will read
// server-initiated messages off it and write responses back.
//
// The fields are unexported because external callers should not
// touch the connection directly \u2014 every protocol message must go
// through this package's helpers so the framing stays in one place.
type Client struct {
	conn     *websocket.Conn
	nodeName string

	// sessionID is the value the server put in its hello_ack /
	// auth_ok. Useful for log correlation; not part of the
	// connection state machine.
	sessionID string
}

// Conn returns the underlying *websocket.Conn. Reserved for the
// dispatch loop in Task 1.6; tests don't use it.
func (c *Client) Conn() *websocket.Conn { return c.conn }

// SessionID returns the server-assigned session identifier.
func (c *Client) SessionID() string { return c.sessionID }

// NodeName returns the name this client authenticated as.
func (c *Client) NodeName() string { return c.nodeName }

// Connect dials the server, performs the full hello \u2192 hello_ack \u2192
// auth \u2192 auth_ok handshake, and returns a ready-to-use Client. On
// auth_err or hello_err it returns a non-nil error wrapping the
// server's reason; the connection is already closed in that case
// (the server sends a close frame right after the err message per
// PROTOCOL.md \u00a73.5 / \u00a74).
//
// Connect does NOT spawn a heartbeat or read loop \u2014 those are
// tasks 1.6 and 1.9. It only completes the handshake; once it
// returns successfully, the caller owns the connection's lifecycle.
func Connect(ctx context.Context, opts DialOptions) (*Client, error) {
	opts = opts.withDefaults()

	// Normalise ServerURL: append /ws/nodes if the user omitted it.
	// This lets operators pass just "wss://host:port" without the path,
	// which is the natural mistake given the Python CLI prints "<host:port>"
	// in its pair instructions.
	opts.ServerURL = normaliseServerURL(opts.ServerURL)

	wsCtx, cancel := context.WithTimeout(ctx, opts.HandshakeTimeout)
	defer cancel()

	// Build the dialer. A nil opts.TLSConfig leaves the default
	// dialer in place (OS CA bundle, no pin). A non-nil config
	// gets attached to a copy of DefaultDialer — we don't mutate
	// the package-global default because that would race with
	// other dials in the process.
	dialer := *websocket.DefaultDialer
	if opts.TLSConfig != nil {
		dialer.TLSClientConfig = opts.TLSConfig
	}
	conn, _, err := dialer.DialContext(wsCtx, opts.ServerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("wire: dial %s: %w", opts.ServerURL, err)
	}
	// From here on, any error path must close conn so we don't
	// leak the socket.
	c := &Client{conn: conn, nodeName: opts.NodeName}
	if err := c.handshake(ctx, opts); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// handshake runs the four-message exchange against an already-open
// connection. It is split out from Connect so tests can drive the
// state machine directly with a pre-built *websocket.Conn if needed.
func (c *Client) handshake(ctx context.Context, opts DialOptions) error {
	// 1. Send hello.
	hello := NewHelloEnvelope(
		ProtocolVersion,
		opts.NodeName,
		opts.NodeVersion,
		opts.Platform,
		opts.Arch,
		opts.Capabilities,
	)
	if err := c.writeJSON(ctx, opts.HandshakeTimeout, hello); err != nil {
		return fmt.Errorf("wire: send hello: %w", err)
	}

	// 2. Await hello_ack or hello_err.
	ackEnv, err := c.readTyped(ctx, opts.HandshakeTimeout)
	if err != nil {
		return fmt.Errorf("wire: read hello_ack: %w", err)
	}
	switch ackEnv.Type {
	case TypeHelloAck:
		var ack HelloAckPayload
		if err := reMarshalInto(ackEnv.Payload, &ack); err != nil {
			return fmt.Errorf("wire: decode hello_ack: %w", err)
		}
		c.sessionID = ack.SessionID
	case TypeHelloErr:
		var he HelloErrPayload
		if err := reMarshalInto(ackEnv.Payload, &he); err != nil {
			return fmt.Errorf("wire: decode hello_err: %w", err)
		}
		return fmt.Errorf("%w: reason=%q code=%d server_max_version=%q",
			ErrProtocolMismatch, he.Reason, he.Code, he.ServerMaxVersion)
	default:
		return fmt.Errorf("%w: got %q, want hello_ack or hello_err",
			ErrUnexpectedMessage, ackEnv.Type)
	}

	// 3. Send auth.
	auth := NewAuthEnvelope(opts.NodeName, opts.Token)
	if err := c.writeJSON(ctx, opts.HandshakeTimeout, auth); err != nil {
		return fmt.Errorf("wire: send auth: %w", err)
	}

	// 4. Await auth_ok or auth_err.
	authEnv, err := c.readTyped(ctx, opts.HandshakeTimeout)
	if err != nil {
		return fmt.Errorf("wire: read auth_ok: %w", err)
	}
	switch authEnv.Type {
	case TypeAuthOK:
		var ok AuthOKPayload
		if err := reMarshalInto(authEnv.Payload, &ok); err != nil {
			return fmt.Errorf("wire: decode auth_ok: %w", err)
		}
		// The server's auth_ok session_id should match
		// hello_ack's; if it doesn't, something is off, but
		// PROTOCOL.md \u00a73.5 doesn't require us to enforce
		// it. We surface the auth_ok value if hello_ack
		// didn't have one (defensive).
		if c.sessionID == "" {
			c.sessionID = ok.SessionID
		}
		return nil
	case TypeAuthErr:
		var ae AuthErrPayload
		if err := reMarshalInto(authEnv.Payload, &ae); err != nil {
			return fmt.Errorf("wire: decode auth_err: %w", err)
		}
		return fmt.Errorf("%w: reason=%q code=%d", ErrAuthFailed, ae.Reason, ae.Code)
	default:
		return fmt.Errorf("%w: got %q, want auth_ok or auth_err",
			ErrUnexpectedMessage, authEnv.Type)
	}
}

// writeJSON sends one envelope with a per-message write deadline.
// gorilla/websocket serialises writes internally so two concurrent
// writes would corrupt the stream; the handshake is single-threaded
// so we don't take a lock here. Task 1.6's dispatch loop will.
func (c *Client) writeJSON(ctx context.Context, timeout time.Duration, env Envelope) error {
	if err := c.conn.SetWriteDeadline(deadlineFromCtx(ctx, timeout)); err != nil {
		return fmt.Errorf("wire: set write deadline: %w", err)
	}
	if err := c.conn.WriteJSON(env); err != nil {
		return fmt.Errorf("wire: write: %w", err)
	}
	return nil
}

// readTyped reads one message and decodes it into a typed envelope.
// We use a two-step decode (raw bytes \u2192 Envelope with Payload as
// map, then dispatch on Type into the typed payload) so the
// per-type payload structs stay in messages.go and the read loop
// doesn't need a giant type switch inline.
func (c *Client) readTyped(ctx context.Context, timeout time.Duration) (Envelope, error) {
	if err := c.conn.SetReadDeadline(deadlineFromCtx(ctx, timeout)); err != nil {
		return Envelope{}, fmt.Errorf("wire: set read deadline: %w", err)
	}
	_, raw, err := c.conn.ReadMessage()
	if err != nil {
		return Envelope{}, fmt.Errorf("wire: read: %w", err)
	}
	var env Envelope
	if err := decodeEnvelope(raw, &env); err != nil {
		return Envelope{}, fmt.Errorf("wire: decode envelope: %w", err)
	}
	return env, nil
}

// deadlineFromCtx returns the sooner of ctx's deadline and now+timeout.
// If ctx has no deadline, returns now+timeout. We use this rather
// than SetDeadline(time.Now().Add(timeout)) so a caller that passes
// a context.WithTimeout(ctx, 5*time.Second) sees its own timeout
// honoured even if the caller's opts.HandshakeTimeout is longer.
func deadlineFromCtx(ctx context.Context, timeout time.Duration) time.Time {
	now := time.Now()
	t := now.Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(t) {
		return dl
	}
	return t
}

// normaliseServerURL appends /ws/nodes if the given URL has no path component.
// This lets operators pass "wss://host:port" and have it Just Work, without
// having to know the server's internal routing path.
func normaliseServerURL(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		// Invalid URL — leave it unchanged so the dial error is informative
		return serverURL
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = defaultWSPath
	}
	return u.String()
}
