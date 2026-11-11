package intreccio

import (
	"context"
	"reflect"
	"testing"
)

// TestEqualityOperators covers #20: `=` and `<>` on lists, maps, nodes and
// relationships, and null propagation (comparing with null yields null).
func TestEqualityOperators(t *testing.T) {
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
		{"[1, 2] = [1, 2]", true},
		{"[1, 2] = [1, 3]", false},
		{"[1, 2] = [1, 2, 3]", false},
		{"[1, 2] = [1.0, 2]", true},
		{"[[1], ['a']] = [[1], ['a']]", true},
		{"[1, null] = [1, null]", nil},
		{"[1, null] = [2, null]", false},
		{"[] = []", true},
		{"[1] = 1", false},
		{"{a: 1, b: [2]} = {b: [2], a: 1}", true},
		{"{a: 1} = {a: 2}", false},
		{"{a: 1} = {a: 1, b: 2}", false},
		{"{a: null} = {a: null}", nil},
		{"[1, 2] <> [1, 2]", false},
		{"[1, 2] <> [2, 1]", true},
		{"[1, null] <> [1, null]", nil},
		{"1 = null", nil},
		{"null = null", nil},
		{"1 <> null", nil},
		{"1 <> 'a'", true},
		{"1 = 'a'", false},
		{"'a' <> 'a'", false},
		{"NOT (null = 1)", nil},
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

	setup := []string{
		"CREATE (:L {name: 'a', tags: ['x', 'y']}), (:L {name: 'b', tags: ['y']}), (:L {name: 'c'})",
		"MATCH (a:L {name: 'a'}), (b:L {name: 'b'}) CREATE (a)-[:R]->(b), (b)-[:R]->(a)",
	}
	for _, s := range setup {
		if _, err := db.Query(ctx, s, nil); err != nil {
			t.Fatal(err)
		}
	}
	queries := []struct {
		cypher string
		params map[string]any
		want   [][]any
	}{
		{"MATCH (n:L) WHERE n.tags = ['x', 'y'] RETURN n.name", nil, [][]any{{"a"}}},
		{"MATCH (n:L) WHERE n.tags = $t RETURN n.name", map[string]any{"t": []any{"y"}}, [][]any{{"b"}}},
		{"MATCH (n:L {tags: ['y']}) RETURN n.name", nil, [][]any{{"b"}}},
		// A missing property compares as null, so NOT (null = ...) stays null.
		{"MATCH (n:L) WHERE NOT n.tags = ['y'] RETURN n.name", nil, [][]any{{"a"}}},
		{"MATCH (a:L), (b:L) WHERE a = b RETURN a.name ORDER BY a.name", nil, [][]any{{"a"}, {"b"}, {"c"}}},
		{"MATCH (a:L), (b:L) WHERE a <> b RETURN count(*)", nil, [][]any{{int64(6)}}},
		{"MATCH (a)-[r1:R]->(b), (b)-[r2:R]->(a) WHERE r1 = r2 RETURN count(*)", nil, [][]any{{int64(0)}}},
		{"MATCH (a)-[r1:R]->(b) MATCH (c)-[r2:R]->(d) WHERE r1 = r2 RETURN count(*)", nil, [][]any{{int64(2)}}},
	}
	for _, q := range queries {
		res, err := db.Query(ctx, q.cypher, q.params)
		if err != nil {
			t.Errorf("%s: %v", q.cypher, err)
			continue
		}
		if !reflect.DeepEqual(res.Rows, q.want) {
			t.Errorf("%s: got %v, want %v", q.cypher, res.Rows, q.want)
		}
	}
}

// TestMergeMatchesListProperty: MERGE on a list property must find the
// existing node instead of creating a duplicate each time.
func TestMergeMatchesListProperty(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i := 0; i < 3; i++ {
		if _, err := db.Query(ctx, "MERGE (:T {tags: ['a', 'b']})", nil); err != nil {
			t.Fatal(err)
		}
	}
	res, err := db.Query(ctx, "MATCH (n:T) RETURN count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(1) {
		t.Errorf("MERGE created duplicates: count %v, want 1", got)
	}
}

// TestIndexedListLookup: with an index on the property, a lookup by a list
// value (not indexable) must still find the node.
func TestIndexedListLookup(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range []string{
		"CREATE INDEX FOR (n:L) ON (n.tags)",
		"CREATE (:L {name: 'a', tags: ['x', 'y']}), (:L {name: 'b', tags: 'x'})",
	} {
		if _, err := db.Query(ctx, s, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		"MATCH (n:L {tags: ['x', 'y']}) RETURN n.name",
		"MATCH (n:L) WHERE n.tags = ['x', 'y'] RETURN n.name",
	} {
		res, err := db.Query(ctx, q, nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := [][]any{{"a"}}; !reflect.DeepEqual(res.Rows, want) {
			t.Errorf("%s: got %v, want %v", q, res.Rows, want)
		}
	}
}
