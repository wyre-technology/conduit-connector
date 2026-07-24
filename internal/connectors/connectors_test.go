package connectors

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// echoCall drives a tools/call against a slug and returns the echoed text.
func echoCall(t *testing.T, r *Registry, slug, msg string) (json.RawMessage, error) {
	t.Helper()
	payload := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"message":` + mustJSONString(msg) + `}}}`)
	return r.Handle(context.Background(), slug, payload)
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestApplySlugIsTypeByDefault(t *testing.T) {
	r := NewRegistry()
	applied, err := r.Apply(context.Background(), 1, map[string]json.RawMessage{
		"echo": json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 1 || applied[0] != "echo" {
		t.Fatalf("applied = %v, want [echo]", applied)
	}
	if _, err := echoCall(t, r, "echo", "hi"); err != nil {
		t.Fatalf("echo handle: %v", err)
	}
}

func TestApplyNamedInstancesViaType(t *testing.T) {
	r := NewRegistry()
	// Two named connectors backed by the same built-in ("echo"), plus one
	// legacy slug-is-type connector — all three must apply and route
	// independently by their slug.
	applied, err := r.Apply(context.Background(), 1, map[string]json.RawMessage{
		"greeter":  json.RawMessage(`{"type":"echo"}`),
		"farewell": json.RawMessage(`{"type":"echo"}`),
		"echo":     json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 3 {
		t.Fatalf("applied = %v, want 3 connectors", applied)
	}
	for _, slug := range []string{"greeter", "farewell", "echo"} {
		resp, err := echoCall(t, r, slug, "via-"+slug)
		if err != nil {
			t.Fatalf("handle %q: %v", slug, err)
		}
		if !strings.Contains(string(resp), "via-"+slug) {
			t.Fatalf("slug %q did not route to its own instance: %s", slug, resp)
		}
	}
}

func TestApplyUnknownTypeAndSlugErrors(t *testing.T) {
	r := NewRegistry()
	// Unknown explicit type names the type in the error.
	_, err := r.Apply(context.Background(), 1, map[string]json.RawMessage{
		"x": json.RawMessage(`{"type":"nonesuch"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "nonesuch") {
		t.Fatalf("want unknown-type error naming the type, got %v", err)
	}
	// Unknown slug (no type) keeps the original message.
	_, err = r.Apply(context.Background(), 1, map[string]json.RawMessage{
		"nope": json.RawMessage(`{}`),
	})
	if err == nil || !strings.Contains(err.Error(), "no built-in connector") {
		t.Fatalf("want no-built-in-connector error, got %v", err)
	}
}

func TestApplyHTTPBridge(t *testing.T) {
	r := NewRegistry()
	applied, err := r.Apply(context.Background(), 1, map[string]json.RawMessage{
		"http-bridge": json.RawMessage(`{"hosts":[{"baseUrl":"https://cw.example.local"}]}`),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 1 || applied[0] != "http-bridge" {
		t.Fatalf("applied = %v", applied)
	}
	// tools/list must be empty — the bridge exposes no user-facing tools.
	out, err := r.Handle(context.Background(), "http-bridge",
		json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(string(out), `"tools":[]`) {
		t.Errorf("expected empty tools, got %s", out)
	}
}

func TestApplyHTTPBridgeRejectsBadConfig(t *testing.T) {
	r := NewRegistry()
	applied, err := r.Apply(context.Background(), 1, map[string]json.RawMessage{
		"http-bridge": json.RawMessage(`{"hosts":[{"baseUrl":"not-a-url"}]}`),
	})
	if err == nil {
		t.Fatal("expected config rejection error")
	}
	if len(applied) != 0 {
		t.Fatalf("bad config must not apply, got %v", applied)
	}
}
