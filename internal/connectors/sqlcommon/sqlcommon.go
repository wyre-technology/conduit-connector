// Package sqlcommon is the driver-agnostic core shared by the SQL connectors
// (mssql, postgres). It speaks the MCP JSON-RPC surface — initialize,
// tools/list, tools/call — over a *sql.DB, exposing three READ-ONLY tools:
// query (a single SELECT/WITH), list_tables, describe_table.
//
// Read-only by design, enforced in two redundant layers:
//  1. The REAL guard is the site's SQL principal — a scoped read-only login
//     (connector-v1 scope doc). This code never issues a write.
//  2. AssertReadOnlyQuery additionally refuses anything but a single
//     SELECT/WITH statement, with an honest error when a caller tries a write.
//
// A driver package (mssql/postgres) builds the *sql.DB from its own DSN and
// hands it here with the driver's placeholder style ($1 vs @p1) and a server
// name — everything else is identical, so the two connectors cannot drift.
package sqlcommon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultMaxRows   = 100
	HardMaxRows      = 1000
	queryTimeout     = 15 * time.Second
	identifierRegexp = `^[A-Za-z_][A-Za-z0-9_]*$`
)

var identifierRe = regexp.MustCompile(identifierRegexp)

// Placeholder renders the driver's positional-parameter token for the nth
// (1-based) bind: `$1` for postgres, `@p1` for sqlserver.
type Placeholder func(n int) string

// Connector serves the read-only SQL MCP tools over one *sql.DB.
type Connector struct {
	db          *sql.DB
	serverName  string
	placeholder Placeholder
}

// New wraps a prepared *sql.DB. Pool tuning is the caller's (driver defaults
// differ); this just applies conservative caps.
func New(db *sql.DB, serverName string, placeholder Placeholder) *Connector {
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return &Connector{db: db, serverName: serverName, placeholder: placeholder}
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

// Handle processes one MCP JSON-RPC request. Tool names arrive PRE-STRIPPED by
// the conduit unified-router (`postgres__query` reaches us as `query`).
func (c *Connector) Handle(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var req rpcRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("%s: payload is not JSON-RPC shaped: %w", c.serverName, err)
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
			"serverInfo":      map[string]any{"name": c.serverName, "version": "1.0.0"},
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
			q += ` AND TABLE_SCHEMA = ` + c.placeholder(1)
			params = append(params, a.Schema)
		}
		q += ` ORDER BY TABLE_SCHEMA, TABLE_NAME`
		return c.runToJSON(ctx, q, HardMaxRows, params...)

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
		      FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_NAME = ` + c.placeholder(1)
		params := []any{a.Table}
		if a.Schema != "" {
			q += ` AND TABLE_SCHEMA = ` + c.placeholder(2)
			params = append(params, a.Schema)
		}
		q += ` ORDER BY ORDINAL_POSITION`
		return c.runToJSON(ctx, q, HardMaxRows, params...)

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
			maxRows = DefaultMaxRows
		}
		if maxRows > HardMaxRows {
			maxRows = HardMaxRows
		}
		return c.runToJSON(ctx, a.Query, maxRows)

	default:
		return "", fmt.Errorf("unknown tool %q (available: query, list_tables, describe_table)", name)
	}
}

var commentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
var lineCommentRe = regexp.MustCompile(`--[^\n]*`)

// AssertReadOnlyQuery refuses anything but a single SELECT/WITH statement.
// The site's read-only SQL principal is the real enforcement; this is the
// belt-and-suspenders layer. Exported for tests.
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
			"writes go through the application layer, not raw SQL")
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
