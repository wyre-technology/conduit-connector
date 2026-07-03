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

// ConfigHandler applies a cloud-pushed config_update and returns the applied
// connector slugs (⊆ pushed slugs — the relay rejects over-claims). The
// returned error becomes the ack's error field; applied is reported either way.
type ConfigHandler func(ctx context.Context, configVersion int, connectors map[string]json.RawMessage) (applied []string, err error)

// Options configures a Client. The agent speaks protocol v2: enrollment is
// identity-only and capabilities arrive via cloud-pushed config_update.
type Options struct {
	// RelayURL is the WYRE relay WSS endpoint, e.g. wss://conduit-wss.wyre.ai.
	// Must be wss:// — enforced by the caller's boot guards.
	RelayURL string
	// EnrollmentToken is the per-tunnel signed identity-only token.
	EnrollmentToken string
	// OnRequest handles inbound request frames.
	OnRequest Handler
	// OnConfigUpdate applies cloud-pushed connector config.
	OnConfigUpdate ConfigHandler
	// HeartbeatInterval defaults to 30s.
	HeartbeatInterval time.Duration
	// MaxBackoff caps reconnect backoff; defaults to 30s.
	MaxBackoff time.Duration
	Logger     *slog.Logger
}

const baseBackoff = time.Second

// Client maintains the outbound tunnel: dial, register, heartbeat, dispatch,
// config apply/ack, reconnect-with-backoff. An identity register_nack stops
// the client permanently (the operator must fix the token); a
// transient_unavailable nack retries with backoff.
type Client struct {
	opts    Options
	log     *slog.Logger
	backoff time.Duration

	mu       sync.Mutex
	tunnelID string
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
	send := func(frame any) error {
		b, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	if err := send(Register(c.opts.EnrollmentToken)); err != nil {
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
			c.backoff = baseBackoff // a clean registration resets backoff
			c.log.Info("tunnel registered (v2, zero capabilities until config push)", "tunnelId", frame.TunnelID)
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

		case "config_update":
			go c.handleConfigUpdate(ctx, frame, send)

		case "heartbeat":
			// Relay-originated heartbeats are ignored.
		}
	}
}

func (c *Client) handleRequest(ctx context.Context, frame *Frame, send func(any) error) {
	payload, err := c.opts.OnRequest(ctx, frame.Target, frame.Payload)
	var out any
	if err != nil {
		out = ErrorResponse(frame.CorrelationID, err.Error())
	} else {
		out = Response(frame.CorrelationID, payload)
	}
	if err := send(out); err != nil {
		c.log.Warn("failed to send response frame", "correlationId", frame.CorrelationID, "error", err)
	}
}

func (c *Client) handleConfigUpdate(ctx context.Context, frame *Frame, send func(any) error) {
	version := *frame.ConfigVersion // ParseFrame guarantees non-nil for config_update
	applied, err := c.opts.OnConfigUpdate(ctx, version, frame.Connectors)
	var ackErr *FrameError
	if err != nil {
		ackErr = &FrameError{Code: -32000, Message: err.Error()}
	}
	c.log.Info("config applied", "configVersion", version, "applied", applied, "error", err)
	if sendErr := send(ConfigAck(frame.CorrelationID, version, applied, ackErr)); sendErr != nil {
		c.log.Warn("failed to send config_ack", "correlationId", frame.CorrelationID, "error", sendErr)
	}
}

func (c *Client) heartbeatLoop(ctx context.Context, send func(any) error) {
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
