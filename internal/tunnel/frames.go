// Package tunnel implements the Conduit on-prem tunnel protocol — a faithful
// port of conduit's src/relay/frame-protocol.ts. The wire format is JSON
// frames over a single outbound WSS connection.
//
// PROTOCOL VERSIONS (conduit docs/onprem-connector-v1.md, M-A):
//
//	v1 — M1 wire protocol: the enrollment token carries the capability grant.
//	v2 — connector-v1 protocol: enrollment is identity-only; capabilities are
//	     cloud-managed config delivered via config_update/config_ack. This
//	     agent registers at v2.
//
// Validation is deliberately strict — the tunnel is a trust boundary, and a
// frame that does not parse cleanly is never executed (mirrors parseFrame's
// null-on-anything-malformed contract in the TS source).
package tunnel

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the wire version this agent speaks.
const ProtocolVersion = 2

// Frame is one protocol message. A single struct covers all frame types;
// Type discriminates, and ParseFrame enforces the per-type required fields
// the TS parseFrame checks.
type Frame struct {
	Type            string                     `json:"type"`
	V               int                        `json:"v"`
	EnrollmentToken string                     `json:"enrollmentToken,omitempty"`
	Capabilities    []string                   `json:"capabilities,omitempty"`
	TunnelID        string                     `json:"tunnelId,omitempty"`
	Reason          string                     `json:"reason,omitempty"`
	CorrelationID   string                     `json:"correlationId,omitempty"`
	Target          string                     `json:"target,omitempty"`
	Payload         json.RawMessage            `json:"payload,omitempty"`
	Error           *FrameError                `json:"error,omitempty"`
	ConfigVersion   *int                       `json:"configVersion,omitempty"`
	Connectors      map[string]json.RawMessage `json:"connectors,omitempty"`
	Applied         []string                   `json:"applied,omitempty"`
}

// FrameError is the response/ack error object ({code, message}).
type FrameError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ParseFrame parses and validates a raw WSS message from the relay. It
// returns an error on anything malformed; callers treat that as a protocol
// violation and drop the connection (per the TS client's
// close-on-unparseable behavior).
func ParseFrame(raw []byte) (*Frame, error) {
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("frame not valid JSON: %w", err)
	}
	if f.V != 1 && f.V != 2 {
		return nil, fmt.Errorf("unsupported protocol version %d", f.V)
	}
	switch f.Type {
	case "register_ack":
		if f.TunnelID == "" {
			return nil, fmt.Errorf("register_ack missing tunnelId")
		}
	case "register_nack":
		switch f.Reason {
		case "invalid_identity", "revoked_identity", "malformed", "transient_unavailable":
		default:
			return nil, fmt.Errorf("register_nack with unknown reason %q", f.Reason)
		}
	case "heartbeat":
	case "request":
		if f.CorrelationID == "" {
			return nil, fmt.Errorf("request missing correlationId")
		}
		if f.Target == "" {
			return nil, fmt.Errorf("request missing target")
		}
		if f.Payload == nil {
			return nil, fmt.Errorf("request missing payload")
		}
	case "config_update":
		if f.V != 2 {
			return nil, fmt.Errorf("config_update is v2-only, got v%d", f.V)
		}
		if f.CorrelationID == "" {
			return nil, fmt.Errorf("config_update missing correlationId")
		}
		if f.ConfigVersion == nil || *f.ConfigVersion < 0 {
			return nil, fmt.Errorf("config_update missing/invalid configVersion")
		}
		if f.Connectors == nil {
			return nil, fmt.Errorf("config_update missing connectors")
		}
	default:
		// register/response/config_ack only travel connector→relay; anything
		// else is a violation.
		return nil, fmt.Errorf("unexpected frame type %q from relay", f.Type)
	}
	return &f, nil
}

// Outbound frames use dedicated shapes WITHOUT omitempty on array fields:
// the relay's parseFrame requires `capabilities` (register) and `applied`
// (config_ack) to BE arrays — Go's omitempty drops empty slices entirely,
// which the relay treats as a protocol violation and drops the socket.

type registerFrame struct {
	Type            string   `json:"type"`
	V               int      `json:"v"`
	EnrollmentToken string   `json:"enrollmentToken"`
	Capabilities    []string `json:"capabilities"`
}

type heartbeatFrame struct {
	Type string `json:"type"`
	V    int    `json:"v"`
}

type responseFrame struct {
	Type          string          `json:"type"`
	V             int             `json:"v"`
	CorrelationID string          `json:"correlationId"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Error         *FrameError     `json:"error,omitempty"`
}

type configAckFrame struct {
	Type          string      `json:"type"`
	V             int         `json:"v"`
	CorrelationID string      `json:"correlationId"`
	ConfigVersion int         `json:"configVersion"`
	Applied       []string    `json:"applied"`
	Error         *FrameError `json:"error,omitempty"`
}

// Register builds the first frame sent after the WSS opens. v2: the token is
// identity-only and capabilities MUST be [] — the relay nacks `malformed`
// otherwise; capabilities arrive via config_update.
func Register(enrollmentToken string) any {
	return registerFrame{Type: "register", V: ProtocolVersion, EnrollmentToken: enrollmentToken, Capabilities: []string{}}
}

// Heartbeat builds the periodic liveness frame.
func Heartbeat() any {
	return heartbeatFrame{Type: "heartbeat", V: ProtocolVersion}
}

// Response builds a success response for a handled request frame.
func Response(correlationID string, payload json.RawMessage) any {
	return responseFrame{Type: "response", V: ProtocolVersion, CorrelationID: correlationID, Payload: payload}
}

// ErrorResponse builds an error response (code -32000 mirrors the TS client's
// on-prem handler error shape).
func ErrorResponse(correlationID string, message string) any {
	return responseFrame{
		Type: "response", V: ProtocolVersion, CorrelationID: correlationID,
		Error: &FrameError{Code: -32000, Message: message},
	}
}

// ConfigAck reports the applied connector set for a config_update. The cloud
// writes the tunnel's registry capabilities from this ack — `applied` MUST be
// a subset of the pushed connector slugs or the relay rejects the push.
func ConfigAck(correlationID string, configVersion int, applied []string, ackErr *FrameError) any {
	if applied == nil {
		applied = []string{}
	}
	return configAckFrame{
		Type: "config_ack", V: ProtocolVersion, CorrelationID: correlationID,
		ConfigVersion: configVersion, Applied: applied, Error: ackErr,
	}
}
