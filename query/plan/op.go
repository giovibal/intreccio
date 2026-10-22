// Package plan translates the resolved AST into a physical operator plan
// (iterator/Volcano model) using a rule-based planner (DESIGN §9). In v1 there is
// no cost-based planning: the rules produce operators bound to access methods
// directly.
//
// The planner is decoupled from storage through the [Catalog] interface, so it
// can be used as a standalone library. Callers without a database can pass
// [NoIndexes]. Render a plan as text with [Explain].
package plan

import "github.com/giovibal/intreccio/query/ast"

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

// NodeByPropertyRange: scan via the secondary index over a range of values
// (`p`): Lower/Upper bounds (nil = open) or a STARTS WITH Prefix. It may
// return extra candidates; the planner keeps the predicates as a filter.
type NodeByPropertyRange struct {
	Var            string
	Label          string
	Key            string
	Lower, Upper   ast.Expr
	LowerInclusive bool
	UpperInclusive bool
	Prefix         ast.Expr
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
	// ToUnused / RelUnused mark anonymous pattern elements that nothing reads
	// (no label or property filter, no path variable): the executor binds
	// them by ID only instead of loading their records.
	ToUnused  bool
	RelUnused bool
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

// Apply runs Inner once per row produced by Outer (Inner's Argument leaf
// yields that row) and emits every row Inner produces. It is used when an index
// seek depends on values from the incoming row.
type Apply struct {
	Outer Op
	Inner Op
}

// Union concatenates the results of two or more sub-plans. If All is true
// duplicates are kept (UNION ALL); otherwise rows are de-duplicated on the
// projected columns.
type Union struct {
	Parts   []Op
	Columns []string
	All     bool
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
	Name     string
	Label    string
	Property string
}

// DropIndex removes an index (registry and entries) by name.
type DropIndex struct {
	Name     string
	IfExists bool
}

// ShowIndexes lists the indexes as (name, label, property) rows.
type ShowIndexes struct{}

func (*AllNodesScan) op()        {}
func (*NodeByLabelScan) op()     {}
func (*NodeByProperty) op()      {}
func (*NodeByPropertyRange) op() {}
func (*Expand) op()              {}
func (*Filter) op()              {}
func (*Project) op()             {}
func (*Aggregate) op()           {}
func (*Sort) op()                {}
func (*Skip) op()                {}
func (*Limit) op()               {}
func (*CartesianProduct) op()    {}
func (*Create) op()              {}
func (*Merge) op()               {}
func (*SetItems) op()            {}
func (*Delete) op()              {}
func (*CreateIndex) op()         {}
func (*DropIndex) op()           {}
func (*ShowIndexes) op()         {}
func (*Remove) op()              {}
func (*Unwind) op()              {}
func (*Argument) op()            {}
func (*OuterApply) op()          {}
func (*Apply) op()               {}
func (*Union) op()               {}
