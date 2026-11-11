package intreccio

import (
	"context"
	"reflect"
	"testing"
)

// TestDuplicateLabelsStoredOnce covers #22: repeating a label in a pattern
// stores it once, whether the node comes from CREATE or MERGE.
func TestDuplicateLabelsStoredOnce(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range []string{
		"CREATE (:A:A:B:A {k: 'create'})",
		"MERGE (:M:M {k: 'merge'})",
	} {
		if _, err := db.Query(ctx, s, nil); err != nil {
			t.Fatal(err)
		}
	}
	checks := []struct {
		cypher string
		want   [][]any
	}{
		{"MATCH (n {k: 'create'}) RETURN labels(n)", [][]any{{[]any{"A", "B"}}}},
		{"MATCH (n {k: 'merge'}) RETURN labels(n)", [][]any{{[]any{"M"}}}},
		// Removing the label once must leave no copy behind.
		{"MATCH (n:A) REMOVE n:A RETURN labels(n)", [][]any{{[]any{"B"}}}},
		{"MATCH (n:A) RETURN count(n)", [][]any{{int64(0)}}},
	}
	for _, c := range checks {
		res, err := db.Query(ctx, c.cypher, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.cypher, err)
		}
		if !reflect.DeepEqual(res.Rows, c.want) {
			t.Errorf("%s: got %v, want %v", c.cypher, res.Rows, c.want)
		}
	}
}
