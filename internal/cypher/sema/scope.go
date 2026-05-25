package sema

import "github.com/giovibal/mycypher/internal/cypher/ast"

// scope è l'insieme ordinato delle variabili visibili in un punto della query.
type scope struct {
	vars  map[string]ast.Pos
	order []string // ordine di inserimento, per l'espansione di *
}

func newScope() *scope {
	return &scope{vars: map[string]ast.Pos{}}
}

func (s *scope) define(name string, pos ast.Pos) {
	if _, ok := s.vars[name]; ok {
		return // prima definizione vince (le ridefinizioni nei pattern sono riferimenti)
	}
	s.vars[name] = pos
	s.order = append(s.order, name)
}

func (s *scope) has(name string) bool {
	_, ok := s.vars[name]
	return ok
}

func (s *scope) empty() bool { return len(s.order) == 0 }

// merge copia le variabili di other mantenendone l'ordine.
func (s *scope) merge(other *scope) {
	for _, name := range other.order {
		s.define(name, other.vars[name])
	}
}

// replaceWith sostituisce il contenuto dello scope con quello di other (reset su WITH).
func (s *scope) replaceWith(other *scope) {
	s.vars = other.vars
	s.order = other.order
}
