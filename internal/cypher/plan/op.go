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
