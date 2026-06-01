package plan

import (
	"strings"
	"testing"

	"github.com/giovibal/intreccio/query/ast"
	"github.com/giovibal/intreccio/query/parser"
)

type fakeCatalog struct{ idx map[string]bool }

func (c fakeCatalog) HasIndex(label, key string) bool { return c.idx[label+"."+key] }

func withIndex(pairs ...string) fakeCatalog {
	m := map[string]bool{}
	for _, p := range pairs {
		m[p] = true
	}
	return fakeCatalog{idx: m}
}

func mustPlan(t *testing.T, src string, cat Catalog) Op {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse(%q): %v", src, err)
	}
	op, err := Plan(q, cat)
	if err != nil {
		t.Fatalf("plan(%q): %v", src, err)
	}
	return op
}

func collect(o Op) []Op {
	if o == nil {
		return nil
	}
	out := []Op{o}
	switch x := o.(type) {
	case *Expand:
		out = append(out, collect(x.Input)...)
	case *Filter:
		out = append(out, collect(x.Input)...)
	case *Project:
		out = append(out, collect(x.Input)...)
	case *Aggregate:
		out = append(out, collect(x.Input)...)
	case *Sort:
		out = append(out, collect(x.Input)...)
	case *Skip:
		out = append(out, collect(x.Input)...)
	case *Limit:
		out = append(out, collect(x.Input)...)
	case *CartesianProduct:
		out = append(out, collect(x.Left)...)
		out = append(out, collect(x.Right)...)
	case *Create:
		out = append(out, collect(x.Input)...)
	case *Merge:
		out = append(out, collect(x.Input)...)
	case *SetItems:
		out = append(out, collect(x.Input)...)
	case *Remove:
		out = append(out, collect(x.Input)...)
	case *Unwind:
		out = append(out, collect(x.Input)...)
	case *Delete:
		out = append(out, collect(x.Input)...)
	case *OuterApply:
		out = append(out, collect(x.Outer)...)
		out = append(out, collect(x.Inner)...)
	case *Union:
		for _, p := range x.Parts {
			out = append(out, collect(p)...)
		}
	}
	return out
}

func scanOf(t *testing.T, root Op) Op {
	t.Helper()
	for _, o := range collect(root) {
		switch o.(type) {
		case *AllNodesScan, *NodeByLabelScan, *NodeByProperty:
			return o
		}
	}
	t.Fatal("no access method found")
	return nil
}

func firstFilter(root Op) *Filter {
	for _, o := range collect(root) {
		if f, ok := o.(*Filter); ok {
			return f
		}
	}
	return nil
}

func firstExpand(root Op) *Expand {
	for _, o := range collect(root) {
		if e, ok := o.(*Expand); ok {
			return e
		}
	}
	return nil
}

func TestAnchorSelection(t *testing.T) {
	t.Run("indexed-property-from-where", func(t *testing.T) {
		root := mustPlan(t, "MATCH (p:Person) WHERE p.email = $e RETURN p", withIndex("Person.email"))
		np, ok := scanOf(t, root).(*NodeByProperty)
		if !ok {
			t.Fatalf("expected NodeByProperty, got %T", scanOf(t, root))
		}
		if np.Label != "Person" || np.Key != "email" {
			t.Errorf("wrong NodeByProperty: %+v", np)
		}
		// The equality is consumed by the anchor: no residual Filter.
		if f := firstFilter(root); f != nil {
			t.Errorf("unexpected Filter: %s", exprString(f.Pred))
		}
	})

	t.Run("indexed-property-inline", func(t *testing.T) {
		root := mustPlan(t, "MATCH (p:Person {email: $e}) RETURN p", withIndex("Person.email"))
		if _, ok := scanOf(t, root).(*NodeByProperty); !ok {
			t.Fatalf("expected NodeByProperty, got %T", scanOf(t, root))
		}
		if f := firstFilter(root); f != nil {
			t.Errorf("unexpected Filter: %s", exprString(f.Pred))
		}
	})

	t.Run("label-scan-when-not-indexed", func(t *testing.T) {
		root := mustPlan(t, "MATCH (p:Person) WHERE p.email = $e RETURN p", withIndex())
		if _, ok := scanOf(t, root).(*NodeByLabelScan); !ok {
			t.Fatalf("expected NodeByLabelScan, got %T", scanOf(t, root))
		}
		f := firstFilter(root)
		if f == nil || exprString(f.Pred) != "p.email = $e" {
			t.Errorf("expected Filter(p.email = $e), got %v", f)
		}
	})

	t.Run("all-nodes-scan", func(t *testing.T) {
		root := mustPlan(t, "MATCH (n) RETURN n", withIndex())
		if _, ok := scanOf(t, root).(*AllNodesScan); !ok {
			t.Fatalf("expected AllNodesScan, got %T", scanOf(t, root))
		}
	})

	t.Run("prefers-indexed-node-as-anchor", func(t *testing.T) {
		root := mustPlan(t, "MATCH (a:Person)-[:KNOWS]->(b:Person) WHERE b.email = $e RETURN a", withIndex("Person.email"))
		np, ok := scanOf(t, root).(*NodeByProperty)
		if !ok || np.Var != "b" {
			t.Fatalf("expected anchor NodeByProperty on b, got %T %+v", scanOf(t, root), scanOf(t, root))
		}
	})
}

func TestExpandDirection(t *testing.T) {
	t.Run("outgoing-from-anchor", func(t *testing.T) {
		root := mustPlan(t, "MATCH (a:Person)-[:KNOWS]->(b) RETURN b", withIndex())
		e := firstExpand(root)
		if e == nil || e.From != "a" || e.To != "b" || e.Dir != ast.DirOut {
			t.Fatalf("wrong Expand: %+v", e)
		}
	})

	t.Run("reversed-when-anchor-is-dst", func(t *testing.T) {
		// b has the label (anchor), a does not: expand from b to a, flipping the direction.
		root := mustPlan(t, "MATCH (a)-[:KNOWS]->(b:Person) RETURN a", withIndex())
		e := firstExpand(root)
		if e == nil || e.From != "b" || e.To != "a" || e.Dir != ast.DirIn {
			t.Fatalf("wrong reversed Expand: %+v", e)
		}
	})
}

func TestExplain(t *testing.T) {
	root := mustPlan(t, "MATCH (a:Person)-[:KNOWS]->(b) WHERE a.age > 30 RETURN b", withIndex())
	want := "" +
		"Project(b)\n" +
		"  Filter(a.age > 30)\n" +
		"    Expand(a)-[_r0:KNOWS]->(b)\n" +
		"      NodeByLabelScan(a:Person)\n"
	if got := Explain(root); got != want {
		t.Errorf("EXPLAIN\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestLabelOnExpandedNodeBecomesFilter(t *testing.T) {
	// b is reached via Expand: its label must be checked in a Filter.
	root := mustPlan(t, "MATCH (a:Person)-[:KNOWS]->(b:Admin) RETURN b", withIndex())
	f := firstFilter(root)
	if f == nil || exprString(f.Pred) != "b:Admin" {
		t.Errorf("expected Filter(b:Admin), got %v", f)
	}
}

func TestProjectionTail(t *testing.T) {
	root := mustPlan(t, "MATCH (a) RETURN a.name AS name ORDER BY name DESC SKIP 2 LIMIT 10", withIndex())
	// From the top: Limit -> Skip -> Sort -> Project.
	lim, ok := root.(*Limit)
	if !ok {
		t.Fatalf("expected root Limit, got %T", root)
	}
	skip, ok := lim.Input.(*Skip)
	if !ok {
		t.Fatalf("expected Skip, got %T", lim.Input)
	}
	sort, ok := skip.Input.(*Sort)
	if !ok {
		t.Fatalf("expected Sort, got %T", skip.Input)
	}
	if _, ok := sort.Input.(*Project); !ok {
		t.Fatalf("expected Project, got %T", sort.Input)
	}
}

func TestOptionalMatchProducesOuterApply(t *testing.T) {
	root := mustPlan(t,
		"MATCH (a) OPTIONAL MATCH (a)-[:T]->(b) RETURN a, b",
		withIndex())
	var outer *OuterApply
	for _, o := range collect(root) {
		if x, ok := o.(*OuterApply); ok {
			outer = x
			break
		}
	}
	if outer == nil {
		t.Fatal("expected an OuterApply in the plan")
	}
	// NewVars must contain at least the optionally bound `b`.
	found := false
	for _, v := range outer.NewVars {
		if v == "b" {
			found = true
		}
	}
	if !found {
		t.Errorf("NewVars %v does not contain 'b'", outer.NewVars)
	}
}

func TestUnionProducesUnionOp(t *testing.T) {
	root := mustPlan(t,
		"MATCH (a) RETURN a AS x UNION MATCH (b) RETURN b AS x",
		withIndex())
	u, ok := root.(*Union)
	if !ok {
		t.Fatalf("expected *Union at the root, got %T", root)
	}
	if len(u.Parts) != 2 {
		t.Errorf("expected 2 parts, got %d", len(u.Parts))
	}
	if u.All {
		t.Errorf("UNION (without ALL) should have All=false")
	}

	root = mustPlan(t,
		"MATCH (a) RETURN a AS x UNION ALL MATCH (b) RETURN b AS x",
		withIndex())
	u, ok = root.(*Union)
	if !ok {
		t.Fatalf("expected *Union at the root, got %T", root)
	}
	if !u.All {
		t.Errorf("UNION ALL should have All=true")
	}
}

func TestExplainSetVariants(t *testing.T) {
	cases := []struct {
		src      string
		contains string
	}{
		{"MATCH (n) SET n.x = 1", "n.x = 1"},
		{"MATCH (n) SET n:Foo", "n:Foo"},
		{"MATCH (n) SET n = {a: 1}", "n = {a: 1}"},
		{"MATCH (n) SET n += {a: 1}", "n += {a: 1}"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			root := mustPlan(t, tc.src, withIndex())
			got := Explain(root)
			if !strings.Contains(got, tc.contains) {
				t.Errorf("EXPLAIN for %q missing %q:\n%s", tc.src, tc.contains, got)
			}
		})
	}
}

func TestExplainRemoveAndUnwind(t *testing.T) {
	root := mustPlan(t, "MATCH (n) REMOVE n.email, n:Admin", withIndex())
	got := Explain(root)
	if !strings.Contains(got, "Remove(") || !strings.Contains(got, "n.email") || !strings.Contains(got, "n:Admin") {
		t.Errorf("REMOVE EXPLAIN missing expected text:\n%s", got)
	}

	root = mustPlan(t, "UNWIND [1, 2, 3] AS x RETURN x", withIndex())
	got = Explain(root)
	if !strings.Contains(got, "Unwind(") || !strings.Contains(got, "AS x") {
		t.Errorf("UNWIND EXPLAIN missing expected text:\n%s", got)
	}
}

func TestExplainOuterApply(t *testing.T) {
	root := mustPlan(t,
		"MATCH (a) OPTIONAL MATCH (a)-[:T]->(b) RETURN a, b",
		withIndex())
	got := Explain(root)
	if !strings.Contains(got, "OuterApply") {
		t.Errorf("OPTIONAL MATCH EXPLAIN missing OuterApply:\n%s", got)
	}
}

func TestExplainUnion(t *testing.T) {
	root := mustPlan(t,
		"MATCH (a) RETURN a AS x UNION MATCH (b) RETURN b AS x",
		withIndex())
	got := Explain(root)
	if !strings.Contains(got, "Union\n") {
		t.Errorf("UNION EXPLAIN missing 'Union':\n%s", got)
	}

	root = mustPlan(t,
		"MATCH (a) RETURN a AS x UNION ALL MATCH (b) RETURN b AS x",
		withIndex())
	got = Explain(root)
	if !strings.Contains(got, "UnionAll") {
		t.Errorf("UNION ALL EXPLAIN missing 'UnionAll':\n%s", got)
	}
}

func TestAggregatePlan(t *testing.T) {
	root := mustPlan(t, "MATCH (p)-[:KNOWS]->(f) WITH p, count(f) AS c RETURN p, c", withIndex())
	var agg *Aggregate
	for _, o := range collect(root) {
		if a, ok := o.(*Aggregate); ok {
			agg = a
		}
	}
	if agg == nil {
		t.Fatal("expected an Aggregate operator")
	}
	if len(agg.GroupKeys) != 1 || agg.GroupKeys[0].Column != "p" {
		t.Errorf("wrong group keys: %+v", agg.GroupKeys)
	}
	if len(agg.Aggs) != 1 || agg.Aggs[0].Column != "c" {
		t.Errorf("wrong aggs: %+v", agg.Aggs)
	}
}
