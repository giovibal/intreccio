// Package plan traduce l'AST risolto in un piano fisico di operatori (modello
// iterator/Volcano) con un planner a regole (DESIGN §9). In v1 non c'è cost-based:
// le regole producono direttamente operatori legati agli access method.
package plan

import "github.com/giovibal/mycypher/internal/cypher/ast"

// Op è un operatore del piano.
type Op interface{ op() }

// --- Access method (foglie) ---

// AllNodesScan: scan di tutti i nodi (fallback, `n`).
type AllNodesScan struct{ Var string }

// NodeByLabelScan: scan dei nodi con una label (`l`).
type NodeByLabelScan struct {
	Var   string
	Label string
}

// NodeByProperty: scan via indice secondario su equality (`p`).
type NodeByProperty struct {
	Var   string
	Label string
	Key   string
	Value ast.Expr
}

// --- Operatori interni ---

// Expand: a partire dai nodi From, segue gli archi e produce i nodi To.
type Expand struct {
	Input     Op
	From      string
	Rel       string // variabile della relazione (può essere sintetica)
	To        string
	Types     []string
	Dir       ast.Direction
	VarLength bool
	MinHops   int
	MaxHops   int
	ToBound   bool // true se To è già legato: l'expand verifica l'uguaglianza
}

// Filter: applica un predicato allo stream.
type Filter struct {
	Input Op
	Pred  ast.Expr
}

// Project: proiezione (RETURN/WITH senza aggregazioni).
type Project struct {
	Input    Op
	Items    []ProjItem
	Distinct bool
}

// ProjItem è un item di proiezione con il nome di colonna risolto.
type ProjItem struct {
	Expr   ast.Expr
	Column string
}

// Aggregate: raggruppamento implicito (chiavi = item non aggregati). L'esecuzione
// completa è Fase 8; qui il planner lo produce per renderlo ispezionabile.
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

// SortKey è un criterio di ordinamento.
type SortKey struct {
	Expr ast.Expr
	Desc bool
}

// Skip: salta le prime N righe.
type Skip struct {
	Input Op
	Count ast.Expr
}

// Limit: limita a N righe.
type Limit struct {
	Input Op
	Count ast.Expr
}

// CartesianProduct: prodotto cartesiano tra due sottopiani non connessi.
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
