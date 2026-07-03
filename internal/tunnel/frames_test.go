package tunnel

import (
	"encoding/json"
	"testing"
)

func TestParseFrameStrictness(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"register_ack valid", `{"type":"register_ack","v":2,"tunnelId":"t1"}`, false},
		{"register_ack v1 valid", `{"type":"register_ack","v":1,"tunnelId":"t1"}`, false},
		{"register_ack missing tunnelId", `{"type":"register_ack","v":2}`, true},
		{"register_nack valid", `{"type":"register_nack","v":2,"reason":"invalid_identity"}`, false},
		{"register_nack transient", `{"type":"register_nack","v":2,"reason":"transient_unavailable"}`, false},
		{"register_nack unknown reason", `{"type":"register_nack","v":2,"reason":"nope"}`, true},
		{"heartbeat valid", `{"type":"heartbeat","v":2}`, false},
		{"request valid", `{"type":"request","v":2,"correlationId":"c1","target":"echo","payload":{}}`, false},
		{"request missing target", `{"type":"request","v":2,"correlationId":"c1","payload":{}}`, true},
		{"request missing payload", `{"type":"request","v":2,"correlationId":"c1","target":"echo"}`, true},
		{"config_update valid", `{"type":"config_update","v":2,"correlationId":"c1","configVersion":1,"connectors":{"echo":{}}}`, false},
		{"config_update v1 rejected", `{"type":"config_update","v":1,"correlationId":"c1","configVersion":1,"connectors":{}}`, true},
		{"config_update missing configVersion", `{"type":"config_update","v":2,"correlationId":"c1","connectors":{}}`, true},
		{"config_update negative configVersion", `{"type":"config_update","v":2,"correlationId":"c1","configVersion":-1,"connectors":{}}`, true},
		{"config_update missing connectors", `{"type":"config_update","v":2,"correlationId":"c1","configVersion":1}`, true},
		{"unknown version", `{"type":"heartbeat","v":3}`, true},
		{"unknown type", `{"type":"mystery","v":2}`, true},
		{"relay must not send register", `{"type":"register","v":2,"enrollmentToken":"x","capabilities":[]}`, true},
		{"relay must not send config_ack", `{"type":"config_ack","v":2,"correlationId":"c","configVersion":1,"applied":[]}`, true},
		{"not json", `{{{`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFrame([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseFrame(%s) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

// The relay's parseFrame requires array fields to BE arrays — Go omitempty
// drops empty slices, which would read as a protocol violation and drop the
// socket. These tests pin the wire shape.
func TestOutboundWireShapes(t *testing.T) {
	t.Run("register carries capabilities: [] (v2 identity-only)", func(t *testing.T) {
		b, err := json.Marshal(Register("tok"))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		caps, ok := m["capabilities"].([]any)
		if !ok {
			t.Fatalf("register frame must carry capabilities as an array, got: %s", b)
		}
		if len(caps) != 0 {
			t.Fatalf("v2 register must carry EMPTY capabilities, got: %s", b)
		}
		if m["v"] != float64(2) {
			t.Fatalf("register must be v2, got: %s", b)
		}
	})

	t.Run("config_ack carries applied: [] even when nothing applied", func(t *testing.T) {
		b, err := json.Marshal(ConfigAck("c1", 3, nil, &FrameError{Code: -32000, Message: "boom"}))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		applied, ok := m["applied"].([]any)
		if !ok || len(applied) != 0 {
			t.Fatalf("config_ack must carry applied as a (possibly empty) array, got: %s", b)
		}
		if m["configVersion"] != float64(3) {
			t.Fatalf("config_ack must echo configVersion, got: %s", b)
		}
	})

	t.Run("error response wire shape", func(t *testing.T) {
		b, _ := json.Marshal(ErrorResponse("c9", "boom"))
		f := struct {
			Error *FrameError `json:"error"`
		}{}
		if err := json.Unmarshal(b, &f); err != nil || f.Error == nil || f.Error.Code != -32000 {
			t.Fatalf("error response wire shape wrong: %s", b)
		}
	})
}
