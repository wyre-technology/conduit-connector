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
		{"register_ack valid", `{"type":"register_ack","v":1,"tunnelId":"t1"}`, false},
		{"register_ack missing tunnelId", `{"type":"register_ack","v":1}`, true},
		{"register_nack valid", `{"type":"register_nack","v":1,"reason":"invalid_identity"}`, false},
		{"register_nack unknown reason", `{"type":"register_nack","v":1,"reason":"nope"}`, true},
		{"heartbeat valid", `{"type":"heartbeat","v":1}`, false},
		{"request valid", `{"type":"request","v":1,"correlationId":"c1","target":"echo","payload":{}}`, false},
		{"request missing target", `{"type":"request","v":1,"correlationId":"c1","payload":{}}`, true},
		{"request missing payload", `{"type":"request","v":1,"correlationId":"c1","target":"echo"}`, true},
		{"wrong version", `{"type":"heartbeat","v":2}`, true},
		{"unknown type", `{"type":"mystery","v":1}`, true},
		{"relay must not send register", `{"type":"register","v":1,"enrollmentToken":"x","capabilities":[]}`, true},
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

func TestFrameRoundTrip(t *testing.T) {
	reg := Register("tok", []string{"echo"})
	b, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	// The register frame must serialize with the exact field names the TS
	// relay validates (frame-protocol.ts parseFrame).
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"type", "v", "enrollmentToken", "capabilities"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("register frame missing wire field %q: %s", k, b)
		}
	}

	resp := ErrorResponse("c9", "boom")
	b, _ = json.Marshal(resp)
	f := struct {
		Error *FrameError `json:"error"`
	}{}
	if err := json.Unmarshal(b, &f); err != nil || f.Error == nil || f.Error.Code != -32000 {
		t.Fatalf("error response wire shape wrong: %s", b)
	}
}
