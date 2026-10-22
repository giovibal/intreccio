package exec

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/query/ast"
	"github.com/giovibal/intreccio/query/plan"
)

// ErrResultLimit is returned when a query produces more rows than the configured
// maximum (Context.MaxRows).
var ErrResultLimit = fmt.Errorf("exec: query result exceeds the configured row limit")

// Context carries the execution state: the transaction, the query parameters, an
// optional cancellation context and an optional result-row cap.
type Context struct {
	Txn    storage.Txn
	Params map[string]any
	// Ctx, when non-nil, cancels execution when it is done (deadline or manual
	// cancellation). Operators poll it in their inner loops so long queries
	// return promptly.
	Ctx context.Context
	// MaxRows, when > 0, bounds the number of rows a query may produce. Exceeding
	// it aborts with ErrResultLimit.
	MaxRows int64

	// cursors are the streaming scans opened by this execution. Run closes
	// them all when it returns, because the storage engine requires every
	// iterator to be closed before its transaction ends.
	cursors []*graph.NodeCursor

	// keyIDs and labelIDs cache dictionary lookups for this execution.
	// Dictionary entries are never removed, so a found ID stays valid for
	// the whole transaction; misses are not cached because the query itself
	// may intern the name later.
	keyIDs   map[string]uint32
	labelIDs map[string]uint32

	regexps map[string]*regexp.Regexp // compiled =~ patterns
	now     int64                     // timestamp(), fixed for the execution
}

// regexp compiles a =~ pattern once per execution. The pattern must match
// the whole string, as in openCypher.
func (c *Context) regexp(pattern string) (*regexp.Regexp, error) {
	if re, ok := c.regexps[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return nil, fmt.Errorf("exec: invalid regular expression %q: %w", pattern, err)
	}
	if c.regexps == nil {
		c.regexps = map[string]*regexp.Regexp{}
	}
	c.regexps[pattern] = re
	return re, nil
}

// timestamp returns the execution's timestamp in milliseconds since the
// epoch: every call within one query sees the same value.
func (c *Context) timestamp() int64 {
	if c.now == 0 {
		c.now = time.Now().UnixMilli()
	}
	return c.now
}

// lookupKey resolves a property-key name, caching the result.
func (c *Context) lookupKey(name string) (uint32, bool, error) {
	if id, ok := c.keyIDs[name]; ok {
		return id, true, nil
	}
	id, found, err := catalog.LookupKey(c.Txn, name)
	if err == nil && found {
		if c.keyIDs == nil {
			c.keyIDs = map[string]uint32{}
		}
		c.keyIDs[name] = id
	}
	return id, found, err
}

// lookupLabel resolves a label name, caching the result.
func (c *Context) lookupLabel(name string) (uint32, bool, error) {
	if id, ok := c.labelIDs[name]; ok {
		return id, true, nil
	}
	id, found, err := catalog.LookupLabel(c.Txn, name)
	if err == nil && found {
		if c.labelIDs == nil {
			c.labelIDs = map[string]uint32{}
		}
		c.labelIDs[name] = id
	}
	return id, found, err
}

// track registers an open cursor so Run can close it. Exhausted cursors close
// themselves; the list is compacted so per-row inner scans (Apply) do not grow
// it without bound.
func (c *Context) track(cur *graph.NodeCursor) {
	if len(c.cursors) >= 64 {
		live := c.cursors[:0]
		for _, open := range c.cursors {
			if !open.Closed() {
				live = append(live, open)
			}
		}
		c.cursors = live
	}
	c.cursors = append(c.cursors, cur)
}

// closeCursors closes every cursor opened by this execution.
func (c *Context) closeCursors() {
	for _, cur := range c.cursors {
		_ = cur.Close()
	}
	c.cursors = nil
}

// check reports whether execution should stop because Ctx is done.
func (c *Context) check() error {
	if c.Ctx == nil {
		return nil
	}
	return c.Ctx.Err()
}

// op is a Volcano-style iterator: next returns the next row, false at the end.
type op interface {
	next() (binding, bool, error)
}

// Run builds the operator tree for root and drains it, returning the rows mapped
// to the given output columns.
func Run(root plan.Op, columns []string, ctx *Context) ([][]any, error) {
	defer ctx.closeCursors()
	o, err := buildWith(root, ctx, nil)
	if err != nil {
		return nil, err
	}
	// Write-only queries have no output columns: drain for side effects.
	if len(columns) == 0 {
		for {
			if err := ctx.check(); err != nil {
				return nil, err
			}
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
		if err := ctx.check(); err != nil {
			return nil, err
		}
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
		if ctx.MaxRows > 0 && int64(len(rows)) >= ctx.MaxRows {
			// Stop only once the cap is reached and more rows are available.
			if _, more, err := o.next(); err != nil {
				return nil, err
			} else if more {
				return nil, ErrResultLimit
			}
			break
		}
	}
	return rows, nil
}

// buildChildren constructs the exec operator for a non-Argument plan node and
// recursively delegates child construction to buildWith (so any nested
// Argument leaves are replaced by the supplied argumentOp).
func buildChildren(p plan.Op, ctx *Context, arg *argumentOp) (op, error) {
	switch x := p.(type) {
	case nil:
		return &unit{}, nil
	case *plan.AllNodesScan:
		cur := graph.ScanAllNodes(ctx.Txn)
		ctx.track(cur)
		return &nodeScan{ctx: ctx, variable: x.Var, cursor: cur}, nil
	case *plan.NodeByLabelScan:
		id, found, err := catalog.LookupLabel(ctx.Txn, x.Label)
		if err != nil {
			return nil, err
		}
		if !found {
			return &nodeScan{ctx: ctx, variable: x.Var}, nil // unknown label: no rows
		}
		cur := graph.ScanLabel(ctx.Txn, id)
		ctx.track(cur)
		return &nodeScan{ctx: ctx, variable: x.Var, cursor: cur}, nil
	case *plan.NodeByPropertyRange:
		var row binding
		if arg != nil {
			row = arg.row // inside an Apply: the bounds may use the outer row
		}
		return propertyRangeScan(ctx, x, row)
	case *plan.NodeByProperty:
		var row binding
		if arg != nil {
			row = arg.row // inside an Apply: the seek value may use the outer row
		}
		ids, err := propertyScanIDs(ctx, x, row)
		if err != nil {
			return nil, err
		}
		return &nodeScan{ctx: ctx, variable: x.Var, ids: ids}, nil
	case *plan.Expand:
		return buildExpandWith(x, ctx, arg)
	case *plan.ShortestPath:
		return buildShortestPath(x, ctx, arg)
	case *plan.Filter:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &filter{ctx: ctx, input: in, pred: x.Pred}, nil
	case *plan.Project:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		pr := &project{ctx: ctx, input: in, items: x.Items, distinct: x.Distinct}
		if x.Distinct {
			pr.seen = map[string]bool{}
		}
		return pr, nil
	case *plan.Sort:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &sortOp{ctx: ctx, input: in, keys: x.Keys}, nil
	case *plan.Skip:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		n, err := evalCount(x.Count, ctx)
		if err != nil {
			return nil, err
		}
		return &skip{ctx: ctx, input: in, n: n}, nil
	case *plan.Limit:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		n, err := evalCount(x.Count, ctx)
		if err != nil {
			return nil, err
		}
		return &limit{ctx: ctx, input: in, n: n}, nil
	case *plan.CartesianProduct:
		left, err := buildWith(x.Left, ctx, arg)
		if err != nil {
			return nil, err
		}
		right, err := buildWith(x.Right, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &cartesian{ctx: ctx, left: left, right: right}, nil
	case *plan.Aggregate:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &aggregateOp{ctx: ctx, input: in, groupKeys: x.GroupKeys, aggs: x.Aggs}, nil
	case *plan.Create:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &createOp{ctx: ctx, input: in, parts: x.Parts}, nil
	case *plan.Merge:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &mergeOp{ctx: ctx, input: in, part: x.Part, onCreate: x.OnCreate, onMatch: x.OnMatch}, nil
	case *plan.SetItems:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &setOp{ctx: ctx, input: in, items: x.Items}, nil
	case *plan.Remove:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &removeOp{ctx: ctx, input: in, items: x.Items}, nil
	case *plan.Unwind:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &unwindOp{ctx: ctx, input: in, expr: x.Expr, alias: x.Alias}, nil
	case *plan.Delete:
		in, err := buildWith(x.Input, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &deleteOp{ctx: ctx, input: in, exprs: x.Exprs, detach: x.Detach}, nil
	case *plan.CreateIndex:
		return &createIndexOp{ctx: ctx, name: x.Name, label: x.Label, prop: x.Property}, nil
	case *plan.DropIndex:
		return &dropIndexOp{ctx: ctx, name: x.Name, ifExists: x.IfExists}, nil
	case *plan.ShowIndexes:
		return &showIndexesOp{ctx: ctx}, nil
	case *plan.CreateConstraint:
		return &createConstraintOp{ctx: ctx, name: x.Name, label: x.Label, prop: x.Property}, nil
	case *plan.DropConstraint:
		return &dropConstraintOp{ctx: ctx, name: x.Name, ifExists: x.IfExists}, nil
	case *plan.ShowConstraints:
		return &showConstraintsOp{ctx: ctx}, nil
	case *plan.Apply:
		outer, err := buildWith(x.Outer, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &outerApplyOp{ctx: ctx, outer: outer, innerPlan: x.Inner, inner: true}, nil
	case *plan.OuterApply:
		outer, err := buildWith(x.Outer, ctx, arg)
		if err != nil {
			return nil, err
		}
		return &outerApplyOp{
			ctx:       ctx,
			outer:     outer,
			innerPlan: x.Inner,
			newVars:   x.NewVars,
		}, nil
	case *plan.Union:
		parts := make([]op, len(x.Parts))
		for i, p := range x.Parts {
			part, err := buildWith(p, ctx, arg)
			if err != nil {
				return nil, err
			}
			parts[i] = part
		}
		u := &unionOp{ctx: ctx, parts: parts, columns: x.Columns, dedup: !x.All}
		if u.dedup {
			u.seen = map[string]struct{}{}
		}
		return u, nil
	default:
		return nil, fmt.Errorf("exec: unsupported operator %T", p)
	}
}

// propertyRangeScan streams the candidates of an index range scan. The
// predicates it came from stay in the plan as a filter, so the scan only has
// to return a superset of the matching nodes.
func propertyRangeScan(ctx *Context, x *plan.NodeByPropertyRange, row binding) (op, error) {
	empty := &nodeScan{ctx: ctx, variable: x.Var}
	labelID, found, err := catalog.LookupLabel(ctx.Txn, x.Label)
	if err != nil || !found {
		return empty, err
	}
	keyID, found, err := catalog.LookupKey(ctx.Txn, x.Key)
	if err != nil || !found {
		return empty, err
	}
	if row == nil {
		row = binding{}
	}
	var r graph.PropRange
	if x.Prefix != nil {
		v, err := eval(x.Prefix, row, ctx)
		if err != nil {
			return nil, err
		}
		p, ok := v.(string)
		if !ok {
			return empty, nil // STARTS WITH a non-string (or null) is never true
		}
		r.Prefix, r.HasPrefix = p, true
	}
	if x.Lower != nil {
		if r.Lower, err = eval(x.Lower, row, ctx); err != nil {
			return nil, err
		}
		r.HasLower, r.LowerInclusive = true, x.LowerInclusive
	}
	if x.Upper != nil {
		if r.Upper, err = eval(x.Upper, row, ctx); err != nil {
			return nil, err
		}
		r.HasUpper, r.UpperInclusive = true, x.UpperInclusive
	}
	cur, err := graph.ScanPropertyRange(ctx.Txn, labelID, keyID, r)
	if err != nil || cur == nil {
		return empty, err
	}
	ctx.track(cur)
	return &nodeScan{ctx: ctx, variable: x.Var, cursor: cur}, nil
}

func propertyScanIDs(ctx *Context, np *plan.NodeByProperty, row binding) ([]uint64, error) {
	labelID, found, err := catalog.LookupLabel(ctx.Txn, np.Label)
	if err != nil || !found {
		return nil, err
	}
	keyID, found, err := catalog.LookupKey(ctx.Txn, np.Key)
	if err != nil || !found {
		return nil, err
	}
	if row == nil {
		row = binding{}
	}
	value, err := eval(np.Value, row, ctx)
	if err != nil {
		return nil, err
	}
	value = normalize(value) // a Go int is an indexable integer
	if value == nil {
		return nil, nil // x = null is never true
	}
	if !graph.IsIndexable(value) {
		// Lists and maps have no index keys: scan the label and compare with
		// the query equality, so the result matches the non-indexed path.
		return nodesWithEqualProperty(ctx, labelID, keyID, value)
	}
	return graph.NodesByProperty(ctx.Txn, labelID, keyID, value)
}

func nodesWithEqualProperty(ctx *Context, labelID, keyID uint32, value any) ([]uint64, error) {
	ids, err := graph.NodesByLabel(ctx.Txn, labelID)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, id := range ids {
		node, err := graph.GetNode(ctx.Txn, id)
		if err != nil {
			return nil, err
		}
		if v, ok := node.Props[keyID]; ok && equals(v, value) {
			out = append(out, id)
		}
	}
	return out, nil
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
// nodeScan binds variable to each node of a scan. A streaming scan reads from
// cursor; an index lookup (a short, already-known ID list) reads from ids.
type nodeScan struct {
	ctx      *Context
	variable string
	cursor   *graph.NodeCursor
	ids      []uint64
	pos      int
}

func (s *nodeScan) next() (binding, bool, error) {
	if err := s.ctx.check(); err != nil {
		return nil, false, err
	}
	if s.cursor != nil {
		node, ok, err := s.cursor.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		return binding{s.variable: node}, true, nil
	}
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
		if err := f.ctx.check(); err != nil {
			return nil, false, err
		}
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
		if err := p.ctx.check(); err != nil {
			return nil, false, err
		}
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
		if err := s.ctx.check(); err != nil {
			s.err = err
			return
		}
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
	ctx     *Context
	input   op
	n       int64
	skipped bool
}

func (s *skip) next() (binding, bool, error) {
	if !s.skipped {
		s.skipped = true
		for i := int64(0); i < s.n; i++ {
			if err := s.ctx.check(); err != nil {
				return nil, false, err
			}
			_, ok, err := s.input.next()
			if err != nil || !ok {
				return nil, ok, err
			}
		}
	}
	return s.input.next()
}

type limit struct {
	ctx   *Context
	input op
	n     int64
	count int64
}

func (l *limit) next() (binding, bool, error) {
	if err := l.ctx.check(); err != nil {
		return nil, false, err
	}
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
	ctx       *Context
	left      op
	right     op
	rightRows []binding
	loaded    bool
	curLeft   binding
	haveLeft  bool
	ri        int
}

func (c *cartesian) next() (binding, bool, error) {
	// Read the right side only once a left row exists: with no left rows the
	// product is empty and the right scan is never needed.
	if !c.loaded && !c.haveLeft {
		b, ok, err := c.left.next()
		if err != nil || !ok {
			return nil, ok, err
		}
		c.curLeft = b
		c.haveLeft = true
		c.ri = 0
	}
	if !c.loaded {
		for {
			if err := c.ctx.check(); err != nil {
				return nil, false, err
			}
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
		if err := c.ctx.check(); err != nil {
			return nil, false, err
		}
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
