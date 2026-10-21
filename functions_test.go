package intreccio

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestNewFunctionsAndExpressions covers #34.
func TestNewFunctionsAndExpressions(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ints := func(xs ...int64) []any {
		out := make([]any, len(xs))
		for i, x := range xs {
			out[i] = x
		}
		return out
	}
	cases := []struct {
		expr string
		want any
	}{
		{"range(1, 4)", ints(1, 2, 3, 4)},
		{"range(0, 10, 3)", ints(0, 3, 6, 9)},
		{"range(5, 1, -2)", ints(5, 3, 1)},
		{"range(3, 1)", []any{}},
		{"coalesce(null, null, 2, 3)", int64(2)},
		{"coalesce(null)", nil},
		{"round(2.5)", 3.0},
		{"round(-2.5)", -2.0},
		{"round(2)", 2.0},
		{"floor(-1.5)", -2.0},
		{"ceil(1.2)", 2.0},
		{"sqrt(16)", 4.0},
		{"sign(-3)", int64(-1)},
		{"sign(0.0)", int64(0)},
		{"sign(2.5)", int64(1)},
		{"round(null)", nil},
		{"'abc' =~ 'a.*'", true},
		{"'abc' =~ 'b'", false}, // the whole string must match
		{"'abc' =~ '(?i)ABC'", true},
		{"1 =~ 'a'", nil},
		{"null =~ 'a'", nil},
		{"[x IN range(1, 5) WHERE x % 2 = 1 | x * 10]", ints(10, 30, 50)},
		{"[x IN [1, 2, 3] | x + 1]", ints(2, 3, 4)},
		{"[x IN [1, 2, 3] WHERE x > 1]", ints(2, 3)},
		{"[x IN [1, 2, 3]]", ints(1, 2, 3)},
		{"[x IN null | x]", nil},
		{"[x IN [[1, 2], [3]] | size(x)]", ints(2, 1)},
		{"any(x IN [1, 2, 3] WHERE x > 2)", true},
		{"any(x IN [1, 2] WHERE x > 2)", false},
		{"any(x IN [1, null] WHERE x > 2)", nil},
		{"all(x IN [1, 2] WHERE x > 0)", true},
		{"all(x IN [1, null, -1] WHERE x > 0)", false},
		{"all(x IN [1, null] WHERE x > 0)", nil},
		{"all(x IN [] WHERE x > 0)", true},
		{"none(x IN [1, 2] WHERE x > 2)", true},
		{"none(x IN [1, 3] WHERE x > 2)", false},
		{"single(x IN [1, 3] WHERE x > 2)", true},
		{"single(x IN [3, 4] WHERE x > 2)", false},
		{"single(x IN [3, null] WHERE x > 2)", nil},
		{"any(x IN null WHERE x > 0)", nil},
	}
	for _, c := range cases {
		res, err := db.Query(ctx, "RETURN "+c.expr+" AS v", nil)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := res.Rows[0][0]; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.expr, got, c.want)
		}
	}

	res, err := db.Query(ctx, "RETURN sqrt(-1) AS a, rand() AS r, timestamp() = timestamp() AS same", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Rows[0][0].(float64); !math.IsNaN(a) {
		t.Errorf("sqrt(-1) = %v, want NaN", a)
	}
	if r := res.Rows[0][1].(float64); r < 0 || r >= 1 {
		t.Errorf("rand() = %v, want [0, 1)", r)
	}
	if res.Rows[0][2] != true {
		t.Error("timestamp() must be stable within a query")
	}

	for expr, wantErr := range map[string]string{
		"range(1, 5, 0)":           "step",
		"range(1, 100000000)":      "more than",
		"'a' =~ '('":               "invalid regular expression",
		"[x IN 5 | x]":             "list",
		"startNode(1)":             "relationship",
		"range(1.5, 3)":            "integers",
		"[x IN [1] | y]":           "undefined variable",
		"size([x IN [1] | x]) + x": "undefined variable",
	} {
		_, err := db.Query(ctx, "RETURN "+expr, nil)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: got err %v, want one mentioning %q", expr, err, wantErr)
		}
	}
}

// Graph-facing parts: startNode/endNode, comprehensions over properties, and
// a comprehension variable that shadows nothing outside it.
func TestFunctionsOnGraphData(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Query(ctx, "CREATE (:P {name: 'a', tags: ['x', 'yy']})-[:R]->(:P {name: 'b', tags: []})", nil); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		q    string
		want [][]any
	}{
		{"MATCH ()-[r:R]->() RETURN startNode(r).name, endNode(r).name", [][]any{{"a", "b"}}},
		{"MATCH (n:P) RETURN n.name, [t IN n.tags WHERE size(t) > 1] ORDER BY n.name", [][]any{{"a", []any{"yy"}}, {"b", []any{}}}},
		{"MATCH (n:P) WHERE any(t IN n.tags WHERE t = 'x') RETURN n.name", [][]any{{"a"}}},
		{"MATCH (n:P) WHERE n.name =~ '[ab]' RETURN count(n)", [][]any{{int64(2)}}},
		{"UNWIND range(1, 3) AS i RETURN sum(i)", [][]any{{int64(6)}}},
		{"MATCH (n:P) RETURN coalesce(n.missing, n.name) ORDER BY n.name", [][]any{{"a"}, {"b"}}},
	}
	for _, c := range checks {
		res, err := db.Query(ctx, c.q, nil)
		if err != nil {
			t.Errorf("%s: %v", c.q, err)
			continue
		}
		if !reflect.DeepEqual(res.Rows, c.want) {
			t.Errorf("%s: got %v, want %v", c.q, res.Rows, c.want)
		}
	}
}
