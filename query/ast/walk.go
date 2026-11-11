package ast

import "strings"

// Walk visits e and its sub-expressions top-down. Returning false from fn
// skips the children of the expression it was called with. A nil expression
// is not visited.
func Walk(e Expr, fn func(Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	for _, c := range children(e) {
		Walk(c, fn)
	}
}

// Rewrite returns a copy of e in which fn has replaced sub-expressions. fn is
// called top-down; when it returns true its result replaces the expression and
// that subtree is not visited further, otherwise the expression is kept and
// its children are rewritten. e itself is never modified: unchanged subtrees
// are shared between the input and the result.
func Rewrite(e Expr, fn func(Expr) (Expr, bool)) Expr {
	if e == nil {
		return nil
	}
	if repl, ok := fn(e); ok {
		return repl
	}
	re := func(c Expr) Expr { return Rewrite(c, fn) }
	switch x := e.(type) {
	case *PropertyAccess:
		return &PropertyAccess{Target: re(x.Target), Key: x.Key, Pos: x.Pos}
	case *Unary:
		return &Unary{Op: x.Op, Expr: re(x.Expr), Pos: x.Pos}
	case *Binary:
		return &Binary{Op: x.Op, Left: re(x.Left), Right: re(x.Right), Pos: x.Pos}
	case *FunctionCall:
		args := make([]Expr, len(x.Args))
		for i, a := range x.Args {
			args[i] = re(a)
		}
		return &FunctionCall{Name: x.Name, Distinct: x.Distinct, Star: x.Star, Args: args, Pos: x.Pos}
	case *LabelsPredicate:
		return &LabelsPredicate{Expr: re(x.Expr), Labels: x.Labels, Pos: x.Pos}
	case *ListLiteral:
		els := make([]Expr, len(x.Elements))
		for i, el := range x.Elements {
			els[i] = re(el)
		}
		return &ListLiteral{Elements: els, Pos: x.Pos}
	case *MapLiteral:
		entries := make(map[string]Expr, len(x.Entries))
		for k, v := range x.Entries {
			entries[k] = re(v)
		}
		return &MapLiteral{Entries: entries, Pos: x.Pos}
	case *Subscript:
		return &Subscript{Target: re(x.Target), Index: re(x.Index), Pos: x.Pos}
	case *Slice:
		return &Slice{Target: re(x.Target), From: re(x.From), To: re(x.To), Pos: x.Pos}
	case *ListComprehension:
		return &ListComprehension{Variable: x.Variable, List: re(x.List), Where: re(x.Where), Map: re(x.Map), Pos: x.Pos}
	case *Quantifier:
		return &Quantifier{Kind: x.Kind, Variable: x.Variable, List: re(x.List), Where: re(x.Where), Pos: x.Pos}
	case *Case:
		whens := make([]CaseAlternative, len(x.Whens))
		for i, w := range x.Whens {
			whens[i] = CaseAlternative{Cond: re(w.Cond), Result: re(w.Result)}
		}
		return &Case{Operand: re(x.Operand), Whens: whens, Else: re(x.Else), Pos: x.Pos}
	default:
		// Literal, Param, Variable: no children.
		return e
	}
}

// Equal reports whether a and b are the same expression, ignoring source
// positions. It compares structure, not text: (a + b) * c and a + b * c
// format alike but are not equal.
func Equal(a, b Expr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch x := a.(type) {
	case *Literal:
		y, ok := b.(*Literal)
		return ok && x.Value == y.Value
	case *Param:
		y, ok := b.(*Param)
		return ok && x.Name == y.Name
	case *Variable:
		y, ok := b.(*Variable)
		return ok && x.Name == y.Name
	case *PropertyAccess:
		y, ok := b.(*PropertyAccess)
		return ok && x.Key == y.Key && Equal(x.Target, y.Target)
	case *Unary:
		y, ok := b.(*Unary)
		return ok && x.Op == y.Op && Equal(x.Expr, y.Expr)
	case *Binary:
		y, ok := b.(*Binary)
		return ok && x.Op == y.Op && Equal(x.Left, y.Left) && Equal(x.Right, y.Right)
	case *FunctionCall:
		y, ok := b.(*FunctionCall)
		return ok && strings.EqualFold(x.Name, y.Name) && x.Distinct == y.Distinct && x.Star == y.Star &&
			equalList(x.Args, y.Args)
	case *LabelsPredicate:
		y, ok := b.(*LabelsPredicate)
		if !ok || len(x.Labels) != len(y.Labels) || !Equal(x.Expr, y.Expr) {
			return false
		}
		for i := range x.Labels {
			if x.Labels[i] != y.Labels[i] {
				return false
			}
		}
		return true
	case *ListLiteral:
		y, ok := b.(*ListLiteral)
		return ok && equalList(x.Elements, y.Elements)
	case *MapLiteral:
		y, ok := b.(*MapLiteral)
		if !ok || len(x.Entries) != len(y.Entries) {
			return false
		}
		for k, v := range x.Entries {
			w, ok := y.Entries[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	case *Subscript:
		y, ok := b.(*Subscript)
		return ok && Equal(x.Target, y.Target) && Equal(x.Index, y.Index)
	case *Slice:
		y, ok := b.(*Slice)
		return ok && Equal(x.Target, y.Target) && Equal(x.From, y.From) && Equal(x.To, y.To)
	case *ListComprehension:
		y, ok := b.(*ListComprehension)
		return ok && x.Variable == y.Variable && Equal(x.List, y.List) &&
			Equal(x.Where, y.Where) && Equal(x.Map, y.Map)
	case *Quantifier:
		y, ok := b.(*Quantifier)
		return ok && x.Kind == y.Kind && x.Variable == y.Variable &&
			Equal(x.List, y.List) && Equal(x.Where, y.Where)
	case *Case:
		y, ok := b.(*Case)
		if !ok || len(x.Whens) != len(y.Whens) || !Equal(x.Operand, y.Operand) || !Equal(x.Else, y.Else) {
			return false
		}
		for i := range x.Whens {
			if !Equal(x.Whens[i].Cond, y.Whens[i].Cond) || !Equal(x.Whens[i].Result, y.Whens[i].Result) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// children returns the direct sub-expressions of e, absent ones included as
// nil (Walk skips them).
func children(e Expr) []Expr {
	switch x := e.(type) {
	case *PropertyAccess:
		return []Expr{x.Target}
	case *Unary:
		return []Expr{x.Expr}
	case *Binary:
		return []Expr{x.Left, x.Right}
	case *FunctionCall:
		return x.Args
	case *LabelsPredicate:
		return []Expr{x.Expr}
	case *ListLiteral:
		return x.Elements
	case *MapLiteral:
		out := make([]Expr, 0, len(x.Entries))
		for _, v := range x.Entries {
			out = append(out, v)
		}
		return out
	case *Subscript:
		return []Expr{x.Target, x.Index}
	case *Slice:
		return []Expr{x.Target, x.From, x.To}
	case *ListComprehension:
		return []Expr{x.List, x.Where, x.Map}
	case *Quantifier:
		return []Expr{x.List, x.Where}
	case *Case:
		out := []Expr{x.Operand}
		for _, w := range x.Whens {
			out = append(out, w.Cond, w.Result)
		}
		return append(out, x.Else)
	default:
		return nil
	}
}

func equalList(a, b []Expr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
