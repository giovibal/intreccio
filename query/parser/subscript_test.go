package parser

import (
	"testing"

	"github.com/giovibal/intreccio/query/ast"
)

// #33: subscripts and slices parse as postfix operators, render back the same
// way, and leave relationship patterns alone.
func TestParseSubscriptsAndSlices(t *testing.T) {
	cases := map[string]string{
		"RETURN l[0]":          "l[0]",
		"RETURN l[-1]":         "l[-1]",
		"RETURN m['k']":        "m['k']",
		"RETURN l[1..3]":       "l[1..3]",
		"RETURN l[..2]":        "l[..2]",
		"RETURN l[2..]":        "l[2..]",
		"RETURN r.a[0].b":      "r.a[0].b",
		"RETURN [1, 2, 3][$i]": "[1, 2, 3][$i]",
		"RETURN l[i + 1]":      "l[i + 1]",
	}
	for src, want := range cases {
		q, err := Parse(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		ret := q.Clauses[0].(*ast.Return)
		if got := ast.Format(ret.Items[0].Expr); got != want {
			t.Errorf("%s: formatted as %q, want %q", src, got, want)
		}
	}
	// A pattern after an expression still parses as a pattern.
	if _, err := Parse("MATCH (a)-[r:R]->(b) WHERE a.x = 1 RETURN r"); err != nil {
		t.Errorf("pattern: %v", err)
	}
	for _, bad := range []string{"RETURN l[", "RETURN l[1", "RETURN l[1..", "RETURN l[]"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%s: expected a parse error", bad)
		}
	}
}
