// Package wire: message dispatch loop. After the 1.5 handshake
// completes, the connection is otherwise idle until the server sends
// a call. This file is that read loop: pull one envelope off the
// wire, route by `type` to the matching handler, write the result
// back. Tasks 1.7 and 1.8 wire the real exec / read / write handlers
// to the shell and fs packages; this file is the routing layer and
// stays focused on the loop + the reserved-message handling.
package wire

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// ErrUnhandled is returned by a Handler when it could not produce a
// response (e.g. an exec handler that lost its shell). The dispatcher
// converts it into an `error` envelope with reason=internal_error so
// the server still gets a structured failure rather than a hang.
var ErrUnhandled = errors.New("wire: handler returned no response")

// Handler is the dispatch table entry. It receives the raw payload
// (a map[string]any decoded by codec.go's envelope path) and the
// caller's request id, and returns the response envelope to send
// back. Returning an error means "I couldn't produce a response";
// the dispatcher will then synthesise an `error` envelope for the
// caller with reason=internal_error so the server never hangs.
//
// Handlers must be safe to call from a single goroutine \u2014 the
// dispatch loop processes one message at a time. If a future
// handler needs to run long, it should spawn its own goroutine and
// return immediately with a "pending" envelope (out of scope for
// Task 1.6 \u2014 see PROTOCOL.md \u00a79.3 for the async-call shape
// that v0.2.0 will add).
type Handler func(ctx context.Context, requestID string, payload map[string]any) (Envelope, error)

// Dispatcher routes incoming server-originated messages to handlers
// registered with Register. The reserved types (ping / pong / bye /
// error) are handled in-line by Run and cannot be overridden.
type Dispatcher struct {
	conn *websocket.Conn

	// handlers maps server-originated call types to their
	// handlers. The lookup uses MessageType equality.
	handlers map[MessageType]Handler

	// OnError is invoked for non-fatal dispatch errors (handler
	// returned an error, write to the wire failed, etc.). The
	// dispatcher itself logs the on-the-wire failure; this hook
	// is for the caller to plumb it into their audit log.
	// Optional; nil = no-op.
	OnError func(err error, env Envelope)

	// OnRead is invoked after every successful read, before
	// the envelope is processed. The Task 1.9 heartbeat uses
	// this to bump the Pinger's liveness clock on every
	// received frame (PROTOCOL.md §6: "no pong (or any other
	// message) is received within 60s" means a connection is
	// dead — so every frame resets the watchdog, not just
	// pong). Optional; nil = no-op.
	OnRead func()

	// ReadTimeout bounds a single read inside the loop. The
	// PROTOCOL.md §6 heartbeat spec says a connection is dead
	// after 60s of silence. The heartbeat watchdog runs at
	// that same threshold, so the dispatcher's read deadline
	// is set to PongTimeout + 30s (default 90s) so the
	// watchdog wins the race — the dispatcher never times
	// out a still-healthy connection, but a truly dead
	// connection (no bytes at all) trips the watchdog first
	// and the dispatcher's deadline is just a backstop.
	ReadTimeout time.Duration

	// WriteTimeout bounds a single response write. Smaller than
	// the read timeout because a stuck write is unambiguous \u2014
	// the connection is wedged.
	WriteTimeout time.Duration
}

// NewDispatcher wraps a post-handshake Client. The dispatch loop does
// not own the connection's lifecycle: the caller is still responsible
// for closing the Client (and thus the conn) when Run returns.
//
// ReadTimeout is set to DefaultPongTimeout + 30s (90s) so the Task
// 1.9 heartbeat watchdog — which trips at 60s of silence per
// PROTOCOL.md §6 — wins the race for a healthy-but-silent
// connection. The dispatcher's read deadline is a backstop for the
// truly-no-bytes-at-all case, not the primary liveness check.
func NewDispatcher(c *Client) *Dispatcher {
	return &Dispatcher{
		conn:         c.Conn(),
		handlers:     make(map[MessageType]Handler),
		ReadTimeout:  DefaultPongTimeout + 30*time.Second,
		WriteTimeout: 10 * time.Second,
	}
}

// Register binds a server-originated call type to a handler. Calling
// Register with one of the reserved types (ping / pong / bye / error)
// returns an error \u2014 those have fixed handling in Run.
func (d *Dispatcher) Register(t MessageType, h Handler) error {
	switch t {
	case TypePing, TypePong, TypeBye, TypeError:
		return fmt.Errorf("wire: cannot register handler for reserved type %q", t)
	}
	if h == nil {
		return fmt.Errorf("wire: nil handler for type %q", t)
	}
	d.handlers[t] = h
	return nil
}

// Run reads envelopes from the connection, dispatches each to the
// matching handler (or the reserved-message path), writes the
// response, and loops. It returns when:
//   - ctx is cancelled (returns ctx.Err());
//   - the conn reads a close frame or an I/O error (returns the
//     underlying error);
//   - a `bye` envelope is received and the conn closes cleanly
//     (returns nil).
//
// Run is a single goroutine. Concurrent Register / Run is not safe;
// register all handlers before starting Run.
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		// Honour ctx cancellation between reads so a cancelled
		// context doesn't have to wait for the read timeout to
		// fire.
		if err := ctx.Err(); err != nil {
			return err
		}

		env, err := d.readOne(ctx)
		if err != nil {
			// gorilla/websocket wraps the close handshake as
			// a normal I/O error; surface a clean nil on the
			// expected close path so the caller's "server
			// asked us to disconnect" branch doesn't have to
			// know about the wrapper.
			if websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway) {
				return nil
			}
			return err
		}

		// Reserved / operational types are handled in-line. They
		// may return an envelope to send back (ping \u2192 pong) or
		// may end the loop (bye). The booleans tell us whether
		// the type was reserved (in which case we never call
		// dispatchCall, even if no response was produced) and
		// whether to terminate the loop (bye).
		response, isReserved, terminate, err := d.handleReserved(ctx, env)
		if err != nil {
			d.notifyError(err, env)
			return err
		}
		if isReserved {
			if terminate {
				return nil
			}
			if response != nil {
				if err := d.writeOne(ctx, *response); err != nil {
					return fmt.Errorf("wire: write reserved response: %w", err)
				}
			}
			continue
		}

		// Call-type dispatch.
		if err := d.dispatchCall(ctx, env); err != nil {
			// dispatchCall already attempts to send an error
			// envelope back to the server before returning.
			// If THAT write also fails, the connection is
			// toast \u2014 propagate.
			return err
		}
	}
}

// readOne pulls one envelope off the connection with the configured
// read timeout, then decodes it via the shared codec. After a
// successful read, OnRead (if set) is invoked so the heartbeat
// liveness clock can be bumped — PROTOCOL.md §6 says any received
// message counts as liveness, not just pong.
func (d *Dispatcher) readOne(ctx context.Context) (Envelope, error) {
	if err := d.conn.SetReadDeadline(deadlineFromCtx(ctx, d.ReadTimeout)); err != nil {
		return Envelope{}, fmt.Errorf("wire: set read deadline: %w", err)
	}
	_, raw, err := d.conn.ReadMessage()
	if err != nil {
		return Envelope{}, fmt.Errorf("wire: read: %w", err)
	}
	if d.OnRead != nil {
		d.OnRead()
	}
	var env Envelope
	if err := decodeEnvelope(raw, &env); err != nil {
		return Envelope{}, fmt.Errorf("wire: decode envelope: %w", err)
	}
	return env, nil
}

// writeOne sends one envelope with the configured write deadline.
// The envelope's MarshalJSON flattens its typed payload into the
// top-level wire shape (see messages.go).
func (d *Dispatcher) writeOne(ctx context.Context, env Envelope) error {
	if err := d.conn.SetWriteDeadline(deadlineFromCtx(ctx, d.WriteTimeout)); err != nil {
		return fmt.Errorf("wire: set write deadline: %w", err)
	}
	if err := d.conn.WriteJSON(env); err != nil {
		return fmt.Errorf("wire: write: %w", err)
	}
	return nil
}

// WriteEnvelope sends one envelope on the connection from outside
// the dispatch loop. It exists for the Task 1.9 pinger, which runs
// in a separate goroutine and needs to write `ping` frames
// without going through the dispatcher.
//
// WriteEnvelope is goroutine-safe with itself and with the
// dispatcher's Run loop only because gorilla/websocket's Conn
// serialises writes internally. The dispatcher never writes
// asynchronously (every write is inside the Run loop), so the
// only concurrent writer is the pinger. That keeps the invariant
// "at most one in-flight write at any time" intact.
//
// Callers that want to write from multiple goroutines must
// serialize themselves; this method does not.
//
// TODO(async-writes): this single-in-flight invariant is
// load-bearing for three layers (gorilla's write lock, the
// dispatch loop's Run, and the pinger's onPing closure). The
// moment a future task adds a second async writer — e.g. a
// long-running handler that streams results back while the
// dispatch loop is still servicing the next request, or an
// unsolicited `event` push from a background goroutine (PROTOCOL.md
// §10 lists events as a future direction) — gorilla's lock is no
// longer enough and the onPing closure here can interleave with
// a write that started after it. When that lands, the dispatch
// loop needs an explicit write-serialisation mutex and every
// caller of WriteEnvelope needs to be revisited.
func (d *Dispatcher) WriteEnvelope(ctx context.Context, env Envelope) error {
	return d.writeOne(ctx, env)
}

// handleReserved handles the four message types the dispatcher owns
// outright (no user handler). The two booleans distinguish three
// outcomes:
//
//	(response, true,  false, nil)  \u2014 reserved; send `response` and continue
//	(nil,     true,  false, nil)  \u2014 reserved; nothing to send back
//	                                  (unsolicited pong / server error),
//	                                  continue the loop
//	(nil,     true,  true,  nil)  \u2014 reserved; end the loop (bye)
//	(nil,     false, false, nil)  \u2014 not a reserved type; caller falls
//	                                  through to dispatchCall
//	(nil,     false, false, err)  \u2014 fatal; end the loop
func (d *Dispatcher) handleReserved(ctx context.Context, env Envelope) (*Envelope, bool, bool, error) {
	switch env.Type {
	case TypePing:
		// The ping's `ts` is an envelope-level field; the
		// codec hoists it onto env.TS and strips it from
		// the payload map, so we read it from env.TS here
		// (no need to round-trip through PingPayload).
		pong := Envelope{
			Type: TypePong,
			TS:   nowRFC3339(),
			Payload: PongPayload{
				TS:     nowRFC3339(),
				EchoTS: env.TS,
			},
		}
		return &pong, true, false, nil

	case TypePong:
		// We don't send pings from this loop (Task 1.9 owns
		// the heartbeat), so an unsolicited pong is a
		// protocol-level surprise. We log and continue
		// rather than tearing down the connection \u2014 the
		// server may simply have raced us.
		d.notifyError(fmt.Errorf("wire: unsolicited pong (no active ping)"), env)
		return nil, true, false, nil

	case TypeBye:
		// Server asks for graceful shutdown. PROTOCOL.md
		// \u00a73.14 says we close with code 1000. We don't
		// reply \u2014 the spec only requires a clean close
		// from the side that received the bye.
		_ = d.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(d.WriteTimeout),
		)
		return nil, true, true, nil

	case TypeError:
		// Out-of-band error from the server. The spec
		// (\u00a73.13) doesn't define a recovery action
		// beyond "the operator should see it in the
		// audit log". Surface it via OnError and keep
		// going \u2014 one bad error message shouldn't
		// tear down a working session.
		var ep ErrorPayload
		if err := reMarshalInto(env.Payload, &ep); err != nil {
			d.notifyError(fmt.Errorf("wire: decode server error: %w", err), env)
			return nil, true, false, nil
		}
		d.notifyError(
			fmt.Errorf("wire: server error: code=%d reason=%q detail=%q",
				ep.Code, ep.Reason, ep.Detail),
			env,
		)
		return nil, true, false, nil

	default:
		// Not a reserved type \u2014 caller will route to a
		// registered handler.
		return nil, false, false, nil
	}
}

// dispatchCall routes one server-originated call envelope to its
// handler and writes the response. Unknown types get a structured
// `error` envelope back so the server never hangs.
//
// A panic inside a registered handler is recovered at this boundary
// and converted to the same 5000/internal_error envelope shape a
// handler would get if it returned an error. PROTOCOL.md §3.15
// codifies this contract: a buggy handler MUST NOT take down the
// dispatcher's connection. The recovery is per-call because the
// dispatch loop is a single goroutine — recovering here is
// sufficient to bound any handler bug. The panic value is forwarded
// to OnError so the caller can log it (and a runtime.Stack()
// trace, if it wants one) into the audit log.
func (d *Dispatcher) dispatchCall(ctx context.Context, env Envelope) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// Build the same internal_error envelope
			// the err-returning path uses, so the wire
			// shape is identical regardless of how
			// the handler failed. The detail carries
			// the panic value's string form; callers
			// can't distinguish "handler returned
			// err" from "handler panicked" — which is
			// the protocol-level guarantee PROTOCOL.md
			// §3.15 makes.
			panicDetail := fmt.Sprintf("handler panic: %v", r)
			failure := NewErrorEnvelope(env.ID, ErrorPayload{
				Code:   5000,
				Reason: "internal_error",
				Detail: panicDetail,
			})
			if werr := d.writeOne(ctx, failure); werr != nil {
				// If we cannot write the error
				// envelope, the conn is wedged.
				// Promote the write error so Run
				// tears down and the supervisor can
				// reconnect. The original panic is
				// already swallowed by defer; the
				// audit hook fired below before the
				// write, so the operator still has
				// a record.
				err = fmt.Errorf("wire: write handler-panic error envelope: %w", werr)
				return
			}
			d.notifyError(fmt.Errorf("wire: handler panic: %v", r), env)
		}
	}()

	handler, ok := d.handlers[env.Type]
	if !ok {
		// Unknown type. PROTOCOL.md \u00a74 says 5001 /
		// unknown_message. We still echo the request id
		// (if any) so the server can correlate the
		// rejection with the call it sent.
		errEnv := NewErrorEnvelope(env.ID, ErrorPayload{
			Code:   5001,
			Reason: "unknown_message",
			Detail: fmt.Sprintf("no handler registered for type %q", env.Type),
		})
		if err := d.writeOne(ctx, errEnv); err != nil {
			return fmt.Errorf("wire: write unknown_message error: %w", err)
		}
		d.notifyError(
			fmt.Errorf("wire: unknown message type %q", env.Type),
			env,
		)
		return nil
	}

	// The codec hands us payload as map[string]any. Handlers
	// that want a typed struct re-decode via reMarshalInto.
	payload, _ := env.Payload.(map[string]any)

	response, err := handler(ctx, env.ID, payload)
	if err != nil {
		// Handler surfaced an error. Convert to a structured
		// error envelope so the server sees a real failure
		// rather than a hang. We do NOT override response if
		// the handler also returned one \u2014 but in practice
		// handlers should return either a response OR an
		// error, not both. If both come back we honour the
		// response.
		failure := NewErrorEnvelope(env.ID, ErrorPayload{
			Code:   5000,
			Reason: "internal_error",
			Detail: err.Error(),
		})
		if err := d.writeOne(ctx, failure); err != nil {
			return fmt.Errorf("wire: write handler error envelope: %w", err)
		}
		d.notifyError(err, env)
		return nil
	}

	// If the handler returned the zero Envelope, treat that as
	// "I forgot" \u2014 same conversion as above. Useful
	// guardrail for future handlers; today the typed
	// constructors in messages.go always populate Type.
	if response.Type == "" {
		failure := NewErrorEnvelope(env.ID, ErrorPayload{
			Code:   5000,
			Reason: "internal_error",
			Detail: "handler returned an empty envelope",
		})
		if err := d.writeOne(ctx, failure); err != nil {
			return fmt.Errorf("wire: write empty-envelope error: %w", err)
		}
		d.notifyError(ErrUnhandled, env)
		return nil
	}

	// Defensive: if the handler forgot to echo the request id,
	// do it for them. PROTOCOL.md \u00a73.7-3.11 require every
	// result envelope to carry the call's id so the server
	// can correlate.
	if response.ID == "" && env.ID != "" {
		response.ID = env.ID
	}

	if err := d.writeOne(ctx, response); err != nil {
		return fmt.Errorf("wire: write handler response: %w", err)
	}
	return nil
}

func (d *Dispatcher) notifyError(err error, env Envelope) {
	if d.OnError != nil {
		d.OnError(err, env)
	}
}
