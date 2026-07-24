// Package mssql is the SQL Server connector (connector-v1 M-D) — the Sage 100
// Premium path. Read-only; all the MCP + query logic lives in sqlcommon. This
// package is just the SQL Server DSN + driver wiring.
//
// Config (pushed via config_update, held in memory only):
//
//	{ "host": "10.0.0.5", "port": 1433, "database": "SageDemo",
//	  "user": "conduit_readonly", "password": "...", "encrypt": "disable|true" }
//
// Two authentication modes:
//   - SQL login (default): user + password in the config.
//   - Windows Integrated Auth ("auth":"integrated"): NO user/password — the
//     connector authenticates to SQL Server as its own Windows service
//     identity via SSPI. When the Windows service runs under a gMSA (see
//     install.ps1 -ServiceAccount), that identity IS the gMSA, so no SQL
//     credential is stored anywhere. This connector only wires integrated auth
//     on Windows (the driver's winsspi provider, using the process identity);
//     it is rejected on non-Windows.
package mssql

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"runtime"
	"strings"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/wyre-technology/conduit-tunnel/internal/connectors/sqlcommon"
)

// Config is the pushed per-site connector config.
type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
	// Encrypt maps to the driver's encrypt param: "true" (default) or
	// "disable" for lab/demo databases without TLS.
	Encrypt string `json:"encrypt"`
	// Auth selects the authentication mode: "" or "sql" (default) uses
	// user+password; "integrated" uses Windows Integrated Auth (SSPI) as the
	// service's own identity — no SQL credential stored. Integrated requires a
	// Windows host.
	Auth string `json:"auth"`
}

// Connector is the sqlcommon connector bound to a SQL Server pool.
type Connector = sqlcommon.Connector

// New validates the pushed config and prepares the pool. Connection is lazy —
// a down database does not block config application; the first tool call
// surfaces the connectivity error. `@pN` is SQL Server's placeholder style.
func New(raw json.RawMessage) (*Connector, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("mssql config is not valid JSON: %w", err)
	}
	dsn, err := buildDSN(cfg)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("mssql pool init failed: %w", err)
	}
	return sqlcommon.New(db, "conduit-mssql", func(n int) string { return fmt.Sprintf("@p%d", n) }), nil
}

// buildDSN validates the config and produces the sqlserver:// DSN. Split out
// from New so the DSN shape can be unit-tested directly — in particular the
// load-bearing invariant that integrated mode produces NO userinfo (the empty
// userinfo is exactly what makes the driver's winsspi provider authenticate as
// the process's own identity).
func buildDSN(cfg Config) (string, error) {
	if cfg.Host == "" {
		return "", fmt.Errorf("mssql config requires host")
	}
	if cfg.Database == "" {
		return "", fmt.Errorf("mssql config requires database")
	}
	if cfg.Port == 0 {
		cfg.Port = 1433
	}
	encrypt := cfg.Encrypt
	if encrypt == "" {
		encrypt = "true"
	}

	integrated, err := useIntegratedAuth(cfg)
	if err != nil {
		return "", err
	}

	q := url.Values{"database": {cfg.Database}, "encrypt": {encrypt}}
	dsn := &url.URL{
		Scheme:   "sqlserver",
		Host:     fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		RawQuery: q.Encode(),
	}
	if !integrated {
		// SQL-login mode: credentials go in the DSN userinfo.
		dsn.User = url.UserPassword(cfg.User, cfg.Password)
	}
	// Integrated mode: NO userinfo. On Windows the driver's winsspi provider
	// then authenticates as the process's own identity (the gMSA the service
	// runs as); nothing else to set on the DSN.
	return dsn.String(), nil
}

// useIntegratedAuth resolves and validates the auth mode. It returns true for
// Windows Integrated Auth, false for SQL-login, or an error when the config is
// inconsistent (integrated requested off-Windows, or a SQL-login missing its
// credentials). The mode is matched case- and whitespace-insensitively so a
// plausible value like "Integrated" or " sql " is accepted rather than
// confusingly rejected.
func useIntegratedAuth(cfg Config) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Auth)) {
	case "", "sql":
		if cfg.User == "" || cfg.Password == "" {
			return false, fmt.Errorf(
				`mssql config requires user + password for SQL-login auth; ` +
					`set "auth":"integrated" (Windows service running as a gMSA) to authenticate ` +
					`without a stored SQL credential`)
		}
		return false, nil
	case "integrated":
		if runtime.GOOS != "windows" {
			return false, fmt.Errorf(
				`mssql "auth":"integrated" (Windows Integrated Auth / SSPI) is only wired on a `+
					`Windows host by this connector; it is running on %s. Use SQL-login (user + password) here`,
				runtime.GOOS)
		}
		if cfg.User != "" || cfg.Password != "" {
			return false, fmt.Errorf(
				`mssql "auth":"integrated" takes no user/password — the connector authenticates as ` +
					`its own Windows service identity (the gMSA). Remove user/password, or use ` +
					`"auth":"sql" to authenticate with them`)
		}
		return true, nil
	default:
		return false, fmt.Errorf(`mssql "auth" must be "sql" (default) or "integrated"; got %q`, cfg.Auth)
	}
}
