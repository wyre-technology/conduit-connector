package mcpproxy

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// A minimal MCP-over-stdio server: responds to initialize, tools/list, and
// tools/call; ignores notifications. Used as the proxied child.
const fakeServer = `
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if 'id' not in msg:   # notification
        continue
    m = msg.get('method')
    if m == 'initialize':
        r = {'jsonrpc':'2.0','id':msg['id'],'result':{'protocolVersion':'2024-11-05','capabilities':{},'serverInfo':{'name':'fake','version':'1'}}}
    elif m == 'tools/list':
        r = {'jsonrpc':'2.0','id':msg['id'],'result':{'tools':[{'name':'ping','description':'p','inputSchema':{'type':'object'}}]}}
    elif m == 'tools/call':
        r = {'jsonrpc':'2.0','id':msg['id'],'result':{'content':[{'type':'text','text':'pong'}]}}
    else:
        r = {'jsonrpc':'2.0','id':msg['id'],'error':{'code':-32601,'message':'no'}}
    sys.stdout.write(json.dumps(r)+'\n'); sys.stdout.flush()
`

func TestConfigValidation(t *testing.T) {
	if _, err := New(json.RawMessage(`nope`), nil); err == nil {
		t.Fatal("expected error on non-JSON config")
	}
	if _, err := New(json.RawMessage(`{"args":["x"]}`), nil); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("expected missing-command error, got %v", err)
	}
	if _, err := New(json.RawMessage(`{"command":"node"}`), nil); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestProxyRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	cfg := `{"command":"python3","args":["-c",` + mustJSON(fakeServer) + `]}`
	c, err := New(json.RawMessage(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}

	// tools/list is forwarded to the child (which requires the internal
	// initialize handshake to have run first — proven by this succeeding).
	list, err := c.Handle(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if !strings.Contains(string(list), `"ping"`) {
		t.Fatalf("expected ping tool, got %s", list)
	}
	// The response id must echo the request id we sent (1).
	var parsed struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(list, &parsed); err != nil || parsed.ID != 1 {
		t.Fatalf("response id mismatch (want 1): %s", list)
	}

	call, err := c.Handle(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping","arguments":{}}}`))
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if !strings.Contains(string(call), "pong") {
		t.Fatalf("expected pong, got %s", call)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
