package plan

import (
	"strings"
	"testing"
)

// Range predicates on an indexed property use an index range scan (#26),
// and keep the predicate as a filter.
func TestRangeScanPlans(t *testing.T) {
	cat := withIndex("P.x", "P.name", "P.uid")
	cases := []struct{ query, want string }{
		{"MATCH (n:P) WHERE n.x > 30 RETURN n", "NodeByPropertyRange(n:P {x > 30})"},
		{"MATCH (n:P) WHERE n.x >= 30 AND n.x < 32 RETURN n", "NodeByPropertyRange(n:P {x >= 30, x < 32})"},
		{"MATCH (n:P) WHERE 30 < n.x RETURN n", "NodeByPropertyRange(n:P {x > 30})"},
		{"MATCH (n:P) WHERE n.x <= $hi RETURN n", "NodeByPropertyRange(n:P {x <= $hi})"},
		{"MATCH (n:P) WHERE n.name STARTS WITH 'ab' RETURN n", "NodeByPropertyRange(n:P {name STARTS WITH 'ab'})"},
	}
	for _, c := range cases {
		out := Explain(mustPlan(t, c.query, cat))
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: want %q in plan:\n%s", c.query, c.want, out)
		}
		if !strings.Contains(out, "Filter(") {
			t.Errorf("%s: the range predicate must stay as a filter:\n%s", c.query, out)
		}
	}
}

func TestRangeScanNotUsed(t *testing.T) {
	cat := withIndex("P.x", "P.uid")
	for _, q := range []string{
		"MATCH (n:P) WHERE n.y > 30 RETURN n",           // no index on y
		"MATCH (n:Q) WHERE n.x > 30 RETURN n",           // no index on Q.x
		"MATCH (n:P) WHERE n.x > n.uid RETURN n",        // bound uses the node itself
		"MATCH (n:P) WHERE n.x > 1 OR n.x < 0 RETURN n", // not a conjunct
	} {
		if out := Explain(mustPlan(t, q, cat)); strings.Contains(out, "NodeByPropertyRange") {
			t.Errorf("%s: unexpected range scan:\n%s", q, out)
		}
	}
	// Equality wins over a range on the same node.
	out := Explain(mustPlan(t, "MATCH (n:P) WHERE n.x > 1 AND n.uid = 5 RETURN n", cat))
	if !strings.Contains(out, "NodeByProperty(n:P {uid = 5})") {
		t.Errorf("equality seek expected:\n%s", out)
	}
}

func TestCorrelatedRangeScan(t *testing.T) {
	out := Explain(mustPlan(t, "UNWIND $xs AS lo MATCH (n:P) WHERE n.x > lo RETURN n", withIndex("P.x")))
	if !strings.Contains(out, "Apply") || !strings.Contains(out, "NodeByPropertyRange(n:P {x > lo})") {
		t.Errorf("correlated range scan expected under Apply:\n%s", out)
	}
}
