// Package wire implements the client side of the hermes-node WSS
// protocol. This file defines the JSON message shapes sent on the
// wire; see PROTOCOL.md \u00a72-3.5 for the contract.
package wire

import (
	"encoding/json"
	"fmt"
	"time"
)

// MessageType is the discriminator on the envelope. The full list of
// reserved strings lives in PROTOCOL.md \u00a72-3. The handshake types
// (hello / auth) are added in Task 1.5; the call types (exec / read /
// write) and their result envelopes are added here in Task 1.6.
type MessageType string

const (
	// Handshake (Task 1.5).
	TypeHello    MessageType = "hello"
	TypeHelloAck MessageType = "hello_ack"
	TypeHelloErr MessageType = "hello_err"
	TypeAuth     MessageType = "auth"
	TypeAuthOK   MessageType = "auth_ok"
	TypeAuthErr  MessageType = "auth_err"

	// Server-initiated calls (Task 1.6 dispatch loop).
	TypeExec        MessageType = "exec"
	TypeExecResult  MessageType = "exec_result"
	TypeRead        MessageType = "read"
	TypeReadResult  MessageType = "read_result"
	TypeWrite       MessageType = "write"
	TypeWriteResult MessageType = "write_result"

	// Operational / lifecycle.
	TypePing  MessageType = "ping"
	TypePong  MessageType = "pong"
	TypeError MessageType = "error"
	TypeBye   MessageType = "bye"
)

// Envelope is the outer shape of every message. Fields are a
// superset of what each message type uses; json.Unmarshal leaves
// the extras at their zero values.
type Envelope struct {
	Type    MessageType `json:"type"`
	ID      string      `json:"id,omitempty"`
	TS      string      `json:"ts,omitempty"`
	Payload any         `json:"-"` // marshalled into the envelope via custom MarshalJSON
}

// MarshalJSON flattens the typed payload into the envelope so each
// message type's fields sit at the top level (the protocol uses
// "duck-typed" envelopes, not nested objects). We do this with a
// round-trip through json.Marshal so the payload's own json tags
// (snake_case, omitempty, etc.) are honoured consistently.
func (e Envelope) MarshalJSON() ([]byte, error) {
	// Start with the envelope's own fields, then merge the
	// payload's marshalled form. We use a map rather than
	// anonymous struct embedding to keep ordering stable
	// (json.Marshal sorts map keys alphabetically) and to
	// avoid the "anonymous *struct fields promote" quirk
	// when the payload itself has a "type" field.
	raw := map[string]any{
		"type": e.Type,
	}
	if e.ID != "" {
		raw["id"] = e.ID
	}
	if e.TS != "" {
		raw["ts"] = e.TS
	}
	if e.Payload != nil {
		payloadBytes, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, fmt.Errorf("wire: marshal payload: %w", err)
		}
		var payloadMap map[string]any
		if err := json.Unmarshal(payloadBytes, &payloadMap); err != nil {
			return nil, fmt.Errorf("wire: re-decode payload: %w", err)
		}
		for k, v := range payloadMap {
			raw[k] = v
		}
	}
	return json.Marshal(raw)
}

// nowRFC3339 returns the current UTC time formatted as RFC3339
// with millisecond precision, matching PROTOCOL.md \u00a72.
func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// HelloPayload is the body of a `hello` message (PROTOCOL.md \u00a73.1).
type HelloPayload struct {
	ProtocolVersion string   `json:"protocol_version"`
	NodeName        string   `json:"node_name"`
	NodeVersion     string   `json:"node_version"`
	Platform        string   `json:"platform"`
	Arch            string   `json:"arch"`
	Capabilities    []string `json:"capabilities"`
}

// NewHelloEnvelope builds a `hello` envelope ready to send. ts is
// stamped automatically; nodeVersion/platform/arch are filled in by
// the caller (typically main.go knows the build-time values).
func NewHelloEnvelope(protocolVersion, nodeName, nodeVersion, platform, arch string, caps []string) Envelope {
	return Envelope{
		Type: TypeHello,
		TS:   nowRFC3339(),
		Payload: HelloPayload{
			ProtocolVersion: protocolVersion,
			NodeName:        nodeName,
			NodeVersion:     nodeVersion,
			Platform:        platform,
			Arch:            arch,
			Capabilities:    caps,
		},
	}
}

// HelloAckPayload is the body of a `hello_ack` message (PROTOCOL.md
// \u00a73.2). The client treats it as the only acceptable response to
// `hello`; a `hello_err` triggers a different path.
type HelloAckPayload struct {
	ProtocolVersion string `json:"protocol_version"`
	SessionID       string `json:"session_id"`
	ServerTime      string `json:"server_time"`
}

// HelloErrPayload is the body of a `hello_err` message (PROTOCOL.md
// \u00a73.3). The client treats this as a fatal handshake failure.
type HelloErrPayload struct {
	Reason           string `json:"reason"`
	Code             int    `json:"code"`
	ServerMaxVersion string `json:"server_max_version,omitempty"`
}

// AuthPayload is the body of an `auth` message (PROTOCOL.md \u00a73.4).
type AuthPayload struct {
	NodeName string `json:"node_name"`
	Token    string `json:"token"`
}

// NewAuthEnvelope builds an `auth` envelope. The server validates
// that NodeName matches the token's bound name.
func NewAuthEnvelope(nodeName, token string) Envelope {
	return Envelope{
		Type: TypeAuth,
		TS:   nowRFC3339(),
		Payload: AuthPayload{
			NodeName: nodeName,
			Token:    token,
		},
	}
}

// AuthOKPayload is the body of an `auth_ok` message (PROTOCOL.md
// \u00a73.5). The session_id is informational on the client side \u2014
// the server uses it to correlate logs.
type AuthOKPayload struct {
	SessionID string `json:"session_id"`
}

// AuthErrPayload is the body of an `auth_err` message (PROTOCOL.md
// \u00a73.5). The client treats this as a fatal handshake failure and
// surfaces the reason to the operator.
type AuthErrPayload struct {
	Reason string `json:"reason"`
	Code   int    `json:"code"`
}

// reMarshalInto takes whatever was in the envelope's payload slot
// (a map[string]any after the round-trip, or a typed struct if we
// built the envelope locally) and decodes it into the target typed
// payload. We go through json to keep one code path for both
// "locally built" and "received from the wire" envelopes.
func reMarshalInto(src any, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return fmt.Errorf("wire: marshal payload: %w", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("wire: unmarshal payload: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Task 1.6: server-originated call envelopes + node-originated results.
// PROTOCOL.md \u00a73.6-3.11 + \u00a73.13.
// ----------------------------------------------------------------------------

// ExecPayload is the body of an `exec` message (PROTOCOL.md \u00a73.6).
// Cwd / Env / TimeoutMS are optional; the dispatcher's handler is
// responsible for applying defaults (last cwd, 60s default, 600s max).
type ExecPayload struct {
	Command   string            `json:"command"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
}

// ExecResultPayload is the body of an `exec_result` message
// (PROTOCOL.md \u00a73.7). Status is "ok" / "error" / "timeout". On
// status=ok, ExitCode is 0. On status=error the exit code carries the
// real exit number (or -1 if the command couldn't be launched at all).
// On status=timeout ExitCode is -1 and the duration reflects the wall
// clock at the moment the node killed the process.
type ExecResultPayload struct {
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`
}

// NewExecResultEnvelope builds an `exec_result` envelope that echoes
// the call's request id. The dispatch loop uses this for the happy
// path; status="ok" / "error" / "timeout" are set by the caller.
func NewExecResultEnvelope(requestID string, result ExecResultPayload) Envelope {
	return Envelope{
		Type:    TypeExecResult,
		ID:      requestID,
		Payload: result,
	}
}

// ReadPayload is the body of a `read` message (PROTOCOL.md \u00a73.8).
type ReadPayload struct {
	Path string `json:"path"`
}

// ReadResultPayload is the body of a `read_result` message
// (PROTOCOL.md \u00a73.9). On status=ok, ContentB64 holds the file
// bytes base64-encoded and SizeBytes is the on-disk size. On
// status=error, Error / ErrorDetail carry the reason ("path_not_allowed",
// "file_not_found", "file_too_large", "io_error").
type ReadResultPayload struct {
	Status      string `json:"status"`
	ContentB64  string `json:"content_b64,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	Error       string `json:"error,omitempty"`
	ErrorDetail string `json:"error_detail,omitempty"`
}

// NewReadResultEnvelope builds a `read_result` envelope that echoes
// the call's request id.
func NewReadResultEnvelope(requestID string, result ReadResultPayload) Envelope {
	return Envelope{
		Type:    TypeReadResult,
		ID:      requestID,
		Payload: result,
	}
}

// WritePayload is the body of a `write` message (PROTOCOL.md \u00a73.10).
// Mode defaults to "overwrite" when empty per the protocol.
type WritePayload struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64"`
	Mode       string `json:"mode,omitempty"`
}

// WriteResultPayload is the body of a `write_result` message
// (PROTOCOL.md \u00a73.11). On status=ok, BytesWritten is the number of
// bytes actually written to disk. On status=error, Error /
// ErrorDetail carry the reason.
type WriteResultPayload struct {
	Status       string `json:"status"`
	BytesWritten int64  `json:"bytes_written,omitempty"`
	Error        string `json:"error,omitempty"`
	ErrorDetail  string `json:"error_detail,omitempty"`
}

// NewWriteResultEnvelope builds a `write_result` envelope that echoes
// the call's request id.
func NewWriteResultEnvelope(requestID string, result WriteResultPayload) Envelope {
	return Envelope{
		Type:    TypeWriteResult,
		ID:      requestID,
		Payload: result,
	}
}

// ErrorPayload is the body of an `error` envelope (PROTOCOL.md
// \u00a73.13). Used both for out-of-band errors and for structured
// rejections of a specific call (e.g. unknown_message \u2014 see
// PROTOCOL.md \u00a74 for the full code table).
type ErrorPayload struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// NewErrorEnvelope builds an `error` envelope. The id is echoed when
// known so the server can correlate the failure with the request it
// was responding to; per PROTOCOL.md \u00a73.13 the envelope itself is
// "out-of-band" and the id is informational.
func NewErrorEnvelope(requestID string, payload ErrorPayload) Envelope {
	env := Envelope{
		Type:    TypeError,
		Payload: payload,
	}
	if requestID != "" {
		env.ID = requestID
	}
	return env
}

// PingPayload / PongPayload are the bodies of `ping` / `pong`
// messages (PROTOCOL.md \u00a73.12). The node's dispatch loop
// auto-replies to pings with a pong that echoes the inbound ts.
type PingPayload struct {
	TS string `json:"ts,omitempty"`
}

type PongPayload struct {
	TS     string `json:"ts,omitempty"`
	EchoTS string `json:"echo_ts,omitempty"`
}
