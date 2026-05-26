package exec

import (
	"fmt"
	"sort"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/cypher/ast"
	"github.com/giovibal/mycypher/internal/cypher/plan"
	"github.com/giovibal/mycypher/internal/graph"
	"github.com/giovibal/mycypher/internal/storage"
)

// Context carries the execution state: the transaction and the query parameters.
type Context struct {
	Txn    storage.Txn
	Params map[string]any
}

// op is a Volcano-style iterator: next returns the next row, false at the end.
type op interface {
	next() (binding, bool, error)
}

// Run builds the operator tree for root and drains it, returning the rows mapped
// to the given output columns.
func Run(root plan.Op, columns []string, ctx *Context) ([][]any, error) {
	o, err := build(root, ctx)
	if err != nil {
		return nil, err
	}
	// Write-only queries have no output columns: drain for side effects.
	if len(columns) == 0 {
		for {
			_, ok, err := o.next()
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, nil
			}
		}
	}
	var rows [][]any
	for {
		b, ok, err := o.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		row := make([]any, len(columns))
		for i, c := range columns {
			row[i] = b[c]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func build(p plan.Op, ctx *Context) (op, error) {
	switch x := p.(type) {
	case nil:
		return &unit{}, nil
	case *plan.AllNodesScan:
		ids, err := graph.AllNodes(ctx.Txn)
		if err != nil {
			return nil, err
		}
		return &nodeScan{ctx: ctx, variable: x.Var, ids: ids}, nil
	case *plan.NodeByLabelScan:
		ids, err := labelScanIDs(ctx, x.Label)
		if err != nil {
			return nil, err
		}
		return &nodeScan{ctx: ctx, variable: x.Var, ids: ids}, nil
	case *plan.NodeByProperty:
		ids, err := propertyScanIDs(ctx, x)
		if err != nil {
			return nil, err
		}
		return &nodeScan{ctx: ctx, variable: x.Var, ids: ids}, nil
	case *plan.Expand:
		return buildExpand(x, ctx)
	case *plan.Filter:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		return &filter{ctx: ctx, input: in, pred: x.Pred}, nil
	case *plan.Project:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		pr := &project{ctx: ctx, input: in, items: x.Items, distinct: x.Distinct}
		if x.Distinct {
			pr.seen = map[string]bool{}
		}
		return pr, nil
	case *plan.Sort:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		return &sortOp{ctx: ctx, input: in, keys: x.Keys}, nil
	case *plan.Skip:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		n, err := evalCount(x.Count, ctx)
		if err != nil {
			return nil, err
		}
		return &skip{input: in, n: n}, nil
	case *plan.Limit:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		n, err := evalCount(x.Count, ctx)
		if err != nil {
			return nil, err
		}
		return &limit{input: in, n: n}, nil
	case *plan.CartesianProduct:
		left, err := build(x.Left, ctx)
		if err != nil {
			return nil, err
		}
		right, err := build(x.Right, ctx)
		if err != nil {
			return nil, err
		}
		return &cartesian{left: left, right: right}, nil
	case *plan.Aggregate:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		return &aggregateOp{ctx: ctx, input: in, groupKeys: x.GroupKeys, aggs: x.Aggs}, nil
	case *plan.Create:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		return &createOp{ctx: ctx, input: in, parts: x.Parts}, nil
	case *plan.Merge:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		return &mergeOp{ctx: ctx, input: in, part: x.Part}, nil
	case *plan.SetItems:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		return &setOp{ctx: ctx, input: in, items: x.Items}, nil
	case *plan.Delete:
		in, err := build(x.Input, ctx)
		if err != nil {
			return nil, err
		}
		return &deleteOp{ctx: ctx, input: in, exprs: x.Exprs, detach: x.Detach}, nil
	case *plan.CreateIndex:
		return &createIndexOp{ctx: ctx, label: x.Label, prop: x.Property}, nil
	default:
		return nil, fmt.Errorf("exec: unsupported operator %T", p)
	}
}

func labelScanIDs(ctx *Context, label string) ([]uint64, error) {
	id, found, err := catalog.LookupLabel(ctx.Txn, label)
	if err != nil || !found {
		return nil, err
	}
	return graph.NodesByLabel(ctx.Txn, id)
}

func propertyScanIDs(ctx *Context, np *plan.NodeByProperty) ([]uint64, error) {
	labelID, found, err := catalog.LookupLabel(ctx.Txn, np.Label)
	if err != nil || !found {
		return nil, err
	}
	keyID, found, err := catalog.LookupKey(ctx.Txn, np.Key)
	if err != nil || !found {
		return nil, err
	}
	value, err := eval(np.Value, binding{}, ctx)
	if err != nil {
		return nil, err
	}
	return graph.NodesByProperty(ctx.Txn, labelID, keyID, value)
}

func evalCount(e ast.Expr, ctx *Context) (int64, error) {
	v, err := eval(e, binding{}, ctx)
	if err != nil {
		return 0, err
	}
	n, ok := normalize(v).(int64)
	if !ok {
		return 0, fmt.Errorf("exec: SKIP/LIMIT requires an integer, got %T", v)
	}
	return n, nil
}

// unit emits a single empty row (source for queries without a scan, e.g. RETURN 1).
type unit struct{ done bool }

func (u *unit) next() (binding, bool, error) {
	if u.done {
		return nil, false, nil
	}
	u.done = true
	return binding{}, true, nil
}

// nodeScan binds variable to each node in ids, fetching the record.
type nodeScan struct {
	ctx      *Context
	variable string
	ids      []uint64
	pos      int
}

func (s *nodeScan) next() (binding, bool, error) {
	if s.pos >= len(s.ids) {
		return nil, false, nil
	}
	id := s.ids[s.pos]
	s.pos++
	node, err := graph.GetNode(s.ctx.Txn, id)
	if err != nil {
		return nil, false, err
	}
	return binding{s.variable: node}, true, nil
}

type filter struct {
	ctx   *Context
	input op
	pred  ast.Expr
}

func (f *filter) next() (binding, bool, error) {
	for {
		b, ok, err := f.input.next()
		if err != nil || !ok {
			return nil, ok, err
		}
		v, err := eval(f.pred, b, f.ctx)
		if err != nil {
			return nil, false, err
		}
		if truthy(v) {
			return b, true, nil
		}
	}
}

type project struct {
	ctx      *Context
	input    op
	items    []plan.ProjItem
	distinct bool
	seen     map[string]bool
}

func (p *project) next() (binding, bool, error) {
	for {
		b, ok, err := p.input.next()
		if err != nil || !ok {
			return nil, ok, err
		}
		out := make(binding, len(p.items))
		for _, it := range p.items {
			v, err := eval(it.Expr, b, p.ctx)
			if err != nil {
				return nil, false, err
			}
			out[it.Column] = v
		}
		if p.distinct {
			key := rowKey(out, p.items)
			if p.seen[key] {
				continue
			}
			p.seen[key] = true
		}
		return out, true, nil
	}
}

func rowKey(b binding, items []plan.ProjItem) string {
	parts := make([]any, len(items))
	for i, it := range items {
		parts[i] = b[it.Column]
	}
	return fmt.Sprintf("%v", parts)
}

type sortOp struct {
	ctx    *Context
	input  op
	keys   []plan.SortKey
	rows   []binding
	pos    int
	loaded bool
	err    error
}

func (s *sortOp) next() (binding, bool, error) {
	if !s.loaded {
		s.load()
	}
	if s.err != nil {
		return nil, false, s.err
	}
	if s.pos >= len(s.rows) {
		return nil, false, nil
	}
	r := s.rows[s.pos]
	s.pos++
	return r, true, nil
}

func (s *sortOp) load() {
	s.loaded = true
	for {
		b, ok, err := s.input.next()
		if err != nil {
			s.err = err
			return
		}
		if !ok {
			break
		}
		s.rows = append(s.rows, b)
	}
	sort.SliceStable(s.rows, func(i, j int) bool {
		for _, k := range s.keys {
			vi := s.sortValue(s.rows[i], k.Expr)
			vj := s.sortValue(s.rows[j], k.Expr)
			c, ok := compareValues(vi, vj)
			if !ok || c == 0 {
				continue
			}
			if k.Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
}

// sortValue resolves a sort key: if it names an output column it uses that value,
// otherwise it evaluates the expression against the row.
func (s *sortOp) sortValue(b binding, e ast.Expr) any {
	if col := columnKey(e); col != "" {
		if v, ok := b[col]; ok {
			return v
		}
	}
	v, err := eval(e, b, s.ctx)
	if err != nil {
		return nil
	}
	return v
}

func columnKey(e ast.Expr) string {
	switch ex := e.(type) {
	case *ast.Variable:
		return ex.Name
	case *ast.PropertyAccess:
		if v, ok := ex.Target.(*ast.Variable); ok {
			return v.Name + "." + ex.Key
		}
	}
	return ""
}

type skip struct {
	input   op
	n       int64
	skipped bool
}

func (s *skip) next() (binding, bool, error) {
	if !s.skipped {
		s.skipped = true
		for i := int64(0); i < s.n; i++ {
			_, ok, err := s.input.next()
			if err != nil || !ok {
				return nil, ok, err
			}
		}
	}
	return s.input.next()
}

type limit struct {
	input op
	n     int64
	count int64
}

func (l *limit) next() (binding, bool, error) {
	if l.count >= l.n {
		return nil, false, nil
	}
	b, ok, err := l.input.next()
	if err != nil || !ok {
		return nil, ok, err
	}
	l.count++
	return b, true, nil
}

// cartesian is a nested-loop product: for each left row, all right rows.
type cartesian struct {
	left      op
	right     op
	rightRows []binding
	loaded    bool
	curLeft   binding
	haveLeft  bool
	ri        int
}

func (c *cartesian) next() (binding, bool, error) {
	if !c.loaded {
		for {
			b, ok, err := c.right.next()
			if err != nil {
				return nil, false, err
			}
			if !ok {
				break
			}
			c.rightRows = append(c.rightRows, b)
		}
		c.loaded = true
	}
	for {
		if !c.haveLeft {
			b, ok, err := c.left.next()
			if err != nil || !ok {
				return nil, ok, err
			}
			c.curLeft = b
			c.haveLeft = true
			c.ri = 0
		}
		if c.ri >= len(c.rightRows) {
			c.haveLeft = false
			continue
		}
		r := c.rightRows[c.ri]
		c.ri++
		out := c.curLeft.clone()
		for k, v := range r {
			out[k] = v
		}
		return out, true, nil
	}
}
