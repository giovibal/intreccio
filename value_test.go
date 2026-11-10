package intreccio

import (
	"context"
	"reflect"
	"testing"
)

// TestResultValuesArePublic verifies that whole nodes and relationships come
// back as the public Node/Relationship types (names resolved, labels sorted),
// never as the engine's internal records.
func TestResultValuesArePublic(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if _, err := db.Query(ctx,
		"CREATE (a:Person:Admin {name: 'Ann', age: 30})-[:KNOWS {since: 2020}]->(b:Person {name: 'Bob'})", nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := db.Query(ctx, "MATCH (a:Admin)-[r:KNOWS]->(b) RETURN a, r, b", nil)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(res.Rows))
	}
	a, ok := res.Rows[0][0].(Node)
	if !ok {
		t.Fatalf("a is %T, want intreccio.Node", res.Rows[0][0])
	}
	r, ok := res.Rows[0][1].(Relationship)
	if !ok {
		t.Fatalf("r is %T, want intreccio.Relationship", res.Rows[0][1])
	}
	b, ok := res.Rows[0][2].(Node)
	if !ok {
		t.Fatalf("b is %T, want intreccio.Node", res.Rows[0][2])
	}

	if want := []string{"Admin", "Person"}; !reflect.DeepEqual(a.Labels, want) {
		t.Errorf("a.Labels = %v, want %v (sorted)", a.Labels, want)
	}
	if want := map[string]any{"name": "Ann", "age": int64(30)}; !reflect.DeepEqual(a.Props, want) {
		t.Errorf("a.Props = %v, want %v", a.Props, want)
	}
	if want := (Relationship{ID: r.ID, Type: "KNOWS", Start: a.ID, End: b.ID, Props: map[string]any{"since": int64(2020)}}); !reflect.DeepEqual(r, want) {
		t.Errorf("r = %+v, want %+v", r, want)
	}
	if b.Props["name"] != "Bob" {
		t.Errorf("b.Props = %v", b.Props)
	}

	// id() agrees with the exported identifiers.
	ids, err := db.Query(ctx, "MATCH (a:Admin)-[r]->(b) RETURN id(a), id(r), id(b)", nil)
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	if got, want := ids.Rows[0], []any{a.ID, r.ID, b.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("ids = %v, want %v", got, want)
	}
}

// TestResultValuesNested covers entities nested in lists and maps, a node with
// neither labels nor properties, and a value returned by a write query.
func TestResultValuesNested(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	res, err := db.Query(ctx, "CREATE (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("create returning: %v", err)
	}
	bare, ok := res.Rows[0][0].(Node)
	if !ok {
		t.Fatalf("created value is %T, want intreccio.Node", res.Rows[0][0])
	}
	if len(bare.Labels) != 0 || bare.Props == nil || len(bare.Props) != 0 {
		t.Errorf("bare node = %+v, want no labels and an empty non-nil Props", bare)
	}

	if _, err := db.Query(ctx,
		"CREATE (a:P {name: 'a'})-[:T]->(b:P {name: 'b'})-[:T]->(c:P {name: 'c'})", nil); err != nil {
		t.Fatalf("create chain: %v", err)
	}

	res, err = db.Query(ctx, "MATCH (p:P) RETURN collect(p) AS ps", nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	list, ok := res.Rows[0][0].([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("collect = %#v, want a list of 3", res.Rows[0][0])
	}
	for i, v := range list {
		if _, ok := v.(Node); !ok {
			t.Errorf("collect[%d] is %T, want intreccio.Node", i, v)
		}
	}

	res, err = db.Query(ctx, "MATCH (a:P {name: 'a'})-[rs:T*2..2]->(c) RETURN rs", nil)
	if err != nil {
		t.Fatalf("var-length: %v", err)
	}
	rels, ok := res.Rows[0][0].([]any)
	if !ok || len(rels) != 2 {
		t.Fatalf("rs = %#v, want a list of 2", res.Rows[0][0])
	}
	for i, v := range rels {
		if rel, ok := v.(Relationship); !ok || rel.Type != "T" {
			t.Errorf("rs[%d] = %#v, want a T relationship", i, v)
		}
	}

	res, err = db.Query(ctx, "MATCH (p:P {name: 'a'}) RETURN {who: p, n: 1} AS m", nil)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	m, ok := res.Rows[0][0].(map[string]any)
	if !ok {
		t.Fatalf("m is %T, want a map", res.Rows[0][0])
	}
	if _, ok := m["who"].(Node); !ok || m["n"] != int64(1) {
		t.Errorf("m = %#v", m)
	}
}

// TestResultValuesLeaveParamsAlone verifies that exporting a result never
// mutates caller-owned parameter values that are returned as-is.
func TestResultValuesLeaveParamsAlone(t *testing.T) {
	db := newDB(t)
	list := []any{int64(1), "x", nil}
	res, err := db.Query(context.Background(), "RETURN $l AS l", map[string]any{"l": list})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !reflect.DeepEqual(res.Rows[0][0], []any{int64(1), "x", nil}) {
		t.Errorf("row = %#v", res.Rows[0][0])
	}
	if !reflect.DeepEqual(list, []any{int64(1), "x", nil}) {
		t.Errorf("parameter mutated: %#v", list)
	}
}
