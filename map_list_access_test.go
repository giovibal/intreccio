package intreccio

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestMapAndListAccess covers #33: .key on maps, list/map subscripts and
// list slices.
func TestMapAndListAccess(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	params := map[string]any{
		"m": map[string]any{"a": int64(1), "nested": map[string]any{"b": "x"}, "list": []any{int64(10), int64(20)}},
		"l": []any{int64(1), int64(2), int64(3), int64(4)},
	}
	exprs := []struct {
		expr string
		want any
	}{
		{"$m.a", int64(1)},
		{"$m.missing", nil},
		{"$m.nested.b", "x"},
		{"$m.list[1]", int64(20)},
		{"$m['a']", int64(1)},
		{"{a: 1}.a", int64(1)},
		{"$l[0]", int64(1)},
		{"$l[-1]", int64(4)},
		{"$l[4]", nil},
		{"$l[-5]", nil},
		{"$l[null]", nil},
		{"null[0]", nil},
		{"$l[1..3]", []any{int64(2), int64(3)}},
		{"$l[..2]", []any{int64(1), int64(2)}},
		{"$l[2..]", []any{int64(3), int64(4)}},
		{"$l[-2..]", []any{int64(3), int64(4)}},
		{"$l[3..1]", []any{}},
		{"$l[0..100]", []any{int64(1), int64(2), int64(3), int64(4)}},
		{"$l[null..2]", nil},
	}
	for _, c := range exprs {
		res, err := db.Query(ctx, "RETURN "+c.expr+" AS v", params)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := res.Rows[0][0]; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.expr, got, c.want)
		}
	}
	for _, bad := range []string{"$l['a']", "$m[1]", "1[0]", "$l[1.5]", "'abc'.x"} {
		if _, err := db.Query(ctx, "RETURN "+bad, params); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
}

// TestBulkLoadWithMapRows: the batch-loading pattern from #33, creating nodes
// and relationships from rows of maps.
func TestBulkLoadWithMapRows(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	people := []map[string]any{{"uid": 1, "name": "Ann"}, {"uid": 2, "name": "Bob"}, {"uid": 3, "name": "Cid"}}
	edges := []map[string]any{{"a": 1, "b": 2, "since": 2020}, {"a": 2, "b": 3, "since": 2021}}
	steps := []struct {
		q string
		p map[string]any
	}{
		{"CREATE INDEX FOR (p:Person) ON (p.uid)", nil},
		{"UNWIND $rows AS r CREATE (:Person {uid: r.uid, name: r.name})", map[string]any{"rows": people}},
		{"UNWIND $rows AS r MATCH (a:Person {uid: r.a}), (b:Person {uid: r.b}) CREATE (a)-[:KNOWS {since: r.since}]->(b)", map[string]any{"rows": edges}},
	}
	for _, s := range steps {
		if _, err := db.Query(ctx, s.q, s.p); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
	res, err := db.Query(ctx, "MATCH (a:Person)-[k:KNOWS]->(b) RETURN a.name, b.name, k.since ORDER BY k.since", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{"Ann", "Bob", int64(2020)}, {"Bob", "Cid", int64(2021)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Errorf("got %v, want %v", res.Rows, want)
	}
	plan, err := db.Explain(ctx, "UNWIND $rows AS r MATCH (a:Person {uid: r.a}) RETURN a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "NodeByProperty(a:Person {uid = r.a})") {
		t.Errorf("the row lookup should use the index:\n%s", plan)
	}
}

// TestUnaliasedColumnsKeepTheirValues: a projection without an alias is named
// after its expression; lists, maps and CASE used to get the name "?" from
// the analyzer and a different name from the planner, so the value was lost.
func TestUnaliasedColumnsKeepTheirValues(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.Query(ctx, "RETURN [1, 2], {a: 1}, CASE WHEN true THEN 'y' END, [3, 4][0]", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantCols := []string{"[1, 2]", "{a: 1}", "CASE WHEN true THEN 'y' END", "[3, 4][0]"}
	if !reflect.DeepEqual(res.Columns, wantCols) {
		t.Errorf("columns %q, want %q", res.Columns, wantCols)
	}
	wantRow := []any{[]any{int64(1), int64(2)}, map[string]any{"a": int64(1)}, "y", int64(3)}
	if !reflect.DeepEqual(res.Rows[0], wantRow) {
		t.Errorf("row %#v, want %#v", res.Rows[0], wantRow)
	}
}
