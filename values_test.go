package intreccio

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// TestPublicResultValues covers #66: nodes, relationships and paths come
// back as public types with names, also inside lists and maps.
func TestPublicResultValues(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (:Person:Admin {name: 'Ann', age: 30})-[:KNOWS {since: 2020}]->(:Person {name: 'Bob'})", nil)
	res := mustQuery(t, db, `MATCH (a {name: 'Ann'})-[r:KNOWS]->(b)
		MATCH p = shortestPath((a)-[*]-(b))
		MATCH (a)-[vr*1..1]->(b)
		RETURN a, r, b, p, vr, [a, null] AS l, {who: b} AS m, collect(r) AS rs`, nil)
	row := res.Rows[0]
	ann, ok := row[0].(Node)
	if !ok {
		t.Fatalf("a is %T, want Node", row[0])
	}
	if !reflect.DeepEqual(ann.Labels, []string{"Person", "Admin"}) || !reflect.DeepEqual(ann.Props, map[string]any{"name": "Ann", "age": int64(30)}) {
		t.Errorf("a = %+v", ann)
	}
	bob := row[2].(Node)
	rel, ok := row[1].(Relationship)
	if !ok || rel.Type != "KNOWS" || rel.StartID != ann.ID || rel.EndID != bob.ID || rel.Props["since"] != int64(2020) {
		t.Errorf("r = %#v", row[1])
	}
	p, ok := row[3].(Path)
	if !ok || len(p.Nodes) != 2 || len(p.Relationships) != 1 || p.Nodes[0].ID != ann.ID || p.Relationships[0].ID != rel.ID {
		t.Errorf("p = %#v", row[3])
	}
	if vr, ok := row[4].([]any); !ok || len(vr) != 1 || vr[0].(Relationship).ID != rel.ID {
		t.Errorf("vr = %#v", row[4])
	}
	if l := row[5].([]any); l[0].(Node).ID != ann.ID || l[1] != nil {
		t.Errorf("l = %#v", row[5])
	}
	if m := row[6].(map[string]any); m["who"].(Node).ID != bob.ID {
		t.Errorf("m = %#v", row[6])
	}
	if rs := row[7].([]any); rs[0].(Relationship).Type != "KNOWS" {
		t.Errorf("rs = %#v", row[7])
	}

	for got, want := range map[string]string{
		ann.String():    `(:Person:Admin {age: 30, name: "Ann"})`,
		rel.String():    `[:KNOWS {since: 2020}]`,
		p.String():      `(:Person:Admin {age: 30, name: "Ann"})-[:KNOWS {since: 2020}]->(:Person {name: "Bob"})`,
		Node{}.String(): `()`,
	} {
		if got != want {
			t.Errorf("String() = %s, want %s", got, want)
		}
	}
	// A path read from the other end shows the relationship pointing back.
	res = mustQuery(t, db, "MATCH (a {name: 'Bob'}), (b {name: 'Ann'}), p = shortestPath((a)-[*]-(b)) RETURN p", nil)
	if got := res.Rows[0][0].(Path).String(); got != `(:Person {name: "Bob"})<-[:KNOWS {since: 2020}]-(:Person:Admin {age: 30, name: "Ann"})` {
		t.Errorf("reverse path = %s", got)
	}
}

// TestPublicValuesAsParameters: a node, relationship or path from a result
// can be passed back as a parameter and names the stored element.
func TestPublicValuesAsParameters(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	mustQuery(t, db, "CREATE (:P {name: 'a'})-[:R {w: 1}]->(:P {name: 'b'})", nil)
	row := mustQuery(t, db, "MATCH (a {name: 'a'})-[r]->(b), p = shortestPath((a)-[*]-(b)) RETURN a, r, p", nil).Rows[0]
	params := map[string]any{"n": row[0], "r": row[1], "p": row[2], "list": []any{row[0]}}
	mustQuery(t, db, "MATCH (n {name: 'a'}) SET n.name = 'a2'", nil)

	got := rowsOf2(t, db, `MATCH (x)-[y]->() WHERE x = $n AND y = $r
		RETURN id($n) = id(x), $n.name, $r.w, length($p), $list[0].name`, params)
	if got != "[[true a2 1 1 a2]]" {
		t.Errorf("parameters = %s", got)
	}
	// The caller's map is left as it was.
	if _, ok := params["n"].(Node); !ok {
		t.Errorf("params[n] became %T", params["n"])
	}
	// A deleted element is null.
	mustQuery(t, db, "MATCH (n) DETACH DELETE n", nil)
	if got := rowsOf2(t, db, "RETURN $n IS NULL, $r IS NULL, $p IS NULL", params); got != "[[true true true]]" {
		t.Errorf("deleted = %s", got)
	}
	// Explicit transactions return public values too.
	mustQuery(t, db, "CREATE (:Q)", nil)
	err := db.View(ctx, func(tx *Tx) error {
		res, err := tx.Query(ctx, "MATCH (q:Q) RETURN q", nil)
		if err != nil {
			return err
		}
		if _, ok := res.Rows[0][0].(Node); !ok {
			t.Errorf("Tx.Query value is %T", res.Rows[0][0])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func rowsOf2(t *testing.T, db *DB, q string, params map[string]any) string {
	t.Helper()
	res, err := db.Query(context.Background(), q, params)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return fmt.Sprint(res.Rows)
}
