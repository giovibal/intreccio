package plan

import "testing"

func expandsOf(root Op) []*Expand {
	var out []*Expand
	for _, o := range collect(root) {
		if e, ok := o.(*Expand); ok {
			out = append(out, e)
		}
	}
	return out
}

// #28: anonymous elements nothing reads are marked so the executor can skip
// loading their records.
func TestExpandUnusedFlags(t *testing.T) {
	cases := []struct {
		query           string
		toUnused, relUn bool
	}{
		{"MATCH (a:P)-[:R]->() RETURN a, count(*)", true, true},
		{"MATCH (a:P)-[:R]->(b) RETURN a", false, true},
		{"MATCH (a:P)-[r:R]->() RETURN r", true, false},
		{"MATCH (a:P)-[:R]->(:Q) RETURN a", false, true},           // label filter reads it
		{"MATCH (a:P)-[:R]->({x: 1}) RETURN a", false, true},       // property filter reads it
		{"MATCH (a:P)-[:R {w: 1}]->() RETURN a", true, false},      // rel property filter
		{"MATCH p = (a:P)-[:R]->() RETURN a", false, false},        // path variable
		{"MATCH (a:P)-[:R*1..2]->() RETURN a", false, false},       // variable length
		{"MATCH (a:P)-[:R]->(b)-[:S]->() RETURN a, b", true, true}, // last hop only
	}
	for _, c := range cases {
		ex := expandsOf(mustPlan(t, c.query, NoIndexes))
		if len(ex) == 0 {
			t.Fatalf("%s: no expand", c.query)
		}
		last := ex[0] // collect walks from the root: the outermost expand first
		if last.ToUnused != c.toUnused || last.RelUnused != c.relUn {
			t.Errorf("%s: ToUnused=%v RelUnused=%v, want %v %v", c.query, last.ToUnused, last.RelUnused, c.toUnused, c.relUn)
		}
	}
}
