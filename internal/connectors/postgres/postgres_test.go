package postgres

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewConfigValidation(t *testing.T) {
	cases := []struct{ name, cfg, wantErr string }{
		{"missing host", `{"database":"d","user":"u","password":"p"}`, "host"},
		{"missing database", `{"host":"h","user":"u","password":"p"}`, "database"},
		{"missing creds", `{"host":"h","database":"d"}`, "user + password"},
		{"not json", `nope`, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(json.RawMessage(tc.cfg))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
	// Valid config prepares a lazy pool without connecting (default port + sslmode).
	if _, err := New(json.RawMessage(`{"host":"127.0.0.1","database":"app","user":"u","password":"p","sslmode":"disable"}`)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
