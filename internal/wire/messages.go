// Package wire implements the client side of the hermes-nodes WSS
// protocol. This file defines the JSON message shapes sent on the
// wire; see PROTOCOL.md \u00a72-3.5 for the contract.
package wire

import (
	"encoding/json"
	"fmt"
	"time"
)

// MessageType is the discriminator on the envelope. The full list of
// reserved strings lives in PROTOCOL.md \u00a72; this file only covers
// the ones the client (node side) cares about in Task 1.5.
type MessageType string

const (
	TypeHello    MessageType = "hello"
	TypeHelloAck MessageType = "hello_ack"
	TypeHelloErr MessageType = "hello_err"
	TypeAuth     MessageType = "auth"
	TypeAuthOK   MessageType = "auth_ok"
	TypeAuthErr  MessageType = "auth_err"
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
