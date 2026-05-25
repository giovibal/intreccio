package plan

import (
	"testing"

	"github.com/giovibal/mycypher/internal/cypher/ast"
	"github.com/giovibal/mycypher/internal/cypher/parser"
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
	t.Fatal("nessun access method trovato")
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
			t.Fatalf("atteso NodeByProperty, got %T", scanOf(t, root))
		}
		if np.Label != "Person" || np.Key != "email" {
			t.Errorf("NodeByProperty errato: %+v", np)
		}
		// La equality è consumata dall'anchor: niente Filter residuo.
		if f := firstFilter(root); f != nil {
			t.Errorf("Filter inatteso: %s", exprString(f.Pred))
		}
	})

	t.Run("indexed-property-inline", func(t *testing.T) {
		root := mustPlan(t, "MATCH (p:Person {email: $e}) RETURN p", withIndex("Person.email"))
		if _, ok := scanOf(t, root).(*NodeByProperty); !ok {
			t.Fatalf("atteso NodeByProperty, got %T", scanOf(t, root))
		}
		if f := firstFilter(root); f != nil {
			t.Errorf("Filter inatteso: %s", exprString(f.Pred))
		}
	})

	t.Run("label-scan-when-not-indexed", func(t *testing.T) {
		root := mustPlan(t, "MATCH (p:Person) WHERE p.email = $e RETURN p", withIndex())
		if _, ok := scanOf(t, root).(*NodeByLabelScan); !ok {
			t.Fatalf("atteso NodeByLabelScan, got %T", scanOf(t, root))
		}
		f := firstFilter(root)
		if f == nil || exprString(f.Pred) != "p.email = $e" {
			t.Errorf("atteso Filter(p.email = $e), got %v", f)
		}
	})

	t.Run("all-nodes-scan", func(t *testing.T) {
		root := mustPlan(t, "MATCH (n) RETURN n", withIndex())
		if _, ok := scanOf(t, root).(*AllNodesScan); !ok {
			t.Fatalf("atteso AllNodesScan, got %T", scanOf(t, root))
		}
	})

	t.Run("prefers-indexed-node-as-anchor", func(t *testing.T) {
		root := mustPlan(t, "MATCH (a:Person)-[:KNOWS]->(b:Person) WHERE b.email = $e RETURN a", withIndex("Person.email"))
		np, ok := scanOf(t, root).(*NodeByProperty)
		if !ok || np.Var != "b" {
			t.Fatalf("atteso anchor NodeByProperty su b, got %T %+v", scanOf(t, root), scanOf(t, root))
		}
	})
}

func TestExpandDirection(t *testing.T) {
	t.Run("outgoing-from-anchor", func(t *testing.T) {
		root := mustPlan(t, "MATCH (a:Person)-[:KNOWS]->(b) RETURN b", withIndex())
		e := firstExpand(root)
		if e == nil || e.From != "a" || e.To != "b" || e.Dir != ast.DirOut {
			t.Fatalf("Expand errato: %+v", e)
		}
	})

	t.Run("reversed-when-anchor-is-dst", func(t *testing.T) {
		// b ha la label (anchor), a no: si espande da b verso a invertendo la direzione.
		root := mustPlan(t, "MATCH (a)-[:KNOWS]->(b:Person) RETURN a", withIndex())
		e := firstExpand(root)
		if e == nil || e.From != "b" || e.To != "a" || e.Dir != ast.DirIn {
			t.Fatalf("Expand invertito errato: %+v", e)
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
	// b è raggiunto via Expand: la sua label dev'essere verificata in un Filter.
	root := mustPlan(t, "MATCH (a:Person)-[:KNOWS]->(b:Admin) RETURN b", withIndex())
	f := firstFilter(root)
	if f == nil || exprString(f.Pred) != "b:Admin" {
		t.Errorf("atteso Filter(b:Admin), got %v", f)
	}
}

func TestProjectionTail(t *testing.T) {
	root := mustPlan(t, "MATCH (a) RETURN a.name AS name ORDER BY name DESC SKIP 2 LIMIT 10", withIndex())
	// Dall'alto: Limit -> Skip -> Sort -> Project.
	lim, ok := root.(*Limit)
	if !ok {
		t.Fatalf("radice attesa Limit, got %T", root)
	}
	skip, ok := lim.Input.(*Skip)
	if !ok {
		t.Fatalf("atteso Skip, got %T", lim.Input)
	}
	sort, ok := skip.Input.(*Sort)
	if !ok {
		t.Fatalf("atteso Sort, got %T", skip.Input)
	}
	if _, ok := sort.Input.(*Project); !ok {
		t.Fatalf("atteso Project, got %T", sort.Input)
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
		t.Fatal("atteso un operatore Aggregate")
	}
	if len(agg.GroupKeys) != 1 || agg.GroupKeys[0].Column != "p" {
		t.Errorf("group keys errate: %+v", agg.GroupKeys)
	}
	if len(agg.Aggs) != 1 || agg.Aggs[0].Column != "c" {
		t.Errorf("aggs errate: %+v", agg.Aggs)
	}
}
