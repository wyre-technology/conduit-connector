package mssql

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestNewConfigValidation(t *testing.T) {
	cases := []struct{ name, cfg, wantErr string }{
		{"missing host", `{"database":"d","user":"u","password":"p"}`, "host"},
		{"missing database", `{"host":"h","user":"u","password":"p"}`, "database"},
		{"missing creds", `{"host":"h","database":"d"}`, "user + password"},
		{"not json", `nope`, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(json.RawMessage(tc.cfg))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
	// Valid config prepares a lazy pool without connecting.
	if _, err := New(json.RawMessage(`{"host":"127.0.0.1","database":"demo","user":"u","password":"p","encrypt":"disable"}`)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestAuthModes(t *testing.T) {
	base := `"host":"127.0.0.1","database":"demo","encrypt":"disable"`

	// Unknown auth mode is rejected everywhere.
	if _, err := New(json.RawMessage(`{` + base + `,"auth":"kerberos"}`)); err == nil ||
		!strings.Contains(err.Error(), `"auth" must be`) {
		t.Fatalf("unknown auth mode: want validation error, got %v", err)
	}

	// Explicit "sql" behaves like the default (needs user+password).
	if _, err := New(json.RawMessage(`{` + base + `,"auth":"sql"}`)); err == nil ||
		!strings.Contains(err.Error(), "user + password") {
		t.Fatalf(`auth:"sql" without creds: want "user + password" error, got %v`, err)
	}

	// Integrated auth with stray credentials is rejected (they'd be ignored;
	// fail loud instead of silently). On Windows the OS check passes and the
	// stray-cred check fires; off-Windows the OS check fires first — either
	// way it errors.
	if _, err := New(json.RawMessage(`{` + base + `,"auth":"integrated","user":"u","password":"p"}`)); err == nil {
		t.Fatal(`auth:"integrated" with user/password: want error, got nil`)
	}

	// Case/whitespace variants of a valid mode are accepted, not rejected as
	// unknown: "SQL" normalizes to sql (so it needs creds, not an "auth must
	// be" error).
	if _, err := New(json.RawMessage(`{` + base + `,"auth":" SQL "}`)); err == nil ||
		!strings.Contains(err.Error(), "user + password") {
		t.Fatalf(`auth:" SQL " should normalize to sql, got %v`, err)
	}

	// Integrated auth with no credentials: valid on Windows (builds a
	// credential-less SSPI pool), rejected as unsupported off-Windows.
	_, err := New(json.RawMessage(`{` + base + `,"auth":"integrated"}`))
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatalf(`auth:"integrated" on windows: want success, got %v`, err)
		}
	} else {
		if err == nil || !strings.Contains(err.Error(), "only wired on a Windows host") {
			t.Fatalf(`auth:"integrated" off-windows: want Windows-only error, got %v`, err)
		}
	}
}

// TestBuildDSNShape asserts the single load-bearing property of integrated
// auth that can be verified without a live SQL Server: integrated mode must
// produce a DSN with NO userinfo (an empty userinfo is exactly what makes the
// driver authenticate as the process's own identity / gMSA), while SQL-login
// mode must carry credentials. Runs the integrated case only on Windows, where
// the auth mode is permitted.
func TestBuildDSNShape(t *testing.T) {
	// SQL-login mode: userinfo present.
	sqlDSN, err := buildDSN(Config{Host: "h", Port: 1433, Database: "d", User: "u", Password: "p", Encrypt: "disable"})
	if err != nil {
		t.Fatalf("sql-login buildDSN: %v", err)
	}
	if !strings.Contains(sqlDSN, "u:p@") {
		t.Fatalf("sql-login DSN should carry credentials, got %q", sqlDSN)
	}

	if runtime.GOOS == "windows" {
		intDSN, err := buildDSN(Config{Host: "h", Port: 1433, Database: "d", Encrypt: "disable", Auth: "integrated"})
		if err != nil {
			t.Fatalf("integrated buildDSN: %v", err)
		}
		// No '@' => no userinfo => driver uses the process identity (SSPI).
		if strings.Contains(intDSN, "@") {
			t.Fatalf("integrated DSN must have NO userinfo (no '@'), got %q", intDSN)
		}
		if !strings.HasPrefix(intDSN, "sqlserver://h:1433?") {
			t.Fatalf("integrated DSN shape unexpected: %q", intDSN)
		}
	}
}
