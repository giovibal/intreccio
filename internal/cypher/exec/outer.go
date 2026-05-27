package exec

import (
	"fmt"

	"github.com/giovibal/mycypher/internal/cypher/plan"
)

// argumentOp is the single-row source that backs the Inner subtree of an
// OuterApply: it emits the bound "outer row" exactly once and is then
// exhausted. A fresh argumentOp is built for each outer iteration.
type argumentOp struct {
	row  binding
	done bool
}

func (a *argumentOp) next() (binding, bool, error) {
	if a.done {
		return nil, false, nil
	}
	a.done = true
	return a.row, true, nil
}

// outerApplyOp implements the left-outer join used by OPTIONAL MATCH. For each
// outer row it builds a fresh inner exec tree (replacing the Argument leaf
// with an argumentOp seeded with the current row), then drains it. If the
// inner produces at least one row, those rows are emitted; otherwise the
// outer row is emitted once with NewVars bound to nil.
type outerApplyOp struct {
	ctx       *Context
	outer     op
	innerPlan plan.Op
	newVars   []string

	curOuter      binding
	curOuterValid bool
	curInner      op
	rowsEmitted   int
	pendingEmpty  bool
}

func (o *outerApplyOp) next() (binding, bool, error) {
	for {
		if o.pendingEmpty {
			o.pendingEmpty = false
			r := o.curOuter.clone()
			for _, v := range o.newVars {
				r[v] = nil
			}
			o.curOuterValid = false
			return r, true, nil
		}
		if o.curOuterValid {
			b, ok, err := o.curInner.next()
			if err != nil {
				return nil, false, err
			}
			if ok {
				o.rowsEmitted++
				return b, true, nil
			}
			if o.rowsEmitted == 0 {
				o.pendingEmpty = true
				continue
			}
			o.curOuterValid = false
		}

		b, ok, err := o.outer.next()
		if err != nil || !ok {
			return nil, ok, err
		}
		arg := &argumentOp{row: b}
		inner, err := buildWith(o.innerPlan, o.ctx, arg)
		if err != nil {
			return nil, false, err
		}
		o.curOuter = b
		o.curOuterValid = true
		o.curInner = inner
		o.rowsEmitted = 0
	}
}

// buildWith is the variant of build used inside an OuterApply: when it
// encounters the Argument plan operator it returns the supplied argumentOp.
// For all other operators it recursively replicates build() but threads the
// argument through every Input.
func buildWith(p plan.Op, ctx *Context, arg *argumentOp) (op, error) {
	if _, ok := p.(*plan.Argument); ok {
		if arg == nil {
			return nil, fmt.Errorf("exec: Argument outside an OuterApply")
		}
		return arg, nil
	}
	return buildChildren(p, ctx, arg)
}
