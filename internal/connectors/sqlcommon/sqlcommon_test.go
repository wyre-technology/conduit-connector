package sqlcommon

import "testing"

func TestAssertReadOnlyQuery(t *testing.T) {
	allowed := []string{
		"SELECT 1",
		"select * from customers",
		"  WITH cte AS (SELECT 1 AS x) SELECT * FROM cte",
		"SELECT * FROM orders LIMIT 5;",
		"/* aging */ SELECT id FROM customers -- trailing",
	}
	for _, q := range allowed {
		if err := AssertReadOnlyQuery(q); err != nil {
			t.Errorf("expected allowed, got %v: %s", err, q)
		}
	}
	refused := []string{
		"DELETE FROM customers",
		"UPDATE customers SET name = 'x'",
		"INSERT INTO t VALUES (1)",
		"DROP TABLE customers",
		"TRUNCATE t",
		"SELECT 1; DELETE FROM t",    // multi-statement smuggling
		"/* SELECT */ DELETE FROM t", // comment cannot launder the verb
		"-- SELECT\nDROP TABLE t",    // line comment cannot either
	}
	for _, q := range refused {
		if err := AssertReadOnlyQuery(q); err == nil {
			t.Errorf("expected refusal: %s", q)
		}
	}
}
