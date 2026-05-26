package exec

import (
	"fmt"
	"strings"

	"github.com/giovibal/mycypher/internal/cypher/ast"
	"github.com/giovibal/mycypher/internal/cypher/plan"
	"github.com/giovibal/mycypher/internal/graph"
)

// aggregateOp implements grouped aggregation. It reads all input rows, places
// each into a group keyed by the GroupKeys, accumulates the aggregate functions
// per group and then emits one row per group with the projected columns.
//
// When there are no group keys the operator emits a single row even if the
// input is empty (e.g. `RETURN count(*)` against an empty graph returns 0).
type aggregateOp struct {
	ctx       *Context
	input     op
	groupKeys []plan.ProjItem
	aggs      []plan.ProjItem

	loaded bool
	out    []binding
	pos    int
}

func (a *aggregateOp) next() (binding, bool, error) {
	if !a.loaded {
		if err := a.load(); err != nil {
			return nil, false, err
		}
		a.loaded = true
	}
	if a.pos >= len(a.out) {
		return nil, false, nil
	}
	r := a.out[a.pos]
	a.pos++
	return r, true, nil
}

func (a *aggregateOp) load() error {
	specs, err := parseAggFuncs(a.aggs)
	if err != nil {
		return err
	}

	type group struct {
		keys []any
		accs []*accumulator
	}
	groups := map[string]*group{}
	var order []string

	// When there are no group keys, pre-seed the single group so that an empty
	// input still produces one output row.
	if len(a.groupKeys) == 0 {
		groups[""] = &group{keys: nil, accs: newAccumulators(specs)}
		order = append(order, "")
	}

	for {
		b, ok, err := a.input.next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		keyVals := make([]any, len(a.groupKeys))
		for i, gk := range a.groupKeys {
			v, err := eval(gk.Expr, b, a.ctx)
			if err != nil {
				return err
			}
			keyVals[i] = v
		}
		key := canonKeyList(keyVals)

		g, exists := groups[key]
		if !exists {
			g = &group{keys: keyVals, accs: newAccumulators(specs)}
			groups[key] = g
			order = append(order, key)
		}
		for i, s := range specs {
			if err := g.accs[i].add(b, s, a.ctx); err != nil {
				return err
			}
		}
	}

	for _, key := range order {
		g := groups[key]
		row := make(binding, len(a.groupKeys)+len(a.aggs))
		for i, gk := range a.groupKeys {
			row[gk.Column] = g.keys[i]
		}
		for i, it := range a.aggs {
			row[it.Column] = g.accs[i].result(specs[i].kind)
		}
		a.out = append(a.out, row)
	}
	return nil
}

// --- accumulators ---

type aggKind int

const (
	aggCount aggKind = iota
	aggSum
	aggAvg
	aggMin
	aggMax
	aggCollect
)

type aggSpec struct {
	kind     aggKind
	arg      ast.Expr // nil for count(*)
	star     bool
	distinct bool
}

type accumulator struct {
	count  int64
	sum    float64
	intSum int64
	isInt  bool // true while no float has been seen and the running sum fits in int64
	minVal any
	maxVal any
	list   []any
	seen   map[string]struct{} // for DISTINCT
}

func newAccumulators(specs []aggSpec) []*accumulator {
	out := make([]*accumulator, len(specs))
	for i, s := range specs {
		a := &accumulator{}
		if s.kind == aggSum {
			a.isInt = true
		}
		if s.distinct {
			a.seen = map[string]struct{}{}
		}
		out[i] = a
	}
	return out
}

func (a *accumulator) add(row binding, s aggSpec, ctx *Context) error {
	if s.star {
		a.count++
		return nil
	}
	v, err := eval(s.arg, row, ctx)
	if err != nil {
		return err
	}
	if v == nil {
		return nil // null values are skipped by every aggregate
	}
	if s.distinct {
		k := canonKey(v)
		if _, dup := a.seen[k]; dup {
			return nil
		}
		a.seen[k] = struct{}{}
	}
	switch s.kind {
	case aggCount:
		a.count++
	case aggSum:
		switch n := v.(type) {
		case int64:
			if a.isInt {
				a.intSum += n
			} else {
				a.sum += float64(n)
			}
		case float64:
			if a.isInt {
				a.sum = float64(a.intSum) + n
				a.isInt = false
			} else {
				a.sum += n
			}
		default:
			return fmt.Errorf("sum: expected a number, got %T", v)
		}
	case aggAvg:
		f, ok := toFloat(v)
		if !ok {
			return fmt.Errorf("avg: expected a number, got %T", v)
		}
		a.sum += f
		a.count++
	case aggMin:
		if a.minVal == nil {
			a.minVal = v
		} else if c, ok := compareValues(v, a.minVal); ok && c < 0 {
			a.minVal = v
		}
	case aggMax:
		if a.maxVal == nil {
			a.maxVal = v
		} else if c, ok := compareValues(v, a.maxVal); ok && c > 0 {
			a.maxVal = v
		}
	case aggCollect:
		a.list = append(a.list, v)
	}
	return nil
}

func (a *accumulator) result(kind aggKind) any {
	switch kind {
	case aggCount:
		return a.count
	case aggSum:
		if a.isInt {
			return a.intSum
		}
		return a.sum
	case aggAvg:
		if a.count == 0 {
			return nil
		}
		return a.sum / float64(a.count)
	case aggMin:
		return a.minVal
	case aggMax:
		return a.maxVal
	case aggCollect:
		if a.list == nil {
			return []any{}
		}
		return a.list
	}
	return nil
}

// parseAggFuncs validates each aggregate ProjItem and extracts its spec. For
// the MVP each agg item's Expr must be a direct *ast.FunctionCall whose name
// is a recognised aggregate (count/sum/avg/min/max/collect); compound
// expressions like `count(x) + 1` are not yet supported.
func parseAggFuncs(items []plan.ProjItem) ([]aggSpec, error) {
	out := make([]aggSpec, len(items))
	for i, it := range items {
		fc, ok := it.Expr.(*ast.FunctionCall)
		if !ok {
			return nil, fmt.Errorf("aggregate %q must be a direct aggregate function call", it.Column)
		}
		kind, ok := aggKindOf(fc.Name)
		if !ok {
			return nil, fmt.Errorf("unknown aggregate function %s", fc.Name)
		}
		s := aggSpec{kind: kind, distinct: fc.Distinct, star: fc.Star}
		if fc.Star {
			if kind != aggCount {
				return nil, fmt.Errorf("%s(*) is not supported", fc.Name)
			}
		} else {
			if len(fc.Args) != 1 {
				return nil, fmt.Errorf("%s expects exactly one argument", fc.Name)
			}
			s.arg = fc.Args[0]
		}
		out[i] = s
	}
	return out, nil
}

func aggKindOf(name string) (aggKind, bool) {
	switch strings.ToLower(name) {
	case "count":
		return aggCount, true
	case "sum":
		return aggSum, true
	case "avg":
		return aggAvg, true
	case "min":
		return aggMin, true
	case "max":
		return aggMax, true
	case "collect":
		return aggCollect, true
	}
	return 0, false
}

// canonKey renders a single value into a string that uniquely identifies it for
// grouping/dedup purposes.
func canonKey(v any) string {
	switch x := v.(type) {
	case nil:
		return "n"
	case bool:
		if x {
			return "b:1"
		}
		return "b:0"
	case int64:
		return fmt.Sprintf("i:%d", x)
	case float64:
		return fmt.Sprintf("f:%g", x)
	case string:
		return "s:" + x
	case graph.Node:
		return fmt.Sprintf("N:%d", x.ID)
	case graph.Edge:
		return fmt.Sprintf("E:%d", x.ID)
	default:
		return fmt.Sprintf("?:%v", v)
	}
}

func canonKeyList(vs []any) string {
	var b strings.Builder
	for i, v := range vs {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(canonKey(v))
	}
	return b.String()
}
