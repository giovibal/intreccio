package intreccio

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestNullPropertySemantics covers #19: a null property value means "absent",
// and comparing with null never matches, with or without an index.
func TestNullPropertySemantics(t *testing.T) {
	ctx := context.Background()
	for _, indexed := range []bool{false, true} {
		db, err := OpenInMemory()
		if err != nil {
			t.Fatal(err)
		}
		q := func(cypher string, params map[string]any) [][]any {
			t.Helper()
			res, err := db.Query(ctx, cypher, params)
			if err != nil {
				t.Fatalf("indexed=%v %s: %v", indexed, cypher, err)
			}
			return res.Rows
		}
		if indexed {
			q("CREATE INDEX FOR (n:P) ON (n.x)", nil)
		}
		q("CREATE (:P {name: 'a', x: null}), (:P {name: 'b', x: 1})", nil)
		q("CREATE (:P {name: 'c', x: $v})", map[string]any{"v": nil})
		q("MATCH (a:P {name: 'a'}), (b:P {name: 'b'}) CREATE (a)-[:R {w: null, k: 1}]->(b)", nil)

		checks := []struct {
			cypher string
			params map[string]any
			want   [][]any
		}{
			{"MATCH (n:P {name: 'a'}) RETURN keys(n)", nil, [][]any{{[]any{"name"}}}},
			{"MATCH (n:P {name: 'c'}) RETURN keys(n)", nil, [][]any{{[]any{"name"}}}},
			{"MATCH ()-[r:R]->() RETURN keys(r)", nil, [][]any{{[]any{"k"}}}},
			{"MATCH (n:P) WHERE n.x = null RETURN n.name", nil, nil},
			{"MATCH (n:P {x: null}) RETURN n.name", nil, nil},
			{"MATCH (n:P) WHERE n.x = $v RETURN n.name", map[string]any{"v": nil}, nil},
			{"MATCH (n:P {x: $v}) RETURN n.name", map[string]any{"v": nil}, nil},
			{"MATCH (n:P) WHERE n.x IS NULL RETURN n.name ORDER BY n.name", nil, [][]any{{"a"}, {"c"}}},
			{"MATCH (n:P) WHERE n.x = 1 RETURN n.name", nil, [][]any{{"b"}}},
		}
		for _, c := range checks {
			if got := q(c.cypher, c.params); !reflect.DeepEqual(got, c.want) {
				t.Errorf("indexed=%v %s: got %v, want %v", indexed, c.cypher, got, c.want)
			}
		}
		_ = db.Close()
	}
}

// TestMergeRejectsNullProperty: MERGE cannot match or create on a null
// property value (openCypher/Neo4j raise an error instead of guessing).
func TestMergeRejectsNullProperty(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Query(ctx, "CREATE (:M {name: 'existing'})", nil); err != nil {
		t.Fatal(err)
	}
	for _, cypher := range []string{
		"MERGE (:M {x: null})",
		"MERGE (:Fresh {x: null})",
		"MATCH (a:M) MERGE (a)-[:R {w: null}]->(:M {name: 'other'})",
	} {
		_, err := db.Query(ctx, cypher, nil)
		if err == nil || !strings.Contains(err.Error(), "null") {
			t.Errorf("%s: got err %v, want a null-property error", cypher, err)
		}
	}
	res, err := db.Query(ctx, "MATCH (n) RETURN count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(1) {
		t.Errorf("failed MERGE must not write: node count %v, want 1", got)
	}
}
