// Package httpbridge forwards HTTP requests arriving over the tunnel as
// `http/forward` JSON-RPC payloads to LAN hosts on a cloud-pushed allowlist.
// It is the generic last-mile hop that lets CLOUD vendor MCP containers reach
// on-prem servers (ConnectWise Manage/Automate, IT Glue, ...) without running
// any vendor code on the customer box.
//
// Config (pushed per-site):
//
//	{
//	  "hosts": [
//	    {"baseUrl": "https://cw.customer.local",
//	     "caCertPem": "-----BEGIN CERTIFICATE-----...",   // optional private CA
//	     "insecureSkipVerify": false}                      // optional, last resort
//	  ]
//	}
//
// The allowlist is the security boundary: a request whose URL does not match
// a configured host (scheme + host + port + segment-bounded path prefix) is
// refused, so the tunnel can never be used as a general LAN proxy.
package httpbridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// HostConfig is one allowlisted LAN base URL plus its TLS trust settings.
type HostConfig struct {
	BaseURL string `json:"baseUrl"`
	// CACertPem holds one or more PEM certificates to trust for this host
	// (private CA or self-signed server cert). Empty = system roots.
	CACertPem string `json:"caCertPem"`
	// InsecureSkipVerify disables TLS verification for this host. Last
	// resort for appliances with unexportable certs; prefer CACertPem.
	InsecureSkipVerify bool `json:"insecureSkipVerify"`
}

// Config is the pushed per-site connector config.
type Config struct {
	Hosts []HostConfig `json:"hosts"`
}

type hostEntry struct {
	base   *url.URL
	client *http.Client
}

// Connector forwards allowlisted HTTP requests. Safe for concurrent use.
type Connector struct {
	hosts []hostEntry
}

// New parses config and prepares one HTTP client per host. It performs no
// network I/O — a down LAN server must not fail config application.
func New(raw json.RawMessage) (*Connector, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("http-bridge config is not valid JSON: %w", err)
	}
	c := &Connector{}
	for _, h := range cfg.Hosts {
		base, err := url.Parse(h.BaseURL)
		if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
			return nil, fmt.Errorf("http-bridge config: baseUrl %q must be an absolute http(s) URL", h.BaseURL)
		}
		tlsCfg := &tls.Config{InsecureSkipVerify: h.InsecureSkipVerify} //nolint:gosec // deliberate, per-host, cloud-authorized
		if h.CACertPem != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(h.CACertPem)) {
				return nil, fmt.Errorf("http-bridge config: caCertPem for %q contains no valid PEM certificate", h.BaseURL)
			}
			tlsCfg.RootCAs = pool
		}
		c.hosts = append(c.hosts, hostEntry{
			base: base,
			client: &http.Client{
				Transport: &http.Transport{TLSClientConfig: tlsCfg},
				// Never follow redirects: a redirect could point off-allowlist.
				// The 3xx is returned to the caller as the response.
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
		})
	}
	return c, nil
}

// effectivePort returns the explicit or scheme-default port.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// allowedFor returns the host entry whose base URL covers rawURL, or an error.
// Matching is component-wise: scheme, hostname, effective port must be equal,
// and the base's path (if any) must be a segment-bounded prefix of the path.
func (c *Connector) allowedFor(rawURL string) (*hostEntry, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("url %q is not an absolute URL", rawURL)
	}
	// An encoded slash/backslash in the raw path lets the wire path diverge
	// from the decoded path we match on: url.Parse decodes %2f into a "/"
	// for our path.Clean check, but http.NewRequestWithContext forwards the
	// RAW %2f verbatim, and origin servers that don't treat %2f as a
	// separator would route the request outside the allowlisted path prefix.
	// Reject rather than try to reconcile the two encodings.
	if esc := u.EscapedPath(); strings.Contains(esc, "%2f") || strings.Contains(esc, "%2F") ||
		strings.Contains(esc, "%5c") || strings.Contains(esc, "%5C") {
		return nil, fmt.Errorf("url %q contains an encoded path separator", rawURL)
	}
	// Normalize dot-segments in the request path to prevent traversal attacks.
	reqPath := path.Clean(u.Path)
	if reqPath == "." || reqPath == "" {
		reqPath = "/"
	}
	for i := range c.hosts {
		b := c.hosts[i].base
		if u.Scheme != b.Scheme || !strings.EqualFold(u.Hostname(), b.Hostname()) ||
			effectivePort(u) != effectivePort(b) {
			continue
		}
		basePath := strings.TrimSuffix(b.Path, "/")
		if basePath != "" && reqPath != basePath && !strings.HasPrefix(reqPath, basePath+"/") {
			continue
		}
		return &c.hosts[i], nil
	}
	return nil, fmt.Errorf("url %q is not on the allowlist", rawURL)
}

const (
	maxResponseBytes = 10 << 20 // stay under the 16MiB tunnel frame limit post-base64
	// defaultTimeout is a HARD CEILING, not just a default: a larger
	// timeoutMs param is intentionally clamped down to it (see forward()
	// below) because Phase 2b's egress hop must not expect >25s.
	defaultTimeout = 25 * time.Second
)

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Headers   map[string]string `json:"headers"`
		BodyB64   string            `json:"bodyB64"`
		TimeoutMs int               `json:"timeoutMs"`
	} `json:"params"`
}

// hop-by-hop headers are connection-scoped and must not be forwarded.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Host": true,
}

// Handle processes one JSON-RPC request. `http/forward` does the work;
// initialize/tools/list are answered minimally so the gateway's capability
// fan-out sees a well-formed, tool-less server (the bridge has no user tools).
func (c *Connector) Handle(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var req rpcRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("http-bridge: payload is not JSON-RPC shaped: %w", err)
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
				"serverInfo":      map[string]any{"name": "onprem-http-bridge", "version": "1.0.0"},
			},
		})
	case "tools/list":
		return marshal(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{"tools": []any{}},
		})
	case "http/forward":
		return c.forward(ctx, id, &req)
	default:
		return marshal(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": -32601, "message": "Method not found: " + req.Method},
		})
	}
}

func (c *Connector) forward(ctx context.Context, id json.RawMessage, req *rpcRequest) (json.RawMessage, error) {
	p := req.Params
	if p.Method == "" || p.URL == "" {
		return rpcError(id, -32602, "http/forward requires method and url params")
	}
	host, err := c.allowedFor(p.URL)
	if err != nil {
		return rpcError(id, -32000, err.Error())
	}

	var body io.Reader
	if p.BodyB64 != "" {
		raw, err := base64.StdEncoding.DecodeString(p.BodyB64)
		if err != nil {
			return rpcError(id, -32602, "bodyB64 is not valid base64")
		}
		body = strings.NewReader(string(raw))
	}

	timeout := defaultTimeout
	if p.TimeoutMs > 0 && time.Duration(p.TimeoutMs)*time.Millisecond < timeout {
		timeout = time.Duration(p.TimeoutMs) * time.Millisecond
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, p.Method, p.URL, body)
	if err != nil {
		return rpcError(id, -32602, "http/forward: bad method or url: "+err.Error())
	}
	for k, v := range p.Headers {
		if !hopByHop[http.CanonicalHeaderKey(k)] {
			httpReq.Header.Set(k, v)
		}
	}

	resp, err := host.client.Do(httpReq)
	if err != nil {
		return rpcError(id, -32000, "http/forward: request failed: "+err.Error())
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return rpcError(id, -32000, "http/forward: reading response failed: "+err.Error())
	}
	if len(respBody) > maxResponseBytes {
		return rpcError(id, -32000, "http/forward: response exceeds 10MiB limit")
	}

	headers := map[string]string{}
	for k, vs := range resp.Header {
		if !hopByHop[k] {
			// TODO: Set-Cookie needs []string preservation — comma-join corrupts
			// multi-cookie responses (fine for the API-key/basic-auth launch vendors).
			headers[k] = strings.Join(vs, ", ")
		}
	}
	return marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{
			"status":  resp.StatusCode,
			"headers": headers,
			"bodyB64": base64.StdEncoding.EncodeToString(respBody),
		},
	})
}

func rpcError(id json.RawMessage, code int, message string) (json.RawMessage, error) {
	return marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

func marshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}
