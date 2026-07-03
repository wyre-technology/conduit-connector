// Package mssql is the SQL Server connector (connector-v1 M-D) — the Sage 100
// Premium path. Read-only; all the MCP + query logic lives in sqlcommon. This
// package is just the SQL Server DSN + driver wiring.
//
// Config (pushed via config_update, held in memory only):
//
//	{ "host": "10.0.0.5", "port": 1433, "database": "SageDemo",
//	  "user": "conduit_readonly", "password": "...", "encrypt": "disable|true" }
//
// gMSA/Windows Integrated Auth is the Windows-service path (M-D gate) and is
// selected by omitting user/password on a Windows host — not yet wired here.
package mssql

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/wyre-technology/conduit-connector/internal/connectors/sqlcommon"
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
	if cfg.Host == "" {
		return nil, fmt.Errorf("mssql config requires host")
	}
	if cfg.Database == "" {
		return nil, fmt.Errorf("mssql config requires database")
	}
	if cfg.User == "" || cfg.Password == "" {
		return nil, fmt.Errorf("mssql config requires user + password (gMSA/Windows auth is the Windows-service path)")
	}
	if cfg.Port == 0 {
		cfg.Port = 1433
	}
	encrypt := cfg.Encrypt
	if encrypt == "" {
		encrypt = "true"
	}

	dsn := &url.URL{
		Scheme: "sqlserver",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		RawQuery: url.Values{
			"database": {cfg.Database},
			"encrypt":  {encrypt},
		}.Encode(),
	}
	db, err := sql.Open("sqlserver", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("mssql pool init failed: %w", err)
	}
	return sqlcommon.New(db, "conduit-mssql", func(n int) string { return fmt.Sprintf("@p%d", n) }), nil
}
