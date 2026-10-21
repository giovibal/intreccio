package plan

import (
	"strings"
	"testing"
)

// Correlated index lookups (#24, #27): when the value of an indexed equality
// depends on a variable bound earlier in the query, the seek must run once per
// incoming row (Apply) instead of as an independent scan.
func TestCorrelatedIndexSeek(t *testing.T) {
	cat := withIndex("Person.uid")
	cases := []struct {
		name, query string
		wantSeek    string // expected NodeByProperty line in EXPLAIN
	}{
		{
			"inline after UNWIND",
			"UNWIND $ids AS x MATCH (b:Person {uid: x}) RETURN b",
			"NodeByProperty(b:Person {uid = x})",
		},
		{
			"WHERE after UNWIND",
			"UNWIND $ids AS x MATCH (b:Person) WHERE b.uid = x RETURN b",
			"NodeByProperty(b:Person {uid = x})",
		},
		{
			"WHERE with the value on the left",
			"UNWIND $ids AS x MATCH (b:Person) WHERE x = b.uid RETURN b",
			"NodeByProperty(b:Person {uid = x})",
		},
		{
			"property of a bound node",
			"MATCH (a:Person {uid: 1}) MATCH (b:Person {uid: a.ref}) RETURN b",
			"NodeByProperty(b:Person {uid = a.ref})",
		},
		{
			"second part of the same MATCH",
			"MATCH (a:Person {uid: $a}), (b:Person) WHERE b.uid = a.ref RETURN b",
			"NodeByProperty(b:Person {uid = a.ref})",
		},
		{
			"after WITH",
			"WITH 5 AS x MATCH (b:Person {uid: x + 1}) RETURN b",
			"NodeByProperty(b:Person {uid = x + 1})",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Explain(mustPlan(t, c.query, cat))
			if !strings.Contains(out, c.wantSeek) {
				t.Errorf("want index seek %q in plan:\n%s", c.wantSeek, out)
			}
			if !strings.Contains(out, "Apply") {
				t.Errorf("want an Apply in plan:\n%s", out)
			}
			if strings.Contains(out, "NodeByLabelScan(b:Person)") {
				t.Errorf("unexpected label scan in plan:\n%s", out)
			}
		})
	}
}

// A value that depends on a variable not yet bound cannot drive a seek.
func TestUncorrelatedValueNotUsedForSeek(t *testing.T) {
	cat := withIndex("Person.uid")
	out := Explain(mustPlan(t, "MATCH (a:Person), (b:Person) WHERE a.uid = b.ref RETURN a", cat))
	if strings.Contains(out, "Apply") {
		t.Errorf("no Apply expected when nothing is bound yet:\n%s", out)
	}
	out = Explain(mustPlan(t, "MATCH (b:Person) WHERE b.uid = b.ref RETURN b", cat))
	if strings.Contains(out, "NodeByProperty") {
		t.Errorf("a value referencing the node itself cannot use the index:\n%s", out)
	}
}

// Constant values keep the plain, uncorrelated plan.
func TestConstantSeekHasNoApply(t *testing.T) {
	cat := withIndex("Person.uid")
	out := Explain(mustPlan(t, "MATCH (a:Person {uid: $a}), (b:Person {uid: $b}) RETURN a, b", cat))
	if strings.Contains(out, "Apply") {
		t.Errorf("constant seeks should not need Apply:\n%s", out)
	}
}
