// Package plan translates the resolved AST into a physical operator plan
// (iterator/Volcano model) using a rule-based planner (DESIGN §9). In v1 there is
// no cost-based planning: the rules produce operators bound to access methods
// directly.
package plan

import "github.com/giovibal/mycypher/internal/cypher/ast"

// Op is a plan operator.
type Op interface{ op() }

// --- Access methods (leaves) ---

// AllNodesScan: scan of all nodes (fallback, `n`).
type AllNodesScan struct{ Var string }

// NodeByLabelScan: scan of the nodes with a label (`l`).
type NodeByLabelScan struct {
	Var   string
	Label string
}

// NodeByProperty: scan via the secondary index on equality (`p`).
type NodeByProperty struct {
	Var   string
	Label string
	Key   string
	Value ast.Expr
}

// --- Internal operators ---

// Expand: starting from the From nodes, follows the edges and produces the To nodes.
type Expand struct {
	Input     Op
	From      string
	Rel       string // relationship variable (may be synthetic)
	To        string
	Types     []string
	Dir       ast.Direction
	VarLength bool
	MinHops   int
	MaxHops   int
	ToBound   bool // true if To is already bound: the expand checks the equality
}

// Filter: applies a predicate to the stream.
type Filter struct {
	Input Op
	Pred  ast.Expr
}

// Project: projection (RETURN/WITH without aggregations).
type Project struct {
	Input    Op
	Items    []ProjItem
	Distinct bool
}

// ProjItem is a projection item with the resolved column name.
type ProjItem struct {
	Expr   ast.Expr
	Column string
}

// Aggregate: implicit grouping (keys = non-aggregated items). Full execution is
// Phase 8; here the planner produces it to make it inspectable.
type Aggregate struct {
	Input     Op
	GroupKeys []ProjItem
	Aggs      []ProjItem
	Distinct  bool
}

// Sort: ORDER BY.
type Sort struct {
	Input Op
	Keys  []SortKey
}

// SortKey is a sort criterion.
type SortKey struct {
	Expr ast.Expr
	Desc bool
}

// Skip: skips the first N rows.
type Skip struct {
	Input Op
	Count ast.Expr
}

// Limit: limits to N rows.
type Limit struct {
	Input Op
	Count ast.Expr
}

// CartesianProduct: cartesian product between two disconnected subplans.
type CartesianProduct struct {
	Left  Op
	Right Op
}

// --- Write operators ---

// Create: creates the given patterns for each input row.
type Create struct {
	Input Op
	Parts []ast.PatternPart
}

// Merge: match-or-create for a single pattern, per input row. The optional
// OnCreate/OnMatch SET items are applied to the resulting bindings after the
// match-or-create decision.
type Merge struct {
	Input    Op
	Part     ast.PatternPart
	OnCreate []ast.SetClause
	OnMatch  []ast.SetClause
}

// SetItems: applies property assignments, label additions or whole-map updates
// for each input row.
type SetItems struct {
	Input Op
	Items []ast.SetClause
}

// Remove: removes properties or labels for each input row.
type Remove struct {
	Input Op
	Items []ast.RemoveClause
}

// Unwind: expands a list expression into rows by binding each element to Alias.
type Unwind struct {
	Input Op
	Expr  ast.Expr
	Alias string
}

// Argument is a placeholder leaf used inside OuterApply's inner subplan: at exec
// time it is replaced by an operator that emits the current outer row once.
type Argument struct{}

// OuterApply implements the left-outer join used by OPTIONAL MATCH: for each
// row produced by Outer it runs Inner; if Inner emits at least one row, those
// are propagated, otherwise one row is emitted with NewVars bound to null.
type OuterApply struct {
	Outer   Op
	Inner   Op
	NewVars []string
}

// Delete: removes the values produced by the given expressions; if Detach is
// true, incident edges are removed before deleting a node.
type Delete struct {
	Input  Op
	Exprs  []ast.Expr
	Detach bool
}

// CreateIndex: registers a secondary `p` index on (:Label).prop and backfills
// the entries for any existing nodes that already have the property.
type CreateIndex struct {
	Label    string
	Property string
}

func (*AllNodesScan) op()     {}
func (*NodeByLabelScan) op()  {}
func (*NodeByProperty) op()   {}
func (*Expand) op()           {}
func (*Filter) op()           {}
func (*Project) op()          {}
func (*Aggregate) op()        {}
func (*Sort) op()             {}
func (*Skip) op()             {}
func (*Limit) op()            {}
func (*CartesianProduct) op() {}
func (*Create) op()           {}
func (*Merge) op()            {}
func (*SetItems) op()         {}
func (*Delete) op()           {}
func (*CreateIndex) op()      {}
func (*Remove) op()           {}
func (*Unwind) op()           {}
func (*Argument) op()         {}
func (*OuterApply) op()       {}
