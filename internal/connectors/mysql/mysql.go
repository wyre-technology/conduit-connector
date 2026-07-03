// Package mysql is the MySQL/MariaDB connector (connector-v1). Read-only; all
// the MCP + query logic lives in sqlcommon — this package is just the MySQL
// DSN + driver wiring.
//
// Config (pushed via config_update, held in memory only):
//
//	{ "host": "10.0.0.5", "port": 3306, "database": "app",
//	  "user": "conduit_readonly", "password": "...", "tls": "preferred|true|skip-verify|false" }
package mysql

import (
	"database/sql"
	"encoding/json"
	"fmt"

	driver "github.com/go-sql-driver/mysql"

	"github.com/wyre-technology/conduit-connector/internal/connectors/sqlcommon"
)

// Config is the pushed per-site connector config.
type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
	// TLS maps to go-sql-driver's tls param: "preferred" (default: TLS if the
	// server offers it, else plaintext), "true" (require), "skip-verify", or
	// "false" (plaintext — lab DBs only).
	TLS string `json:"tls"`
}

// Connector is the sqlcommon connector bound to a MySQL pool.
type Connector = sqlcommon.Connector

// New validates the pushed config and prepares the pool. Connection is lazy —
// a down database does not block config application; the first tool call
// surfaces the connectivity error. MySQL uses `?` positional placeholders.
func New(raw json.RawMessage) (*Connector, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("mysql config is not valid JSON: %w", err)
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("mysql config requires host")
	}
	if cfg.Database == "" {
		return nil, fmt.Errorf("mysql config requires database")
	}
	if cfg.User == "" || cfg.Password == "" {
		return nil, fmt.Errorf("mysql config requires user + password")
	}
	if cfg.Port == 0 {
		cfg.Port = 3306
	}
	tls := cfg.TLS
	if tls == "" {
		tls = "preferred"
	}

	// Build the DSN via the driver's Config so the password is escaped
	// correctly (a raw fmt.Sprintf DSN breaks on '@' or '/' in the password).
	dc := driver.NewConfig()
	dc.User = cfg.User
	dc.Passwd = cfg.Password
	dc.Net = "tcp"
	dc.Addr = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	dc.DBName = cfg.Database
	dc.TLSConfig = tls
	dc.ParseTime = true

	db, err := sql.Open("mysql", dc.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("mysql pool init failed: %w", err)
	}
	return sqlcommon.New(db, "conduit-mysql", func(int) string { return "?" }), nil
}
