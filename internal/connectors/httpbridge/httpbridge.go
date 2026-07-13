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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
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
