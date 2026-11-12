package intreccio

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestVarLengthRelationshipIsAList covers #81: the relationship variable of a
// variable-length pattern is a list of relationships, usable wherever a list
// is.
//
//	(a)-[:T {name: 'ab'}]->(b)-[:T {name: 'bc'}]->(c)
func TestVarLengthRelationshipIsAList(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (:X {n: 'a'})-[:T {name: 'ab'}]->(:X {n: 'b'})-[:T {name: 'bc'}]->(:X {n: 'c'})", nil)

	const (
		two  = "MATCH (a:X {n: 'a'})-[r:T*2..2]->(c) "
		one  = "MATCH (a:X {n: 'a'})-[r:T*1..1]->(c) "
		zero = "MATCH (a:X {n: 'a'})-[r:T*0..0]->(c) "
	)
	cases := []struct {
		name, query string
		want        [][]any
	}{
		{"size", two + "RETURN size(r)", [][]any{{int64(2)}}},
		{"list comprehension", two + "RETURN [x IN r | x.name]", [][]any{{[]any{"ab", "bc"}}}},
		{"list comprehension with WHERE", two + "RETURN [x IN r WHERE x.name = 'ab' | x.name]", [][]any{{[]any{"ab"}}}},
		{"list comprehension, type()", two + "RETURN [x IN r | type(x)]", [][]any{{[]any{"T", "T"}}}},
		{"all", two + "RETURN all(x IN r WHERE x.name <> 'zz')", [][]any{{true}}},
		{"any", two + "RETURN any(x IN r WHERE x.name = 'ab')", [][]any{{true}}},
		{"none", two + "RETURN none(x IN r WHERE x.name = 'ab')", [][]any{{false}}},
		{"single", two + "RETURN single(x IN r WHERE x.name = 'bc')", [][]any{{true}}},
		{"IN", two + "MATCH (a)-[e]->() RETURN e IN r", [][]any{{true}}},
		{"IN, a relationship not on the path", two + "MATCH (c)<-[e]-() RETURN e IN r[0..1]", [][]any{{false}}},
		{"UNWIND", two + "UNWIND r AS x RETURN x.name ORDER BY x.name", [][]any{{"ab"}, {"bc"}}},
		{"subscript", two + "RETURN r[0].name, r[-1].name", [][]any{{"ab", "bc"}}},
		{"subscript out of range", two + "RETURN r[5]", [][]any{{nil}}},
		{"slice", two + "RETURN size(r[0..1]), r[1..][0].name", [][]any{{int64(1), "bc"}}},
		{"head and last", two + "RETURN head(r).name, last(r).name", [][]any{{"ab", "bc"}}},
		{"tail", two + "RETURN size(tail(r)), tail(r)[0].name", [][]any{{int64(1), "bc"}}},
		{"concatenation", two + "RETURN size(r + r), size(r + [1])", [][]any{{int64(4), int64(3)}}},
		{"equality", two + "WITH r AS r1 " + two + "RETURN r = r1", [][]any{{true}}},
		{"collect and flatten", two + "WITH collect(r) AS rs RETURN size(rs), size(rs[0])", [][]any{{int64(1), int64(2)}}},
		{"through WITH", two + "WITH r RETURN [x IN r | x.name]", [][]any{{[]any{"ab", "bc"}}}},
		{"one hop", one + "RETURN [x IN r | x.name], r[0].name", [][]any{{[]any{"ab"}, "ab"}}},
		{"zero hops: an empty list", zero + "RETURN size(r), [x IN r | x.name], head(r), c.n", [][]any{{int64(0), []any{}, nil, "a"}}},
		{"zero hops: UNWIND gives no rows", zero + "UNWIND r AS x RETURN x", nil},
		{"shortestPath relationship variable",
			"MATCH (a:X {n: 'a'}), (c:X {n: 'c'}) MATCH shortestPath((a)-[r:T*]->(c)) RETURN [x IN r | x.name], r[-1].name",
			[][]any{{[]any{"ab", "bc"}, "bc"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := db.Query(context.Background(), tc.query, nil)
			if err != nil {
				t.Fatalf("%s: %v", tc.query, err)
			}
			if len(res.Rows) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(res.Rows, tc.want) {
				t.Errorf("%s\n got: %#v\nwant: %#v", tc.query, res.Rows, tc.want)
			}
		})
	}
}

// List concatenation with +, which the relationship list relies on.
func TestListConcatenation(t *testing.T) {
	db := newDB(t)
	cases := []struct {
		query string
		want  any
	}{
		{"RETURN [1, 10, 100] + [4, 5]", []any{int64(1), int64(10), int64(100), int64(4), int64(5)}},
		{"RETURN [false, true] + false", []any{false, true, false}},
		{"RETURN 'a' + ['b', 'c']", []any{"a", "b", "c"}},
		{"RETURN [] + []", []any{}},
		{"RETURN [[1]] + [2]", []any{[]any{int64(1)}, int64(2)}},
		{"RETURN [1] + [[2]]", []any{int64(1), []any{int64(2)}}},
		{"RETURN size([1, 2] + $l)", int64(4)},
	}
	for _, tc := range cases {
		res, err := db.Query(context.Background(), tc.query, map[string]any{"l": []any{3, 4}})
		if err != nil {
			t.Errorf("%s: %v", tc.query, err)
			continue
		}
		if got := res.Rows[0][0]; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s = %#v, want %#v", tc.query, got, tc.want)
		}
	}
	// The operands are not modified: a list built from r is still r.
	res := mustQuery(t, db, "WITH [1, 2] AS l WITH l, l + 3 AS m RETURN l, m", nil)
	if !reflect.DeepEqual(res.Rows[0], []any{[]any{int64(1), int64(2)}, []any{int64(1), int64(2), int64(3)}}) {
		t.Errorf("rows = %#v", res.Rows)
	}
}

// The public value of the variable is unchanged: a []any of Relationship.
func TestVarLengthRelationshipResult(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (:X {n: 'a'})-[:T {name: 'ab'}]->(:X {n: 'b'})-[:T {name: 'bc'}]->(:X {n: 'c'})", nil)

	check := func(t *testing.T, v any) {
		t.Helper()
		list, ok := v.([]any)
		if !ok || len(list) != 2 {
			t.Fatalf("r = %#v, want a list of two relationships", v)
		}
		for i, want := range []string{"ab", "bc"} {
			rel, ok := list[i].(Relationship)
			if !ok || rel.Type != "T" || rel.Props["name"] != want {
				t.Errorf("r[%d] = %#v, want relationship %s", i, list[i], want)
			}
		}
	}

	res := mustQuery(t, db, "MATCH (a:X {n: 'a'})-[r:T*2..2]->(c) RETURN r", nil)
	check(t, res.Rows[0][0])

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sub, err := db.Subscribe(ctx, "MATCH (a:X {n: 'a'})-[r:T*2..2]->(c) RETURN r", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	select {
	case c := <-sub.Changes():
		if !c.Initial || len(c.Added) != 1 {
			t.Fatalf("initial batch = %+v", c)
		}
		check(t, c.Added[0][0])
	case <-ctx.Done():
		t.Fatal("no initial batch")
	}
}
