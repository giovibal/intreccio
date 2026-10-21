package intreccio

import (
	"context"
	"reflect"
	"testing"
)

// TestThreeValuedLogic covers #45: AND, OR and IN propagate null the way
// openCypher does, so NOT of an unknown stays unknown.
func TestThreeValuedLogic(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	exprs := []struct {
		expr string
		want any
	}{
		{"true AND true", true},
		{"true AND false", false},
		{"null AND true", nil},
		{"true AND null", nil},
		{"null AND false", false},
		{"false AND null", false},
		{"null AND null", nil},
		{"false OR false", false},
		{"true OR false", true},
		{"null OR true", true},
		{"true OR null", true},
		{"null OR false", nil},
		{"false OR null", nil},
		{"null OR null", nil},
		{"NOT (null AND true)", nil},
		{"NOT (null OR false)", nil},
		{"1 IN [1, 2]", true},
		{"3 IN [1, 2]", false},
		{"1 IN [1, null]", true},
		{"2 IN [1, null]", nil},
		{"null IN [1]", nil},
		{"null IN []", false},
		{"3 IN []", false},
		{"[1] IN [[1], null]", true},
		{"[2] IN [[1, null]]", false},
		{"[1] IN [[1, null]]", false},
		{"[1, 2] IN [[1, null]]", nil},
		{"NOT (2 IN [1, null])", nil},
		{"null IN null", nil},
	}
	for _, c := range exprs {
		res, err := db.Query(ctx, "RETURN "+c.expr+" AS v", nil)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := res.Rows[0][0]; got != c.want {
			t.Errorf("%s = %v, want %v", c.expr, got, c.want)
		}
	}

	if _, err := db.Query(ctx, "CREATE (:N {name: 'ab', a: 1, b: 2}), (:N {name: 'a', a: 1}), (:N {name: 'none'})", nil); err != nil {
		t.Fatal(err)
	}
	queries := []struct {
		cypher string
		want   [][]any
	}{
		// Nodes lacking a property make the conjunction unknown, not false.
		{"MATCH (n:N) WHERE NOT (n.a = 1 AND n.b = 2) RETURN n.name ORDER BY n.name", nil},
		{"MATCH (n:N) WHERE NOT (n.a = 2 AND n.b = 2) RETURN n.name ORDER BY n.name", [][]any{{"a"}, {"ab"}}},
		{"MATCH (n:N) WHERE n.a = 1 OR n.b = 2 RETURN n.name ORDER BY n.name", [][]any{{"a"}, {"ab"}}},
		{"MATCH (n:N) WHERE NOT n.b IN [3, null] RETURN n.name", nil},
	}
	for _, q := range queries {
		res, err := db.Query(ctx, q.cypher, nil)
		if err != nil {
			t.Errorf("%s: %v", q.cypher, err)
			continue
		}
		if !reflect.DeepEqual(res.Rows, q.want) {
			t.Errorf("%s: got %v, want %v", q.cypher, res.Rows, q.want)
		}
	}
}
