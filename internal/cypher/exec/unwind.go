package exec

import (
	"fmt"

	"github.com/giovibal/intreccio/query/ast"
)

// unwindOp expands a list expression into multiple rows: for each input row,
// it evaluates the expression and, for each element of the resulting list,
// emits a copy of the row with Alias bound to that element.
type unwindOp struct {
	ctx   *Context
	input op
	expr  ast.Expr
	alias string

	cur     binding
	pending []any
	pidx    int
}

func (u *unwindOp) next() (binding, bool, error) {
	for {
		if u.pidx < len(u.pending) {
			out := u.cur.clone()
			out[u.alias] = u.pending[u.pidx]
			u.pidx++
			return out, true, nil
		}
		b, ok, err := u.input.next()
		if err != nil || !ok {
			return nil, ok, err
		}
		v, err := eval(u.expr, b, u.ctx)
		if err != nil {
			return nil, false, err
		}
		if v == nil {
			// UNWIND null yields no rows.
			u.pending = nil
			continue
		}
		list, ok := v.([]any)
		if !ok {
			return nil, false, fmt.Errorf("exec: UNWIND expects a list, got %T", v)
		}
		u.cur = b
		u.pending = list
		u.pidx = 0
	}
}
