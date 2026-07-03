// Package echo is the tunnel-proof payload — a faithful port of conduit's
// src/onprem/echo-mcp-server.ts. It speaks just enough MCP JSON-RPC
// (initialize, tools/list, tools/call for `echo`) to prove the full
// gateway→relay→tunnel→connector round-trip without touching any on-prem
// resource or credential.
package echo

import (
	"encoding/json"
	"fmt"
)

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string `json:"name"`
		Arguments struct {
			Message json.RawMessage `json:"message"`
		} `json:"arguments"`
	} `json:"params"`
}

// Handle processes one MCP JSON-RPC request against the echo server.
func Handle(payload json.RawMessage) (json.RawMessage, error) {
	var req rpcRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("echo: payload is not JSON-RPC shaped: %w", err)
	}
	id := req.ID
	if id == nil {
		id = json.RawMessage("null")
	}

	switch req.Method {
	case "initialize":
		return marshal(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "onprem-echo", "version": "1.0.0"},
			},
		})

	case "tools/list":
		return marshal(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{"tools": []any{map[string]any{
				"name":        "echo",
				"description": "Returns its input unchanged. The M1 tunnel-proof payload.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"message": map[string]any{"type": "string", "description": "Text to echo back"},
					},
					"required": []string{"message"},
				},
			}}},
		})

	case "tools/call":
		if req.Params.Name != "echo" {
			return marshal(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": -32601, "message": "Unknown tool: " + req.Params.Name},
			})
		}
		var message string
		if err := json.Unmarshal(req.Params.Arguments.Message, &message); err != nil {
			return marshal(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": -32602, "message": "echo requires a string `message` argument"},
			})
		}
		return marshal(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}},
		})

	default:
		return marshal(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": -32601, "message": "Method not found: " + req.Method},
		})
	}
}

func marshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}
