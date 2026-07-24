// Package postgres is the PostgreSQL connector (connector-v1). Read-only; all
// the MCP + query logic lives in sqlcommon — this package is just the Postgres
// DSN + driver wiring. Works against Postgres, and any Postgres-wire database
// (a MySQL target would want its own driver; not wired here).
//
// Config (pushed via config_update, held in memory only):
//
//	{ "host": "10.0.0.5", "port": 5432, "database": "app",
//	  "user": "conduit_readonly", "password": "...", "sslmode": "require|disable" }
package postgres

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/wyre-technology/conduit-tunnel/internal/connectors/sqlcommon"
)

// Config is the pushed per-site connector config.
type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
	// SSLMode maps to libpq sslmode: "require" (default), "disable" for a lab
	// DB with no TLS, or "verify-full" for cert-verified TLS.
	SSLMode string `json:"sslmode"`
}

// Connector is the sqlcommon connector bound to a Postgres pool.
type Connector = sqlcommon.Connector

// New validates the pushed config and prepares the pool. Connection is lazy —
// a down database does not block config application; the first tool call
// surfaces the connectivity error. `$N` is Postgres's placeholder style.
func New(raw json.RawMessage) (*Connector, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("postgres config is not valid JSON: %w", err)
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("postgres config requires host")
	}
	if cfg.Database == "" {
		return nil, fmt.Errorf("postgres config requires database")
	}
	if cfg.User == "" || cfg.Password == "" {
		return nil, fmt.Errorf("postgres config requires user + password")
	}
	if cfg.Port == 0 {
		cfg.Port = 5432
	}
	sslmode := cfg.SSLMode
	if sslmode == "" {
		sslmode = "require"
	}

	dsn := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Path:     "/" + cfg.Database,
		RawQuery: url.Values{"sslmode": {sslmode}}.Encode(),
	}
	db, err := sql.Open("pgx", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("postgres pool init failed: %w", err)
	}
	return sqlcommon.New(db, "conduit-postgres", func(n int) string { return fmt.Sprintf("$%d", n) }), nil
}
