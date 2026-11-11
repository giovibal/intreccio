package ast_test

import (
	"testing"

	"github.com/giovibal/intreccio/query/ast"
	"github.com/giovibal/intreccio/query/parser"
)

// expr parses src as the single RETURN item of a query.
func expr(t *testing.T, src string) ast.Expr {
	t.Helper()
	q, err := parser.Parse("RETURN " + src)
	if err != nil {
		t.Fatalf("parse(%q): %v", src, err)
	}
	return q.Clauses[0].(*ast.Return).Items[0].Expr
}

func TestEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"n.age + 1", "n.age + 1", true},
		{"n.age + 1", "n.age  +  1", true},
		{"count(*)", "COUNT(*)", true},
		{"count(n)", "count(DISTINCT n)", false},
		{"n.age + 1", "n.age + 2", false},
		{"n.age + 1", "m.age + 1", false},
		{"1", "1.0", false},
		{"1", "'1'", false},
		// Same text once formatted, different structure.
		{"(a + b) * c", "a + b * c", false},
		{"[x IN xs WHERE x > 1 | x * 2]", "[x IN xs WHERE x > 1 | x * 2]", true},
		{"[x IN xs WHERE x > 1 | x * 2]", "[x IN xs | x * 2]", false},
		{"any(x IN xs WHERE x = 1)", "all(x IN xs WHERE x = 1)", false},
		{"{a: 1, b: n.x}", "{b: n.x, a: 1}", true},
		{"{a: 1}", "{a: 1, b: 2}", false},
		{"CASE WHEN n.a THEN 1 ELSE 2 END", "CASE WHEN n.a THEN 1 ELSE 2 END", true},
		{"CASE WHEN n.a THEN 1 ELSE 2 END", "CASE WHEN n.a THEN 1 END", false},
		{"xs[0]", "xs[0]", true},
		{"xs[0..2]", "xs[0..3]", false},
		{"n:A:B", "n:A:B", true},
		{"n:A:B", "n:A", false},
		{"NOT n.a", "NOT n.a", true},
		{"$p", "$p", true},
		{"$p", "$q", false},
	}
	for _, tc := range cases {
		if got := ast.Equal(expr(t, tc.a), expr(t, tc.b)); got != tc.want {
			t.Errorf("Equal(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if !ast.Equal(nil, nil) || ast.Equal(nil, expr(t, "1")) {
		t.Error("Equal: nil is equal to nil only")
	}
}

// every kind of expression with children, to check that Walk and Rewrite
// reach all of them.
const everyKind = "[n.a, -n.b, n.c + 1, f(n.d), n:L, {k: n.e}, n.f[0], n.g[1..2], " +
	"[x IN n.h WHERE x > n.i | x + n.j], any(y IN n.k WHERE y = n.l), " +
	"CASE n.m WHEN n.o THEN n.p ELSE n.q END]"

func TestWalkVisitsEveryChild(t *testing.T) {
	seen := map[string]bool{}
	ast.Walk(expr(t, everyKind), func(e ast.Expr) bool {
		if pa, ok := e.(*ast.PropertyAccess); ok {
			seen[pa.Key] = true
		}
		return true
	})
	for _, k := range "abcdefghijklmopq" {
		if !seen[string(k)] {
			t.Errorf("Walk did not reach n.%c", k)
		}
	}
}

func TestWalkSkipsChildren(t *testing.T) {
	var vars []string
	ast.Walk(expr(t, "f(a) + b"), func(e ast.Expr) bool {
		if v, ok := e.(*ast.Variable); ok {
			vars = append(vars, v.Name)
		}
		_, call := e.(*ast.FunctionCall)
		return !call
	})
	if len(vars) != 1 || vars[0] != "b" {
		t.Errorf("visited variables = %v, want [b]", vars)
	}
}

func TestRewrite(t *testing.T) {
	in := expr(t, everyKind)
	before := ast.Format(in)

	// Rename n to m everywhere.
	out := ast.Rewrite(in, func(e ast.Expr) (ast.Expr, bool) {
		if v, ok := e.(*ast.Variable); ok && v.Name == "n" {
			return &ast.Variable{Name: "m"}, true
		}
		return nil, false
	})
	renamed := "[m.a, -m.b, m.c + 1, f(m.d), m:L, {k: m.e}, m.f[0], m.g[1..2], " +
		"[x IN m.h WHERE x > m.i | x + m.j], any(y IN m.k WHERE y = m.l), " +
		"CASE m.m WHEN m.o THEN m.p ELSE m.q END]"
	if !ast.Equal(out, expr(t, renamed)) {
		t.Errorf("Rewrite = %s\n      want %s", ast.Format(out), renamed)
	}
	if got := ast.Format(in); got != before {
		t.Errorf("Rewrite modified its input:\n got %s\nwant %s", got, before)
	}

	// A replaced subtree is not visited again.
	calls := 0
	ast.Rewrite(expr(t, "f(a, b)"), func(e ast.Expr) (ast.Expr, bool) {
		calls++
		return &ast.Literal{Value: int64(1)}, true
	})
	if calls != 1 {
		t.Errorf("fn called %d times on a replaced root, want 1", calls)
	}

	if ast.Rewrite(nil, func(ast.Expr) (ast.Expr, bool) { return nil, false }) != nil {
		t.Error("Rewrite(nil) must be nil")
	}
}
