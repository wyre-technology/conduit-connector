package httpbridge

import (
	"encoding/json"
	"testing"
)

func mustNew(t *testing.T, cfg string) *Connector {
	t.Helper()
	c, err := New(json.RawMessage(cfg))
	if err != nil {
		t.Fatalf("New(%s): %v", cfg, err)
	}
	return c
}

func TestNewRejectsBadConfig(t *testing.T) {
	bad := []string{
		`not json`,
		`{"hosts":[{"baseUrl":""}]}`, // empty baseUrl
		`{"hosts":[{"baseUrl":"ftp://cw.local"}]}`,                           // scheme not http(s)
		`{"hosts":[{"baseUrl":"cw.local"}]}`,                                 // no scheme
		`{"hosts":[{"baseUrl":"https://ok.local","caCertPem":"not a pem"}]}`, // unparseable CA
	}
	for _, cfg := range bad {
		if _, err := New(json.RawMessage(cfg)); err == nil {
			t.Errorf("expected New to reject config: %s", cfg)
		}
	}
}

func TestNewAcceptsEmptyHosts(t *testing.T) {
	// Valid but refuses everything — the cloud disables the bridge by pushing {}.
	c := mustNew(t, `{"hosts":[]}`)
	if _, err := c.allowedFor("https://cw.local/api"); err == nil {
		t.Error("empty allowlist must refuse every URL")
	}
}

func TestAllowlistMatching(t *testing.T) {
	c := mustNew(t, `{"hosts":[
		{"baseUrl":"https://cw.customer.local"},
		{"baseUrl":"https://apps.customer.local:8443/itglue"}
	]}`)

	allowed := []string{
		"https://cw.customer.local/v4_6_release/apis/3.0/company/companies?pageSize=1",
		"https://cw.customer.local/",
		"https://cw.customer.local:443/anything", // explicit default port == implicit
		"https://apps.customer.local:8443/itglue/organizations",
		"https://apps.customer.local:8443/itglue", // exact path == prefix
		"https://apps.customer.local:8443/itglue/a/../b", // dot-segments within prefix are normalized away
	}
	for _, u := range allowed {
		if _, err := c.allowedFor(u); err != nil {
			t.Errorf("expected allowed, got %v: %s", err, u)
		}
	}

	refused := []string{
		"https://cw.customer.local.evil.com/x",       // suffix-extended host
		"http://cw.customer.local/x",                 // scheme downgrade
		"https://cw.customer.local:8080/x",           // wrong port
		"https://evil.com/https://cw.customer.local", // host mismatch
		"https://apps.customer.local:8443/itglue2/x", // path prefix without segment boundary
		"https://apps.customer.local:8443/other",     // outside path prefix
		"https://apps.customer.local/itglue/x",       // missing required port
		"https://apps.customer.local:8443/itglue/../other", // path traversal attack with dot-segments
		"https://apps.customer.local:8443/itglue/%2e%2e/other", // path traversal with URL-encoded dots
		"not a url",
	}
	for _, u := range refused {
		if _, err := c.allowedFor(u); err == nil {
			t.Errorf("expected refusal: %s", u)
		}
	}
}
