// Package ast defines the Cypher AST types for the MVP slice (DESIGN §8).
// The AST is purely syntactic: labels, types and property keys stay as strings;
// binding to internal IDs happens during semantic analysis (Phase 4).
package ast

// Pos is a position in the source, used for error messages.
type Pos struct {
	Offset int // byte offset from the start
	Line   int // 1-based
	Col    int // 1-based, in runes
}

// Query is a regular query: a sequence of clauses optionally followed by one
// or more UNION arms.
type Query struct {
	Clauses []Clause
	Unions  []QueryUnion
}

// QueryUnion is one UNION arm of a Query. All=true corresponds to UNION ALL
// (no deduplication); All=false corresponds to UNION (DISTINCT).
type QueryUnion struct {
	All     bool
	Clauses []Clause
	Pos     Pos
}

// Clause is a top-level clause.
type Clause interface{ clause() }

// Match is MATCH / OPTIONAL MATCH with an optional WHERE.
type Match struct {
	Optional bool
	Parts    []PatternPart
	Where    Expr // nil if absent
	Pos      Pos
}

// With is WITH (intermediate projection with scope reset).
type With struct {
	Distinct bool
	Star     bool
	Items    []ReturnItem
	OrderBy  []SortItem
	Skip     Expr
	Limit    Expr
	Where    Expr // WITH ... WHERE
	Pos      Pos
}

// Return is RETURN.
type Return struct {
	Distinct bool
	Star     bool
	Items    []ReturnItem
	OrderBy  []SortItem
	Skip     Expr
	Limit    Expr
	Pos      Pos
}

// Create is CREATE of one or more patterns.
type Create struct {
	Parts []PatternPart
	Pos   Pos
}

// Merge is MERGE of a single pattern with optional ON CREATE / ON MATCH SET
// blocks applied to the matched-or-created bindings.
type Merge struct {
	Part     PatternPart
	OnCreate []SetClause
	OnMatch  []SetClause
	Pos      Pos
}

// Set is SET of one or more items (property assignment, label set, or map set).
type Set struct {
	Items []SetClause
	Pos   Pos
}

// Remove removes properties or labels.
type Remove struct {
	Items []RemoveClause
	Pos   Pos
}

// Unwind expands a list expression into individual rows bound to Alias.
type Unwind struct {
	Expr  Expr
	Alias string
	Pos   Pos
}

// Delete is DELETE / DETACH DELETE.
type Delete struct {
	Detach bool
	Exprs  []Expr
	Pos    Pos
}

// CreateIndex is CREATE INDEX FOR (v:Label) ON (v.prop).
type CreateIndex struct {
	Variable string
	Label    string
	Property string
	Pos      Pos
}

func (*Match) clause()       {}
func (*With) clause()        {}
func (*Return) clause()      {}
func (*Create) clause()      {}
func (*Merge) clause()       {}
func (*Set) clause()         {}
func (*Remove) clause()      {}
func (*Unwind) clause()      {}
func (*Delete) clause()      {}
func (*CreateIndex) clause() {}

// ReturnItem is a projection element (RETURN/WITH).
type ReturnItem struct {
	Expr  Expr
	Alias string // "" if absent
}

// SortItem is an ORDER BY criterion.
type SortItem struct {
	Expr Expr
	Desc bool
}

// SetClause is one assignment inside a SET clause: property update, label
// addition, or whole-property replacement/merge via a map expression.
type SetClause interface{ setClause() }

// SetProperty is `SET target = value`, where target is `var.prop`.
type SetProperty struct {
	Target *PropertyAccess
	Value  Expr
}

// SetLabels is `SET var:Label1:Label2` — adds labels to a node.
type SetLabels struct {
	Variable string
	Labels   []string
	Pos      Pos
}

// SetMap is `SET var = expr` (Replace=true) or `SET var += expr` (Replace=false)
// where expr evaluates to a map of property values.
type SetMap struct {
	Variable string
	Value    Expr
	Replace  bool
	Pos      Pos
}

func (*SetProperty) setClause() {}
func (*SetLabels) setClause()   {}
func (*SetMap) setClause()      {}

// RemoveClause is one item inside a REMOVE clause: a property removal or a
// label removal.
type RemoveClause interface{ removeClause() }

// RemoveProperty is `REMOVE target` where target is `var.prop`.
type RemoveProperty struct {
	Target *PropertyAccess
}

// RemoveLabels is `REMOVE var:Label1:Label2`.
type RemoveLabels struct {
	Variable string
	Labels   []string
	Pos      Pos
}

func (*RemoveProperty) removeClause() {}
func (*RemoveLabels) removeClause()   {}

// --- Pattern ---

// Direction is the direction of a relationship in the pattern.
type Direction int

const (
	DirOut  Direction = iota // -[]->
	DirIn                    // <-[]-
	DirBoth                  // -[]-
)

// PatternPart is a pattern segment: the start node plus a chain of (rel, node).
// Variable, if non-empty, is the path variable (p = (...)).
type PatternPart struct {
	Variable string
	Start    *NodePattern
	Chain    []PatternChain
}

// PatternChain is a step (relationship → node) in the chain.
type PatternChain struct {
	Rel  *RelPattern
	Node *NodePattern
}

// NodePattern is a node in the pattern: (var:Label {props}).
type NodePattern struct {
	Variable string
	Labels   []string
	Props    map[string]Expr
	Pos      Pos
}

// RelPattern is a relationship in the pattern: -[var:TYPE {props}]->, with an
// optional variable length *min..max.
type RelPattern struct {
	Variable  string
	Types     []string
	Props     map[string]Expr
	Direction Direction
	VarLength bool
	MinHops   int // valid if VarLength; -1 = unspecified
	MaxHops   int // valid if VarLength; -1 = unbounded
	Pos       Pos
}

// --- Expressions ---

// Expr is an expression.
type Expr interface{ expr() }

// Literal is null/bool/int64/float64/string.
type Literal struct {
	Value any // nil, bool, int64, float64, string
	Pos   Pos
}

// Param is a $name parameter.
type Param struct {
	Name string
	Pos  Pos
}

// Variable is a variable reference.
type Variable struct {
	Name string
	Pos  Pos
}

// PropertyAccess is a property access: target.key.
type PropertyAccess struct {
	Target Expr
	Key    string
	Pos    Pos
}

// Unary is a unary operation: NOT expr, -expr.
type Unary struct {
	Op   string // "NOT", "-"
	Expr Expr
	Pos  Pos
}

// Binary is a binary operation (arithmetic, comparison, AND/OR).
type Binary struct {
	Op    string // + - * / % = <> < <= > >= AND OR
	Left  Expr
	Right Expr
	Pos   Pos
}

// FunctionCall is a function call: name(args) / count(*) / count(DISTINCT x).
type FunctionCall struct {
	Name     string
	Distinct bool
	Star     bool
	Args     []Expr
	Pos      Pos
}

// LabelsPredicate is the label predicate in WHERE: expr:Label1:Label2.
type LabelsPredicate struct {
	Expr   Expr
	Labels []string
	Pos    Pos
}

// ListLiteral is a [expr, expr, ...] expression.
type ListLiteral struct {
	Elements []Expr
	Pos      Pos
}

// MapLiteral is a {key: expr, ...} expression.
type MapLiteral struct {
	Entries map[string]Expr
	Pos     Pos
}

// Case is the CASE expression. When Operand is non-nil the form is "simple"
// (CASE x WHEN v THEN r ...); when nil the form is "searched"
// (CASE WHEN cond THEN r ...). Else is optional.
type Case struct {
	Operand Expr
	Whens   []CaseAlternative
	Else    Expr
	Pos     Pos
}

// CaseAlternative is one WHEN ... THEN ... branch of a CASE expression.
type CaseAlternative struct {
	Cond   Expr
	Result Expr
}

func (*Literal) expr()         {}
func (*Param) expr()           {}
func (*Variable) expr()        {}
func (*PropertyAccess) expr()  {}
func (*Unary) expr()           {}
func (*Binary) expr()          {}
func (*FunctionCall) expr()    {}
func (*LabelsPredicate) expr() {}
func (*ListLiteral) expr()     {}
func (*MapLiteral) expr()      {}
func (*Case) expr()            {}
