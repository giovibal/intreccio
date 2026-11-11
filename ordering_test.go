package intreccio

import (
	"context"
	"math"
	"reflect"
	"testing"
)

// TestOrderingComparisons covers #46: <, <=, >, >= are false when NaN is
// involved and null when a side is null or the values cannot be compared.
func TestOrderingComparisons(t *testing.T) {
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
		{"1 < 2", true},
		{"2 <= 1", false},
		{"1 < 1.5", true},
		{"'a' < 'b'", true},
		{"false < true", true},
		{"$nan < 1", false},
		{"$nan > 1", false},
		{"$nan <= 1", false},
		{"$nan >= 1", false},
		{"1 >= $nan", false},
		{"$nan <= $nan", false},
		{"$nan >= $nan", false},
		{"null < 1", nil},
		{"1 >= null", nil},
		{"null <= null", nil},
		{"1 < 'a'", nil},
		{"'a' >= 1", nil},
		{"true < 1", nil},
		{"[1] < [2]", nil},
		{"NOT (null < 1)", nil},
		{"NOT (1 < 'a')", nil},
		{"NOT ($nan >= 1)", true},
	}
	params := map[string]any{"nan": math.NaN()}
	for _, c := range exprs {
		res, err := db.Query(ctx, "RETURN "+c.expr+" AS v", params)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := res.Rows[0][0]; got != c.want {
			t.Errorf("%s = %v, want %v", c.expr, got, c.want)
		}
	}

	setup := "CREATE (:S {name: 'nan', score: $nan}), (:S {name: 'ten', score: 10}), (:S {name: 'none'})"
	if _, err := db.Query(ctx, setup, params); err != nil {
		t.Fatal(err)
	}
	queries := []struct {
		cypher string
		want   [][]any
	}{
		{"MATCH (n:S) WHERE n.score >= 10 RETURN n.name", [][]any{{"ten"}}},
		{"MATCH (n:S) WHERE NOT n.score < 5 RETURN n.name ORDER BY n.name", [][]any{{"nan"}, {"ten"}}},
		// ORDER BY still needs a total order and must keep working.
		{"MATCH (n:S) WHERE n.score IS NOT NULL RETURN n.name ORDER BY n.name DESC", [][]any{{"ten"}, {"nan"}}},
	}
	for _, q := range queries {
		res, err := db.Query(ctx, q.cypher, params)
		if err != nil {
			t.Errorf("%s: %v", q.cypher, err)
			continue
		}
		if !reflect.DeepEqual(res.Rows, q.want) {
			t.Errorf("%s: got %v, want %v", q.cypher, res.Rows, q.want)
		}
	}
}
