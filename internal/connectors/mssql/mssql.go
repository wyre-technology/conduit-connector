// Package mssql is the SQL Server connector (connector-v1 M-D) — the Sage 100
// Premium path. READ-ONLY by design: the write path to Sage data is the
// business-object layer, never raw SQL (docs/onprem-connector-v1.md).
//
// Two enforcement layers, deliberately redundant:
//  1. The REAL guard is the site's SQL principal: the scope doc's posture is
//     a scoped read-only login/gMSA (db_datareader or curated roles).
//  2. This code additionally refuses anything but a single SELECT/WITH
//     statement — belt and suspenders, and an honest error message when a
//     caller tries a write.
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
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

const (
	defaultMaxRows   = 100
	hardMaxRows      = 1000
	queryTimeout     = 15 * time.Second
	identifierRegexp = `^[A-Za-z_][A-Za-z0-9_]*$`
)

var identifierRe = regexp.MustCompile(identifierRegexp)

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

// Connector holds the connection pool for one configured site database.
type Connector struct {
	db *sql.DB
}

// New validates the pushed config and prepares the pool. Connection is lazy —
// a down database does not block config application; the first tool call
// surfaces the connectivity error.
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
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return &Connector{db: db}, nil
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

// Handle processes one MCP JSON-RPC request. Tool names arrive PRE-STRIPPED
// by the conduit unified-router (`mssql__query` reaches us as `query`).
func (c *Connector) Handle(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var req rpcRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("mssql: payload is not JSON-RPC shaped: %w", err)
	}
	id := req.ID
	if id == nil {
		id = json.RawMessage("null")
	}

	switch req.Method {
	case "initialize":
		return marshal(rpcResult(id, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "conduit-mssql", "version": "1.0.0"},
		}))

	case "tools/list":
		return marshal(rpcResult(id, map[string]any{"tools": toolCatalog()}))

	case "tools/call":
		ctx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()
		text, err := c.callTool(ctx, req.Params.Name, req.Params.Arguments)
		if err != nil {
			return marshal(rpcError(id, -32000, err.Error()))
		}
		return marshal(rpcResult(id, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
		}))

	default:
		return marshal(rpcError(id, -32601, "Method not found: "+req.Method))
	}
}

func (c *Connector) callTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "list_tables":
		var a struct {
			Schema string `json:"schema"`
		}
		_ = json.Unmarshal(args, &a)
		q := `SELECT TABLE_SCHEMA, TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_TYPE = 'BASE TABLE'`
		params := []any{}
		if a.Schema != "" {
			q += ` AND TABLE_SCHEMA = @p1`
			params = append(params, a.Schema)
		}
		q += ` ORDER BY TABLE_SCHEMA, TABLE_NAME`
		return c.runToJSON(ctx, q, hardMaxRows, params...)

	case "describe_table":
		var a struct {
			Table  string `json:"table"`
			Schema string `json:"schema"`
		}
		if err := json.Unmarshal(args, &a); err != nil || a.Table == "" {
			return "", fmt.Errorf("describe_table requires a `table` argument")
		}
		if !identifierRe.MatchString(a.Table) || (a.Schema != "" && !identifierRe.MatchString(a.Schema)) {
			return "", fmt.Errorf("table/schema must be plain identifiers (%s)", identifierRegexp)
		}
		q := `SELECT COLUMN_NAME, DATA_TYPE, IS_NULLABLE, CHARACTER_MAXIMUM_LENGTH
		      FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_NAME = @p1`
		params := []any{a.Table}
		if a.Schema != "" {
			q += ` AND TABLE_SCHEMA = @p2`
			params = append(params, a.Schema)
		}
		q += ` ORDER BY ORDINAL_POSITION`
		return c.runToJSON(ctx, q, hardMaxRows, params...)

	case "query":
		var a struct {
			Query   string `json:"query"`
			MaxRows int    `json:"max_rows"`
		}
		if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Query) == "" {
			return "", fmt.Errorf("query requires a `query` argument")
		}
		if err := AssertReadOnlyQuery(a.Query); err != nil {
			return "", err
		}
		maxRows := a.MaxRows
		if maxRows <= 0 {
			maxRows = defaultMaxRows
		}
		if maxRows > hardMaxRows {
			maxRows = hardMaxRows
		}
		return c.runToJSON(ctx, a.Query, maxRows)

	default:
		return "", fmt.Errorf("unknown tool %q (available: query, list_tables, describe_table)", name)
	}
}

var commentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
var lineCommentRe = regexp.MustCompile(`--[^\n]*`)

// AssertReadOnlyQuery refuses anything but a single SELECT/WITH statement.
// This is the belt-and-suspenders layer — the site's read-only SQL principal
// is the real enforcement (docs/onprem-connector-v1.md, per-user permissions
// design note). Exported for tests.
func AssertReadOnlyQuery(q string) error {
	stripped := lineCommentRe.ReplaceAllString(commentRe.ReplaceAllString(q, " "), " ")
	stripped = strings.TrimSpace(stripped)
	// A single trailing semicolon is tolerated; any OTHER semicolon means
	// multiple statements — refuse rather than parse.
	body := strings.TrimSuffix(stripped, ";")
	if strings.Contains(body, ";") {
		return fmt.Errorf("only a single statement is allowed (found ';')")
	}
	upper := strings.ToUpper(body)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return fmt.Errorf("only SELECT/WITH queries are allowed — this connector is read-only by design; " +
			"writes to Sage/MSSQL data go through the application layer, not raw SQL")
	}
	return nil
}

// runToJSON executes a query and renders rows as a JSON array of objects.
func (c *Connector) runToJSON(ctx context.Context, query string, maxRows int, params ...any) (string, error) {
	rows, err := c.db.QueryContext(ctx, query, params...)
	if err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	out := make([]map[string]any, 0, 16)
	truncated := false
	for rows.Next() {
		if len(out) >= maxRows {
			truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			if b, ok := vals[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = vals[i]
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	result := map[string]any{"rows": out, "rowCount": len(out), "truncated": truncated}
	b, err := json.Marshal(result)
	return string(b), err
}

func toolCatalog() []any {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	return []any{
		map[string]any{
			"name":        "query",
			"description": "Run a single read-only SELECT/WITH query against the site database. Results capped (default 100, max 1000 rows).",
			"inputSchema": obj(map[string]any{
				"query":    map[string]any{"type": "string", "description": "A single SELECT or WITH statement"},
				"max_rows": map[string]any{"type": "integer", "description": "Row cap (default 100, max 1000)"},
			}, "query"),
		},
		map[string]any{
			"name":        "list_tables",
			"description": "List base tables, optionally filtered by schema.",
			"inputSchema": obj(map[string]any{
				"schema": map[string]any{"type": "string"},
			}),
		},
		map[string]any{
			"name":        "describe_table",
			"description": "Column names, types, and nullability for a table.",
			"inputSchema": obj(map[string]any{
				"table":  map[string]any{"type": "string"},
				"schema": map[string]any{"type": "string"},
			}, "table"),
		},
	}
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func marshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}
