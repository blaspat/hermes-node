// Package wire: shared JSON encoding helpers used by client.go and
// (in 1.6) dispatch.go. Kept in its own file so messages.go can stay
// focused on type definitions and client.go can stay focused on the
// handshake state machine.
package wire

import (
	"encoding/json"
	"fmt"
)

// decodeEnvelope reads a single JSON envelope off the wire. The
// payload is left as a json.RawMessage so the caller can dispatch on
// the `type` field and then re-decode into the appropriate typed
// payload (HelloAckPayload, AuthErrPayload, etc.) without an
// intermediate map[string]any allocation.
//
// We use a custom unmarshaler on Envelope so the "Payload any"
// field captures the full set of remaining keys verbatim, which
// means the typed re-decode in messages.go (reMarshalInto) gets the
// snake_case names the protocol mandates without further mapping.
func decodeEnvelope(raw []byte, dst *Envelope) error {
	// First pass: extract the discriminator and the timestamp /
	// id at the top level, leaving the rest as raw JSON.
	var header struct {
		Type MessageType    `json:"type"`
		ID   string         `json:"id,omitempty"`
		TS   string         `json:"ts,omitempty"`
		Rest map[string]any `json:"-"`
	}
	// Decode twice: once for the typed header, once for the raw
	// map. json.Unmarshal doesn't have a built-in "everything
	// else" syntax until you reach for a custom UnmarshalJSON.
	header.Rest = make(map[string]any)
	if err := json.Unmarshal(raw, &header.Rest); err != nil {
		return fmt.Errorf("wire: parse envelope: %w", err)
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return fmt.Errorf("wire: parse envelope header: %w", err)
	}
	dst.Type = header.Type
	dst.ID = header.ID
	dst.TS = header.TS

	// Strip the envelope-level fields from the payload map so
	// the typed re-decode in reMarshalInto only sees the
	// payload's own fields. This matters when the payload type
	// has its own `type` field (none in 1.5, but the dispatch
	// loop in 1.6 has `exec`, `read`, `write` \u2014 we don't want
	// the envelope's outer "type" colliding with their inner
	// schema).
	delete(header.Rest, "type")
	delete(header.Rest, "id")
	delete(header.Rest, "ts")
	dst.Payload = header.Rest
	return nil
}
