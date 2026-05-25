// Package ast definisce i tipi dell'AST Cypher per lo slice MVP (DESIGN §8).
// L'AST è puramente sintattico: label, tipi e chiavi-proprietà restano stringhe;
// il binding agli ID interni avviene nell'analisi semantica (Fase 4).
package ast

// Pos è una posizione nel sorgente, usata per i messaggi d'errore.
type Pos struct {
	Offset int // offset in byte dall'inizio
	Line   int // 1-based
	Col    int // 1-based, in rune
}

// Query è una singola query: una sequenza di clausole.
type Query struct {
	Clauses []Clause
}

// Clause è una clausola di alto livello.
type Clause interface{ clause() }

// Match è MATCH / OPTIONAL MATCH con eventuale WHERE.
type Match struct {
	Optional bool
	Parts    []PatternPart
	Where    Expr // nil se assente
	Pos      Pos
}

// With è WITH (proiezione intermedia con reset dello scope).
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

// Return è RETURN.
type Return struct {
	Distinct bool
	Star     bool
	Items    []ReturnItem
	OrderBy  []SortItem
	Skip     Expr
	Limit    Expr
	Pos      Pos
}

// Create è CREATE di uno o più pattern.
type Create struct {
	Parts []PatternPart
	Pos   Pos
}

// Merge è MERGE di un singolo pattern.
type Merge struct {
	Part PatternPart
	Pos  Pos
}

// Set è SET di una o più proprietà.
type Set struct {
	Items []SetItem
	Pos   Pos
}

// Delete è DELETE / DETACH DELETE.
type Delete struct {
	Detach bool
	Exprs  []Expr
	Pos    Pos
}

// CreateIndex è CREATE INDEX FOR (v:Label) ON (v.prop).
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
func (*Delete) clause()      {}
func (*CreateIndex) clause() {}

// ReturnItem è un elemento di proiezione (RETURN/WITH).
type ReturnItem struct {
	Expr  Expr
	Alias string // "" se assente
}

// SortItem è un criterio di ORDER BY.
type SortItem struct {
	Expr Expr
	Desc bool
}

// SetItem è un'assegnazione di SET: Target = Value (con Target accesso a proprietà).
type SetItem struct {
	Target *PropertyAccess
	Value  Expr
}

// --- Pattern ---

// Direction è la direzione di una relazione nel pattern.
type Direction int

const (
	DirOut  Direction = iota // -[]->
	DirIn                    // <-[]-
	DirBoth                  // -[]-
)

// PatternPart è un pezzo di pattern: nodo iniziale più una catena di (rel, nodo).
// Variable, se non vuoto, è la variabile di path (p = (...)).
type PatternPart struct {
	Variable string
	Start    *NodePattern
	Chain    []PatternChain
}

// PatternChain è un passo (relazione → nodo) nella catena.
type PatternChain struct {
	Rel  *RelPattern
	Node *NodePattern
}

// NodePattern è un nodo del pattern: (var:Label {props}).
type NodePattern struct {
	Variable string
	Labels   []string
	Props    map[string]Expr
	Pos      Pos
}

// RelPattern è una relazione del pattern: -[var:TYPE {props}]->, con eventuale
// lunghezza variabile *min..max.
type RelPattern struct {
	Variable  string
	Types     []string
	Props     map[string]Expr
	Direction Direction
	VarLength bool
	MinHops   int // valido se VarLength; -1 = non specificato
	MaxHops   int // valido se VarLength; -1 = illimitato
	Pos       Pos
}

// --- Espressioni ---

// Expr è un'espressione.
type Expr interface{ expr() }

// Literal è null/bool/int64/float64/string.
type Literal struct {
	Value any // nil, bool, int64, float64, string
	Pos   Pos
}

// Param è un parametro $name.
type Param struct {
	Name string
	Pos  Pos
}

// Variable è un riferimento a variabile.
type Variable struct {
	Name string
	Pos  Pos
}

// PropertyAccess è accesso a proprietà: target.key.
type PropertyAccess struct {
	Target Expr
	Key    string
	Pos    Pos
}

// Unary è un'operazione unaria: NOT expr, -expr.
type Unary struct {
	Op   string // "NOT", "-"
	Expr Expr
	Pos  Pos
}

// Binary è un'operazione binaria (aritmetica, confronto, AND/OR).
type Binary struct {
	Op    string // + - * / % = <> < <= > >= AND OR
	Left  Expr
	Right Expr
	Pos   Pos
}

// FunctionCall è una chiamata di funzione: name(args) / count(*) / count(DISTINCT x).
type FunctionCall struct {
	Name     string
	Distinct bool
	Star     bool
	Args     []Expr
	Pos      Pos
}

// LabelsPredicate è il predicato di label in WHERE: expr:Label1:Label2.
type LabelsPredicate struct {
	Expr   Expr
	Labels []string
	Pos    Pos
}

func (*Literal) expr()         {}
func (*Param) expr()           {}
func (*Variable) expr()        {}
func (*PropertyAccess) expr()  {}
func (*Unary) expr()           {}
func (*Binary) expr()          {}
func (*FunctionCall) expr()    {}
func (*LabelsPredicate) expr() {}
