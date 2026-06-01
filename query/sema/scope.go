package sema

import "github.com/giovibal/intreccio/query/ast"

// scope is the ordered set of variables visible at a point in the query.
type scope struct {
	vars  map[string]ast.Pos
	order []string // insertion order, for * expansion
}

func newScope() *scope {
	return &scope{vars: map[string]ast.Pos{}}
}

func (s *scope) define(name string, pos ast.Pos) {
	if _, ok := s.vars[name]; ok {
		return // first definition wins (redefinitions in patterns are references)
	}
	s.vars[name] = pos
	s.order = append(s.order, name)
}

func (s *scope) has(name string) bool {
	_, ok := s.vars[name]
	return ok
}

func (s *scope) empty() bool { return len(s.order) == 0 }

// merge copies the variables of other, preserving their order.
func (s *scope) merge(other *scope) {
	for _, name := range other.order {
		s.define(name, other.vars[name])
	}
}

// replaceWith replaces the scope content with that of other (reset on WITH).
func (s *scope) replaceWith(other *scope) {
	s.vars = other.vars
	s.order = other.order
}
