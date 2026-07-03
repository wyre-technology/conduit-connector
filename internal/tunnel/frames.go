// Package tunnel implements the Conduit on-prem tunnel protocol (frame v1):
// a faithful port of conduit's src/relay/frame-protocol.ts. The wire format
// is JSON frames over a single outbound WSS connection.
//
// Validation is deliberately strict — the tunnel is a trust boundary, and a
// frame that does not parse cleanly is never executed (mirrors parseFrame's
// null-on-anything-malformed contract in the TS source).
package tunnel

import (
	"encoding/json"
	"fmt"
)

const protocolVersion = 1

// Frame is one protocol message. A single struct covers all six frame types;
// Type discriminates, and Validate enforces the per-type required fields the
// TS parseFrame checks.
type Frame struct {
	Type            string          `json:"type"`
	V               int             `json:"v"`
	EnrollmentToken string          `json:"enrollmentToken,omitempty"`
	Capabilities    []string        `json:"capabilities,omitempty"`
	TunnelID        string          `json:"tunnelId,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	CorrelationID   string          `json:"correlationId,omitempty"`
	Target          string          `json:"target,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	Error           *FrameError     `json:"error,omitempty"`
}

// FrameError is the response-frame error object ({code, message}).
type FrameError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ParseFrame parses and validates a raw WSS message. It returns an error on
// anything malformed; callers treat that as a protocol violation and drop the
// connection (per the TS client's close-on-unparseable behavior).
func ParseFrame(raw []byte) (*Frame, error) {
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("frame not valid JSON: %w", err)
	}
	if f.V != protocolVersion {
		return nil, fmt.Errorf("unsupported protocol version %d", f.V)
	}
	switch f.Type {
	case "register_ack":
		if f.TunnelID == "" {
			return nil, fmt.Errorf("register_ack missing tunnelId")
		}
	case "register_nack":
		switch f.Reason {
		case "invalid_identity", "revoked_identity", "malformed":
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
	default:
		// register/response only travel connector→relay; anything else is a
		// violation. The relay legitimately sends only ack/nack/request(/heartbeat).
		return nil, fmt.Errorf("unexpected frame type %q from relay", f.Type)
	}
	return &f, nil
}

// Register builds the first frame sent after the WSS opens.
func Register(enrollmentToken string, capabilities []string) Frame {
	return Frame{Type: "register", V: protocolVersion, EnrollmentToken: enrollmentToken, Capabilities: capabilities}
}

// Heartbeat builds the periodic liveness frame.
func Heartbeat() Frame {
	return Frame{Type: "heartbeat", V: protocolVersion}
}

// Response builds a success response for a handled request frame.
func Response(correlationID string, payload json.RawMessage) Frame {
	return Frame{Type: "response", V: protocolVersion, CorrelationID: correlationID, Payload: payload}
}

// ErrorResponse builds an error response (code -32000 mirrors the TS client's
// on-prem handler error shape).
func ErrorResponse(correlationID string, message string) Frame {
	return Frame{
		Type: "response", V: protocolVersion, CorrelationID: correlationID,
		Error: &FrameError{Code: -32000, Message: message},
	}
}
