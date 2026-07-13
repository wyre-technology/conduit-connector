package httpbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		"https://apps.customer.local:8443/itglue",        // exact path == prefix
		"https://apps.customer.local:8443/itglue/a/../b", // dot-segments within prefix are normalized away
	}
	for _, u := range allowed {
		if _, err := c.allowedFor(u); err != nil {
			t.Errorf("expected allowed, got %v: %s", err, u)
		}
	}

	refused := []string{
		"https://cw.customer.local.evil.com/x",                 // suffix-extended host
		"http://cw.customer.local/x",                           // scheme downgrade
		"https://cw.customer.local:8080/x",                     // wrong port
		"https://evil.com/https://cw.customer.local",           // host mismatch
		"https://apps.customer.local:8443/itglue2/x",           // path prefix without segment boundary
		"https://apps.customer.local:8443/other",               // outside path prefix
		"https://apps.customer.local/itglue/x",                 // missing required port
		"https://apps.customer.local:8443/itglue/../other",     // path traversal attack with dot-segments
		"https://apps.customer.local:8443/itglue/%2e%2e/other", // path traversal with URL-encoded dots
		"not a url",
	}
	for _, u := range refused {
		if _, err := c.allowedFor(u); err == nil {
			t.Errorf("expected refusal: %s", u)
		}
	}
}

func call(t *testing.T, c *Connector, payload string) map[string]any {
	t.Helper()
	out, err := c.Handle(context.Background(), json.RawMessage(payload))
	if err != nil {
		t.Fatalf("Handle returned transport error: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return resp
}

func forwardParams(method, u string, headers map[string]string, body []byte) string {
	p := map[string]any{"method": method, "url": u}
	if headers != nil {
		p["headers"] = headers
	}
	if body != nil {
		p["bodyB64"] = base64.StdEncoding.EncodeToString(body)
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "http/forward", "params": p})
	return string(b)
}

func TestToolsListIsEmptyAndUnknownMethodsRefused(t *testing.T) {
	c := mustNew(t, `{"hosts":[]}`)
	resp := call(t, c, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 0 {
		t.Errorf("tools/list must be empty, got %v", tools)
	}
	for _, m := range []string{"tools/call", "resources/list", "bogus"} {
		resp := call(t, c, `{"jsonrpc":"2.0","id":1,"method":"`+m+`"}`)
		if resp["error"] == nil {
			t.Errorf("method %q must return a JSON-RPC error", m)
		}
	}
}

func TestForwardRoundTrip(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Answer", "42")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := mustNew(t, `{"hosts":[{"baseUrl":"`+srv.URL+`"}]}`)
	resp := call(t, c, forwardParams("POST", srv.URL+"/api/v1/thing?x=1",
		map[string]string{"Authorization": "Basic abc", "Connection": "close"}, []byte(`{"in":1}`)))

	res := resp["result"].(map[string]any)
	if res["status"].(float64) != 201 {
		t.Errorf("status = %v, want 201", res["status"])
	}
	body, _ := base64.StdEncoding.DecodeString(res["bodyB64"].(string))
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %s", body)
	}
	if res["headers"].(map[string]any)["X-Answer"] != "42" {
		t.Errorf("missing response header, got %v", res["headers"])
	}
	if got.Method != "POST" || got.URL.RequestURI() != "/api/v1/thing?x=1" {
		t.Errorf("server saw %s %s", got.Method, got.URL.RequestURI())
	}
	if got.Header.Get("Authorization") != "Basic abc" {
		t.Error("Authorization header not forwarded")
	}
	if got.Header.Get("Connection") == "close" {
		t.Error("hop-by-hop Connection header must be stripped")
	}
	if string(gotBody) != `{"in":1}` {
		t.Errorf("request body = %s", gotBody)
	}
}

func TestForwardErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 11<<20))) // > 10MiB cap
	}))
	defer srv.Close()
	c := mustNew(t, `{"hosts":[{"baseUrl":"`+srv.URL+`"}]}`)

	cases := []struct{ name, payload, wantErrSubstr string }{
		{"off-allowlist", forwardParams("GET", "https://evil.example/x", nil, nil), "allowlist"},
		{"bad method param", `{"jsonrpc":"2.0","id":1,"method":"http/forward","params":{"url":"` + srv.URL + `"}}`, "method"},
		{"oversized response", forwardParams("GET", srv.URL+"/big", nil, nil), "10MiB"},
		{"bad base64", `{"jsonrpc":"2.0","id":1,"method":"http/forward","params":{"method":"POST","url":"` + srv.URL + `","bodyB64":"!!!"}}`, "bodyB64"},
	}
	for _, tc := range cases {
		resp := call(t, c, tc.payload)
		errObj, ok := resp["error"].(map[string]any)
		if !ok {
			t.Errorf("%s: expected JSON-RPC error, got %v", tc.name, resp)
			continue
		}
		if !strings.Contains(errObj["message"].(string), tc.wantErrSubstr) {
			t.Errorf("%s: error %q does not mention %q", tc.name, errObj["message"], tc.wantErrSubstr)
		}
	}
}

func TestTLSPrivateCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// PEM-encode the test server's self-signed cert.
	certPem := pemEncodeCert(t, srv.Certificate().Raw)

	// Without trust: TLS failure surfaces as a JSON-RPC error.
	noTrust := mustNew(t, `{"hosts":[{"baseUrl":"`+srv.URL+`"}]}`)
	resp := call(t, noTrust, forwardParams("GET", srv.URL+"/", nil, nil))
	if resp["error"] == nil {
		t.Error("expected TLS verification failure without CA trust")
	}

	// With the CA pinned: succeeds.
	cfg, _ := json.Marshal(map[string]any{"hosts": []map[string]any{{"baseUrl": srv.URL, "caCertPem": certPem}}})
	trusted := mustNew(t, string(cfg))
	resp = call(t, trusted, forwardParams("GET", srv.URL+"/", nil, nil))
	if resp["error"] != nil {
		t.Errorf("expected success with pinned CA, got %v", resp["error"])
	}

	// insecureSkipVerify also succeeds.
	skip := mustNew(t, `{"hosts":[{"baseUrl":"`+srv.URL+`","insecureSkipVerify":true}]}`)
	resp = call(t, skip, forwardParams("GET", srv.URL+"/", nil, nil))
	if resp["error"] != nil {
		t.Errorf("expected success with insecureSkipVerify, got %v", resp["error"])
	}
}

func pemEncodeCert(t *testing.T, der []byte) string {
	t.Helper()
	return fmt.Sprintf("-----BEGIN CERTIFICATE-----\n%s\n-----END CERTIFICATE-----\n",
		base64.StdEncoding.EncodeToString(der))
}
