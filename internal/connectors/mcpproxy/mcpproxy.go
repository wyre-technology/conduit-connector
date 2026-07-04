// Package mcpproxy is a generic connector that proxies to a LOCAL MCP server
// spoken over stdio — so any existing MCP server (e.g. the Veeam MCP servers)
// can be reached over the tunnel without rewriting it as a built-in connector.
//
// The connector spawns the configured command as a child process, performs the
// MCP initialize handshake once, and forwards each inbound tunnel request
// (initialize / tools/list / tools/call) to the child, returning the matching
// JSON-RPC response. The child's stdout is the MCP message stream (newline-
// delimited JSON per the stdio transport); its stderr is captured to the
// connector log.
//
// Config (pushed via config_update, held in memory only):
//
//	{ "command": "node",
//	  "args": ["/opt/vbr-mcp/dist/server.js"],
//	  "env": { "VBR_HOST": "10.0.0.9", "VBR_USERNAME": "svc", "VBR_PASSWORD": "..." },
//	  "cwd": "/opt/vbr-mcp" }
//
// Trust note: this runs an operator-configured local command. The config is
// authored in Conduit by an org admin and pushed over TLS — the connector does
// not accept a command from an untrusted source. The child runs with the
// connector's own (unprivileged, DynamicUser) service identity.
package mcpproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

const (
	requestTimeout    = 30 * time.Second
	initializeTimeout = 20 * time.Second
)

// Config is the pushed per-connector config.
type Config struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
}

// Connector proxies to one local MCP server. Requests are serialized over the
// child's stdio pipe (one in-flight at a time — simple and correct for a
// connector's throughput).
type Connector struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	child *child // nil until first request / after the child dies
}

// New validates the config and prepares the connector. The child process is
// spawned lazily on the first request, so a mis-set command does not block
// config application — the first tool call surfaces the spawn error.
func New(raw json.RawMessage, log *slog.Logger) (*Connector, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("mcp-proxy config is not valid JSON: %w", err)
	}
	if cfg.Command == "" {
		return nil, fmt.Errorf("mcp-proxy config requires a `command` to run the local MCP server")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Connector{cfg: cfg, log: log}, nil
}

// Handle forwards one MCP JSON-RPC request to the local server and returns its
// response. Notifications from the request layer (no id) are not expected from
// the tunnel, but if present are answered with an empty object.
func (c *Connector) Handle(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.child == nil {
		ch, err := c.spawn(ctx)
		if err != nil {
			return nil, fmt.Errorf("mcp-proxy: failed to start local MCP server (%s): %w", c.cfg.Command, err)
		}
		c.child = ch
	}

	// Extract the request id so we can match the response (and skip any
	// interleaved notifications the server emits).
	var idProbe struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(payload, &idProbe)

	resp, err := c.child.roundTrip(ctx, payload, idProbe.ID, requestTimeout)
	if err != nil {
		// The child is likely dead / desynced — drop it so the next request
		// respawns a clean process.
		c.child.close()
		c.child = nil
		return nil, fmt.Errorf("mcp-proxy: local MCP server request failed: %w", err)
	}
	return resp, nil
}

// child is a live MCP-over-stdio subprocess.
type child struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func (c *Connector) spawn(ctx context.Context) (*child, error) {
	cmd := exec.Command(c.cfg.Command, c.cfg.Args...) //nolint:gosec // operator-configured command, run as the connector's unprivileged identity
	cmd.Dir = c.cfg.Cwd
	cmd.Env = flattenEnv(c.cfg.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Drain stderr to the connector log so the child's diagnostics are visible.
	go func() {
		s := bufio.NewScanner(stderrPipe)
		for s.Scan() {
			c.log.Debug("mcp-proxy child stderr", "command", c.cfg.Command, "line", s.Text())
		}
	}()

	ch := &child{cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdoutPipe, 1<<20)}

	// MCP handshake: initialize, then the initialized notification. Without
	// this the server rejects tools/* calls.
	if err := ch.initialize(ctx); err != nil {
		ch.close()
		return nil, fmt.Errorf("initialize handshake failed: %w", err)
	}
	c.log.Info("mcp-proxy: local MCP server started", "command", c.cfg.Command)
	return ch, nil
}

func (ch *child) initialize(ctx context.Context) error {
	initReq := json.RawMessage(`{"jsonrpc":"2.0","id":"conduit-init","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"conduit-connector","version":"1.0.0"}}}`)
	if _, err := ch.roundTrip(ctx, initReq, json.RawMessage(`"conduit-init"`), initializeTimeout); err != nil {
		return err
	}
	// initialized notification (no id, no response expected).
	return ch.write(json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
}

// roundTrip writes a request and reads until the response whose id matches
// wantID (skipping notifications / unrelated messages). wantID may be null for
// requests without an id, in which case the first response-shaped message is
// returned.
func (ch *child) roundTrip(ctx context.Context, req json.RawMessage, wantID json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	if err := ch.write(req); err != nil {
		return nil, err
	}

	type readResult struct {
		line []byte
		err  error
	}
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for response")
		}
		// Read one line with a bounded wait so ctx cancellation is honored.
		lineCh := make(chan readResult, 1)
		go func() {
			line, err := ch.stdout.ReadBytes('\n')
			lineCh <- readResult{line, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-lineCh:
			if r.err != nil && len(r.line) == 0 {
				return nil, fmt.Errorf("child closed stdout: %w", r.err)
			}
			trimmed := trimSpace(r.line)
			if len(trimmed) == 0 {
				continue
			}
			var probe struct {
				ID     json.RawMessage `json:"id"`
				Method *string         `json:"method"`
			}
			if err := json.Unmarshal(trimmed, &probe); err != nil {
				continue // not JSON we understand; skip
			}
			// A message with a method and no id is a notification — skip.
			if probe.Method != nil && len(probe.ID) == 0 {
				continue
			}
			if idMatches(probe.ID, wantID) {
				out := make([]byte, len(trimmed))
				copy(out, trimmed)
				return out, nil
			}
			// A response to a different id — skip (shouldn't happen with our
			// serialized round-trips, but be robust).
		}
	}
}

func (ch *child) write(msg json.RawMessage) error {
	if _, err := ch.stdin.Write(append([]byte(msg), '\n')); err != nil {
		return err
	}
	return nil
}

func (ch *child) close() {
	if ch == nil || ch.cmd == nil {
		return
	}
	_ = ch.stdin.Close()
	if ch.cmd.Process != nil {
		_ = ch.cmd.Process.Kill()
	}
	_ = ch.cmd.Wait()
}

func flattenEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil // inherit the connector's environment
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func idMatches(a, b json.RawMessage) bool {
	// Compare the raw JSON of the two ids. null/absent both render empty.
	as, bs := string(trimSpace(a)), string(trimSpace(b))
	if as == "" {
		as = "null"
	}
	if bs == "" {
		bs = "null"
	}
	return as == bs
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
