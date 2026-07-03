package mssql

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAssertReadOnlyQuery(t *testing.T) {
	allowed := []string{
		"SELECT 1",
		"select * from AR_Customer",
		"  WITH cte AS (SELECT 1 AS x) SELECT * FROM cte",
		"SELECT TOP 5 * FROM SO_SalesOrderHeader;",
		"/* aging */ SELECT CustomerNo FROM AR_Customer -- trailing",
	}
	for _, q := range allowed {
		if err := AssertReadOnlyQuery(q); err != nil {
			t.Errorf("expected allowed, got %v: %s", err, q)
		}
	}

	refused := []string{
		"DELETE FROM AR_Customer",
		"UPDATE AR_Customer SET CustomerName = 'x'",
		"INSERT INTO t VALUES (1)",
		"DROP TABLE AR_Customer",
		"EXEC sp_who",
		"TRUNCATE TABLE t",
		"SELECT 1; DELETE FROM t",       // multi-statement smuggling
		"/* SELECT */ DELETE FROM t",    // comment cannot launder the verb
		"-- SELECT\nDROP TABLE t",       // line comment cannot either
	}
	for _, q := range refused {
		if err := AssertReadOnlyQuery(q); err == nil {
			t.Errorf("expected refusal: %s", q)
		}
	}
}

func TestNewConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     string
		wantErr string
	}{
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
	c, err := New(json.RawMessage(`{"host":"127.0.0.1","database":"demo","user":"u","password":"p","encrypt":"disable"}`))
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if c.db == nil {
		t.Fatal("pool not initialized")
	}
}
