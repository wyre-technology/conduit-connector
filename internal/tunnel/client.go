package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Handler processes one inbound request frame's payload for a capability
// target and returns the JSON-RPC-shaped response payload.
type Handler func(ctx context.Context, target string, payload json.RawMessage) (json.RawMessage, error)

// Options configures a Client. Semantics mirror conduit's TS TunnelClient.
type Options struct {
	// RelayURL is the WYRE relay WSS endpoint, e.g. wss://conduit-wss.wyre.ai.
	// Must be wss:// — enforced by the caller's boot guards.
	RelayURL string
	// EnrollmentToken is the per-tunnel signed identity (v1).
	EnrollmentToken string
	// Capabilities this connector offers, byte-for-byte slugs.
	Capabilities []string
	// OnRequest handles inbound request frames.
	OnRequest Handler
	// HeartbeatInterval defaults to 30s.
	HeartbeatInterval time.Duration
	// MaxBackoff caps reconnect backoff; defaults to 30s.
	MaxBackoff time.Duration
	Logger     *slog.Logger
}

const baseBackoff = time.Second

// Client maintains the outbound tunnel: dial, register, heartbeat, dispatch,
// reconnect-with-backoff. A register_nack stops the client permanently (the
// operator must fix the token; reconnecting cannot help — TS parity).
type Client struct {
	opts    Options
	log     *slog.Logger
	backoff time.Duration

	mu       sync.Mutex
	tunnelID string
	stopped  bool
}

func NewClient(opts Options) *Client {
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 30 * time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{opts: opts, log: opts.Logger, backoff: baseBackoff}
}

// TunnelID returns the relay-assigned tunnel id, or "" until registered.
func (c *Client) TunnelID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tunnelID
}

// Run dials and serves until ctx is cancelled or the relay nacks the
// identity. It reconnects with exponential backoff on any other disconnect.
func (c *Client) Run(ctx context.Context) error {
	for {
		err := c.dialAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errNacked) {
			return err
		}
		c.log.Warn("tunnel disconnected; reconnecting", "backoff", c.backoff.String(), "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.backoff):
		}
		c.backoff = min(c.backoff*2, c.opts.MaxBackoff)
	}
}

var errNacked = errors.New("relay rejected identity (register_nack); fix the enrollment token and restart")

func (c *Client) dialAndServe(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, c.opts.RelayURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "shutting down")
	conn.SetReadLimit(16 << 20) // MCP payloads can be large; 16 MiB ceiling.

	// One writer at a time: heartbeat ticker + concurrent request handlers.
	var writeMu sync.Mutex
	send := func(f Frame) error {
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	if err := send(Register(c.opts.EnrollmentToken, c.opts.Capabilities)); err != nil {
		return err
	}

	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			c.setTunnelID("")
			return err
		}
		frame, err := ParseFrame(raw)
		if err != nil {
			// Protocol violation from the relay — drop the socket, reconnect.
			c.setTunnelID("")
			return err
		}

		switch frame.Type {
		case "register_ack":
			c.setTunnelID(frame.TunnelID)
			c.backoff = baseBackoff // clean registration resets backoff (TS parity)
			c.log.Info("tunnel registered", "tunnelId", frame.TunnelID, "capabilities", c.opts.Capabilities)
			go c.heartbeatLoop(hbCtx, send)

		case "register_nack":
			if frame.Reason == "transient_unavailable" {
				// Relay-side transient failure (503-equivalent) — not an
				// identity problem; reconnect-with-backoff will succeed once
				// the relay recovers.
				c.log.Warn("relay transiently unavailable at registration; will retry")
				return errors.New("relay transient_unavailable at registration")
			}
			c.log.Error("relay rejected registration", "reason", frame.Reason)
			return errNacked

		case "request":
			go c.handleRequest(ctx, frame, send)

		case "heartbeat":
			// Relay-originated heartbeats are ignored (TS parity: only
			// ack/nack/request are acted on).
		}
	}
}

func (c *Client) handleRequest(ctx context.Context, frame *Frame, send func(Frame) error) {
	payload, err := c.opts.OnRequest(ctx, frame.Target, frame.Payload)
	var out Frame
	if err != nil {
		out = ErrorResponse(frame.CorrelationID, err.Error())
	} else {
		out = Response(frame.CorrelationID, payload)
	}
	if err := send(out); err != nil {
		c.log.Warn("failed to send response frame", "correlationId", frame.CorrelationID, "error", err)
	}
}

func (c *Client) heartbeatLoop(ctx context.Context, send func(Frame) error) {
	ticker := time.NewTicker(c.opts.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := send(Heartbeat()); err != nil {
				return // socket is going down; the read loop drives reconnect
			}
		}
	}
}

func (c *Client) setTunnelID(id string) {
	c.mu.Lock()
	c.tunnelID = id
	c.mu.Unlock()
}
