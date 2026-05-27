// Package parser translates Cypher text into an AST (recursive descent + Pratt for
// expressions), limited to the MVP slice (DESIGN §8).
package parser

import (
	"fmt"
	"strings"

	"github.com/giovibal/mycypher/internal/cypher/ast"
)

// Parse parses a Cypher query and returns the AST, or a *ParseError.
func Parse(src string) (*ast.Query, error) {
	toks, err := newLexer(src).tokenize()
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	q, err := p.parseQuery()
	if err != nil {
		return nil, err
	}
	if p.at(tSemi) {
		p.advance()
	}
	if !p.at(tEOF) {
		return nil, p.errf(p.cur().pos, "unexpected token %s", p.describe(p.cur()))
	}
	return q, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) cur() token { return p.toks[p.pos] }

func (p *parser) peek(n int) token {
	i := p.pos + n
	if i >= len(p.toks) {
		return p.toks[len(p.toks)-1] // EOF
	}
	return p.toks[i]
}

func (p *parser) advance() token {
	t := p.toks[p.pos]
	if t.typ != tEOF {
		p.pos++
	}
	return t
}

func (p *parser) at(tt tokenType) bool { return p.cur().typ == tt }

func (p *parser) atKw(kw string) bool {
	t := p.cur()
	return t.typ == tIdent && strings.EqualFold(t.text, kw)
}

// peekKw reports whether the token at distance n is a (case-insensitive) match
// for the given keyword.
func (p *parser) peekKw(n int, kw string) bool {
	t := p.peek(n)
	return t.typ == tIdent && strings.EqualFold(t.text, kw)
}

func (p *parser) errf(pos ast.Pos, format string, args ...any) *ParseError {
	return &ParseError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) describe(t token) string {
	switch t.typ {
	case tIdent:
		return fmt.Sprintf("%q", t.text)
	case tString:
		return "string"
	case tInt, tFloat:
		return "number"
	default:
		return t.typ.String()
	}
}

func (p *parser) expect(tt tokenType) (token, error) {
	if !p.at(tt) {
		return token{}, p.errf(p.cur().pos, "expected %s, found %s", tt, p.describe(p.cur()))
	}
	return p.advance(), nil
}

func (p *parser) expectKw(kw string) error {
	if !p.atKw(kw) {
		return p.errf(p.cur().pos, "expected keyword %s, found %s", kw, p.describe(p.cur()))
	}
	p.advance()
	return nil
}

// identName consumes an identifier and returns its text.
func (p *parser) identName() (string, error) {
	t, err := p.expect(tIdent)
	if err != nil {
		return "", err
	}
	return t.text, nil
}

// --- Clauses ---

func (p *parser) parseQuery() (*ast.Query, error) {
	q := &ast.Query{}
	clauses, err := p.parseClauseList()
	if err != nil {
		return nil, err
	}
	if len(clauses) == 0 {
		return nil, p.errf(p.cur().pos, "empty query")
	}
	q.Clauses = clauses

	for p.atKw("UNION") {
		pos := p.advance().pos
		all := false
		if p.atKw("ALL") {
			all = true
			p.advance()
		}
		rest, err := p.parseClauseList()
		if err != nil {
			return nil, err
		}
		if len(rest) == 0 {
			return nil, p.errf(pos, "UNION arm has no clauses")
		}
		q.Unions = append(q.Unions, ast.QueryUnion{All: all, Clauses: rest, Pos: pos})
	}
	return q, nil
}

// parseClauseList reads clauses until EOF, semicolon or a UNION keyword.
func (p *parser) parseClauseList() ([]ast.Clause, error) {
	var clauses []ast.Clause
	for !p.at(tEOF) && !p.at(tSemi) && !p.atKw("UNION") {
		c, err := p.parseClause()
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, c)
	}
	return clauses, nil
}

func (p *parser) parseClause() (ast.Clause, error) {
	switch {
	case p.atKw("OPTIONAL"):
		pos := p.advance().pos
		if err := p.expectKw("MATCH"); err != nil {
			return nil, err
		}
		return p.matchBody(pos, true)
	case p.atKw("MATCH"):
		pos := p.advance().pos
		return p.matchBody(pos, false)
	case p.atKw("WITH"):
		return p.parseWith()
	case p.atKw("RETURN"):
		return p.parseReturn()
	case p.atKw("CREATE"):
		return p.parseCreate()
	case p.atKw("MERGE"):
		return p.parseMerge()
	case p.atKw("SET"):
		return p.parseSet()
	case p.atKw("REMOVE"):
		return p.parseRemove()
	case p.atKw("UNWIND"):
		return p.parseUnwind()
	case p.atKw("DETACH"):
		pos := p.advance().pos
		if err := p.expectKw("DELETE"); err != nil {
			return nil, err
		}
		return p.deleteBody(pos, true)
	case p.atKw("DELETE"):
		pos := p.advance().pos
		return p.deleteBody(pos, false)
	default:
		return nil, p.errf(p.cur().pos, "unexpected clause: %s", p.describe(p.cur()))
	}
}

func (p *parser) matchBody(pos ast.Pos, optional bool) (*ast.Match, error) {
	parts, err := p.parsePattern()
	if err != nil {
		return nil, err
	}
	m := &ast.Match{Optional: optional, Parts: parts, Pos: pos}
	if p.atKw("WHERE") {
		p.advance()
		if m.Where, err = p.parseExpr(0); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (p *parser) parseReturn() (*ast.Return, error) {
	pos := p.advance().pos // RETURN
	proj, err := p.parseProjection()
	if err != nil {
		return nil, err
	}
	return &ast.Return{
		Distinct: proj.distinct, Star: proj.star, Items: proj.items,
		OrderBy: proj.orderBy, Skip: proj.skip, Limit: proj.limit, Pos: pos,
	}, nil
}

func (p *parser) parseWith() (*ast.With, error) {
	pos := p.advance().pos // WITH
	proj, err := p.parseProjection()
	if err != nil {
		return nil, err
	}
	w := &ast.With{
		Distinct: proj.distinct, Star: proj.star, Items: proj.items,
		OrderBy: proj.orderBy, Skip: proj.skip, Limit: proj.limit, Pos: pos,
	}
	if p.atKw("WHERE") {
		p.advance()
		if w.Where, err = p.parseExpr(0); err != nil {
			return nil, err
		}
	}
	return w, nil
}

type projection struct {
	distinct bool
	star     bool
	items    []ast.ReturnItem
	orderBy  []ast.SortItem
	skip     ast.Expr
	limit    ast.Expr
}

func (p *parser) parseProjection() (projection, error) {
	var proj projection
	if p.atKw("DISTINCT") {
		proj.distinct = true
		p.advance()
	}
	if p.at(tStar) {
		proj.star = true
		p.advance()
		if p.at(tComma) {
			p.advance()
			items, err := p.parseReturnItems()
			if err != nil {
				return proj, err
			}
			proj.items = items
		}
	} else {
		items, err := p.parseReturnItems()
		if err != nil {
			return proj, err
		}
		proj.items = items
	}

	if p.atKw("ORDER") {
		p.advance()
		if err := p.expectKw("BY"); err != nil {
			return proj, err
		}
		ob, err := p.parseSortItems()
		if err != nil {
			return proj, err
		}
		proj.orderBy = ob
	}
	if p.atKw("SKIP") {
		p.advance()
		e, err := p.parseExpr(0)
		if err != nil {
			return proj, err
		}
		proj.skip = e
	}
	if p.atKw("LIMIT") {
		p.advance()
		e, err := p.parseExpr(0)
		if err != nil {
			return proj, err
		}
		proj.limit = e
	}
	return proj, nil
}

func (p *parser) parseReturnItems() ([]ast.ReturnItem, error) {
	var items []ast.ReturnItem
	for {
		e, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		item := ast.ReturnItem{Expr: e}
		if p.atKw("AS") {
			p.advance()
			alias, err := p.identName()
			if err != nil {
				return nil, err
			}
			item.Alias = alias
		}
		items = append(items, item)
		if !p.at(tComma) {
			break
		}
		p.advance()
	}
	return items, nil
}

func (p *parser) parseSortItems() ([]ast.SortItem, error) {
	var items []ast.SortItem
	for {
		e, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		item := ast.SortItem{Expr: e}
		switch {
		case p.atKw("DESC") || p.atKw("DESCENDING"):
			item.Desc = true
			p.advance()
		case p.atKw("ASC") || p.atKw("ASCENDING"):
			p.advance()
		}
		items = append(items, item)
		if !p.at(tComma) {
			break
		}
		p.advance()
	}
	return items, nil
}

func (p *parser) parseCreate() (ast.Clause, error) {
	pos := p.advance().pos // CREATE
	if p.atKw("INDEX") {
		return p.createIndexBody(pos)
	}
	parts, err := p.parsePattern()
	if err != nil {
		return nil, err
	}
	return &ast.Create{Parts: parts, Pos: pos}, nil
}

func (p *parser) createIndexBody(pos ast.Pos) (*ast.CreateIndex, error) {
	p.advance() // INDEX
	// Optional index name before FOR.
	if !p.atKw("FOR") && p.at(tIdent) {
		p.advance()
	}
	if err := p.expectKw("FOR"); err != nil {
		return nil, err
	}
	if _, err := p.expect(tLParen); err != nil {
		return nil, err
	}
	variable, err := p.identName()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tColon); err != nil {
		return nil, err
	}
	label, err := p.identName()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tRParen); err != nil {
		return nil, err
	}
	if err := p.expectKw("ON"); err != nil {
		return nil, err
	}
	if _, err := p.expect(tLParen); err != nil {
		return nil, err
	}
	if _, err := p.identName(); err != nil { // variable reference (ignored)
		return nil, err
	}
	if _, err := p.expect(tDot); err != nil {
		return nil, err
	}
	prop, err := p.identName()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tRParen); err != nil {
		return nil, err
	}
	return &ast.CreateIndex{Variable: variable, Label: label, Property: prop, Pos: pos}, nil
}

func (p *parser) parseMerge() (ast.Clause, error) {
	pos := p.advance().pos // MERGE
	part, err := p.parsePatternPart()
	if err != nil {
		return nil, err
	}
	m := &ast.Merge{Part: part, Pos: pos}
	for p.atKw("ON") {
		p.advance() // ON
		switch {
		case p.atKw("CREATE"):
			p.advance()
			if err := p.expectKw("SET"); err != nil {
				return nil, err
			}
			items, err := p.parseSetItems()
			if err != nil {
				return nil, err
			}
			m.OnCreate = append(m.OnCreate, items...)
		case p.atKw("MATCH"):
			p.advance()
			if err := p.expectKw("SET"); err != nil {
				return nil, err
			}
			items, err := p.parseSetItems()
			if err != nil {
				return nil, err
			}
			m.OnMatch = append(m.OnMatch, items...)
		default:
			return nil, p.errf(p.cur().pos, "expected CREATE or MATCH after ON, found %s", p.describe(p.cur()))
		}
	}
	return m, nil
}

func (p *parser) parseSet() (ast.Clause, error) {
	pos := p.advance().pos // SET
	items, err := p.parseSetItems()
	if err != nil {
		return nil, err
	}
	return &ast.Set{Items: items, Pos: pos}, nil
}

// parseSetItems reads one or more comma-separated SET items.
func (p *parser) parseSetItems() ([]ast.SetClause, error) {
	var items []ast.SetClause
	for {
		item, err := p.parseSetItem()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if !p.at(tComma) {
			break
		}
		p.advance()
	}
	return items, nil
}

// parseSetItem reads one SET item: property assignment, label addition or
// whole-map assignment (replace `=` or merge `+=`).
func (p *parser) parseSetItem() (ast.SetClause, error) {
	if !p.at(tIdent) {
		return nil, p.errf(p.cur().pos, "SET expects an identifier, found %s", p.describe(p.cur()))
	}
	name := p.cur().text
	namePos := p.cur().pos
	next := p.peek(1)
	switch next.typ {
	case tColon:
		p.advance() // identifier
		labels, err := p.parseLabels()
		if err != nil {
			return nil, err
		}
		if len(labels) == 0 {
			return nil, p.errf(namePos, "SET %s: expected one or more labels", name)
		}
		return &ast.SetLabels{Variable: name, Labels: labels, Pos: namePos}, nil
	case tEq:
		p.advance() // identifier
		p.advance() // =
		value, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		return &ast.SetMap{Variable: name, Value: value, Replace: true, Pos: namePos}, nil
	case tPlus:
		if p.peek(2).typ != tEq {
			return nil, p.errf(namePos, "SET %s: expected '+=' after identifier", name)
		}
		p.advance() // identifier
		p.advance() // +
		p.advance() // =
		value, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		return &ast.SetMap{Variable: name, Value: value, Replace: false, Pos: namePos}, nil
	case tDot:
		target, err := p.parseSetTarget()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tEq); err != nil {
			return nil, err
		}
		value, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		return &ast.SetProperty{Target: target, Value: value}, nil
	default:
		return nil, p.errf(namePos, "SET %s: expected '.', ':', '=' or '+='", name)
	}
}

func (p *parser) parseRemove() (ast.Clause, error) {
	pos := p.advance().pos // REMOVE
	var items []ast.RemoveClause
	for {
		item, err := p.parseRemoveItem()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if !p.at(tComma) {
			break
		}
		p.advance()
	}
	return &ast.Remove{Items: items, Pos: pos}, nil
}

func (p *parser) parseRemoveItem() (ast.RemoveClause, error) {
	if !p.at(tIdent) {
		return nil, p.errf(p.cur().pos, "REMOVE expects an identifier, found %s", p.describe(p.cur()))
	}
	name := p.cur().text
	namePos := p.cur().pos
	next := p.peek(1)
	switch next.typ {
	case tColon:
		p.advance() // identifier
		labels, err := p.parseLabels()
		if err != nil {
			return nil, err
		}
		if len(labels) == 0 {
			return nil, p.errf(namePos, "REMOVE %s: expected one or more labels", name)
		}
		return &ast.RemoveLabels{Variable: name, Labels: labels, Pos: namePos}, nil
	case tDot:
		target, err := p.parseSetTarget()
		if err != nil {
			return nil, err
		}
		return &ast.RemoveProperty{Target: target}, nil
	default:
		return nil, p.errf(namePos, "REMOVE %s: expected '.' or ':'", name)
	}
}

func (p *parser) parseUnwind() (ast.Clause, error) {
	pos := p.advance().pos // UNWIND
	expr, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("AS"); err != nil {
		return nil, err
	}
	alias, err := p.identName()
	if err != nil {
		return nil, err
	}
	return &ast.Unwind{Expr: expr, Alias: alias, Pos: pos}, nil
}

func (p *parser) parseSetTarget() (*ast.PropertyAccess, error) {
	prim, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	e, err := p.parsePostfix(prim)
	if err != nil {
		return nil, err
	}
	pa, ok := e.(*ast.PropertyAccess)
	if !ok {
		return nil, p.errf(p.cur().pos, "expected property access (e.g. n.prop) on the left of =")
	}
	return pa, nil
}

func (p *parser) deleteBody(pos ast.Pos, detach bool) (*ast.Delete, error) {
	var exprs []ast.Expr
	for {
		e, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, e)
		if !p.at(tComma) {
			break
		}
		p.advance()
	}
	return &ast.Delete{Detach: detach, Exprs: exprs, Pos: pos}, nil
}

// --- Pattern ---

func (p *parser) parsePattern() ([]ast.PatternPart, error) {
	var parts []ast.PatternPart
	for {
		part, err := p.parsePatternPart()
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
		if !p.at(tComma) {
			break
		}
		p.advance()
	}
	return parts, nil
}

func (p *parser) parsePatternPart() (ast.PatternPart, error) {
	var part ast.PatternPart
	// Optional path variable: var = (...)
	if p.at(tIdent) && p.peek(1).typ == tEq {
		part.Variable = p.cur().text
		p.advance() // var
		p.advance() // =
	}
	start, err := p.parseNodePattern()
	if err != nil {
		return part, err
	}
	part.Start = start
	for p.at(tMinus) || p.at(tLt) {
		rel, err := p.parseRelPattern()
		if err != nil {
			return part, err
		}
		node, err := p.parseNodePattern()
		if err != nil {
			return part, err
		}
		part.Chain = append(part.Chain, ast.PatternChain{Rel: rel, Node: node})
	}
	return part, nil
}

func (p *parser) parseNodePattern() (*ast.NodePattern, error) {
	open, err := p.expect(tLParen)
	if err != nil {
		return nil, err
	}
	n := &ast.NodePattern{Pos: open.pos}
	if p.at(tIdent) {
		n.Variable = p.advance().text
	}
	labels, err := p.parseLabels()
	if err != nil {
		return nil, err
	}
	n.Labels = labels
	if p.at(tLBrace) {
		props, err := p.parsePropertyMap()
		if err != nil {
			return nil, err
		}
		n.Props = props
	}
	if _, err := p.expect(tRParen); err != nil {
		return nil, err
	}
	return n, nil
}

func (p *parser) parseLabels() ([]string, error) {
	var labels []string
	for p.at(tColon) {
		p.advance()
		name, err := p.identName()
		if err != nil {
			return nil, err
		}
		labels = append(labels, name)
	}
	return labels, nil
}

func (p *parser) parseRelPattern() (*ast.RelPattern, error) {
	pos := p.cur().pos
	leftArrow := false
	if p.at(tLt) {
		leftArrow = true
		p.advance()
	}
	if _, err := p.expect(tMinus); err != nil {
		return nil, err
	}

	rel := &ast.RelPattern{Pos: pos, MinHops: -1, MaxHops: -1}
	if p.at(tLBracket) {
		if err := p.parseRelDetail(rel); err != nil {
			return nil, err
		}
	}

	if _, err := p.expect(tMinus); err != nil {
		return nil, err
	}
	rightArrow := false
	if p.at(tGt) {
		rightArrow = true
		p.advance()
	}

	switch {
	case leftArrow && rightArrow:
		return nil, p.errf(pos, "relationship with ambiguous direction (<-...->)")
	case leftArrow:
		rel.Direction = ast.DirIn
	case rightArrow:
		rel.Direction = ast.DirOut
	default:
		rel.Direction = ast.DirBoth
	}
	return rel, nil
}

// parseRelDetail parses the content between the square brackets of a relationship.
func (p *parser) parseRelDetail(rel *ast.RelPattern) error {
	p.advance() // [
	if p.at(tIdent) {
		rel.Variable = p.advance().text
	}
	if p.at(tColon) {
		p.advance()
		name, err := p.identName()
		if err != nil {
			return err
		}
		rel.Types = append(rel.Types, name)
		for p.at(tPipe) {
			p.advance()
			name, err := p.identName()
			if err != nil {
				return err
			}
			rel.Types = append(rel.Types, name)
		}
	}
	if p.at(tStar) {
		p.advance()
		rel.VarLength = true
		if p.at(tInt) {
			rel.MinHops = int(p.advance().val.(int64))
		}
		if p.at(tDotDot) {
			p.advance()
			if p.at(tInt) {
				rel.MaxHops = int(p.advance().val.(int64))
			}
		} else if rel.MinHops != -1 {
			rel.MaxHops = rel.MinHops // *n = exactly n
		}
	}
	if p.at(tLBrace) {
		props, err := p.parsePropertyMap()
		if err != nil {
			return err
		}
		rel.Props = props
	}
	if _, err := p.expect(tRBracket); err != nil {
		return err
	}
	return nil
}

func (p *parser) parsePropertyMap() (map[string]ast.Expr, error) {
	if _, err := p.expect(tLBrace); err != nil {
		return nil, err
	}
	m := map[string]ast.Expr{}
	if !p.at(tRBrace) {
		for {
			key, err := p.identName()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tColon); err != nil {
				return nil, err
			}
			v, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			m[key] = v
			if !p.at(tComma) {
				break
			}
			p.advance()
		}
	}
	if _, err := p.expect(tRBrace); err != nil {
		return nil, err
	}
	return m, nil
}

// --- Expressions (Pratt) ---

// infixBP returns the binding power of the infix operator, 0 if it is not one.
func (p *parser) infixBP() (int, string) {
	switch p.cur().typ {
	case tStar:
		return 6, "*"
	case tSlash:
		return 6, "/"
	case tPercent:
		return 6, "%"
	case tPlus:
		return 5, "+"
	case tMinus:
		return 5, "-"
	case tEq:
		return 4, "="
	case tNeq:
		return 4, "<>"
	case tLt:
		return 4, "<"
	case tLe:
		return 4, "<="
	case tGt:
		return 4, ">"
	case tGe:
		return 4, ">="
	case tIdent:
		if p.atKw("AND") {
			return 2, "AND"
		}
		if p.atKw("OR") {
			return 1, "OR"
		}
	}
	return 0, ""
}

const (
	bpCompare    = 4 // comparison level (=, <>, <, <=, >, >=, IS NULL, STARTS WITH, ...)
	bpNot        = 3 // NOT (prefix): looser than comparisons, tighter than AND
	bpUnaryMinus = 7 // - (prefix): tighter than all infix operators
)

func (p *parser) parseExpr(minBP int) (ast.Expr, error) {
	left, err := p.parsePrefix()
	if err != nil {
		return nil, err
	}
	for {
		// Multi-keyword postfix/infix operators at comparison level (bp 4).
		if minBP <= bpCompare {
			// IS NULL / IS NOT NULL (postfix)
			if p.atKw("IS") && (p.peekKw(1, "NULL") || (p.peekKw(1, "NOT") && p.peekKw(2, "NULL"))) {
				pos := p.advance().pos
				isNot := false
				if p.atKw("NOT") {
					isNot = true
					p.advance()
				}
				if err := p.expectKw("NULL"); err != nil {
					return nil, err
				}
				op := "IS NULL"
				if isNot {
					op = "IS NOT NULL"
				}
				left = &ast.Unary{Op: op, Expr: left, Pos: pos}
				continue
			}
			// Binary string predicates and IN.
			if op, consume := p.matchKeywordInfix(); op != "" {
				pos := p.cur().pos
				for i := 0; i < consume; i++ {
					p.advance()
				}
				right, err := p.parseExpr(bpCompare + 1)
				if err != nil {
					return nil, err
				}
				left = &ast.Binary{Op: op, Left: left, Right: right, Pos: pos}
				continue
			}
		}
		// Standard single-token infix.
		bp, op := p.infixBP()
		if bp == 0 || bp < minBP {
			break
		}
		opTok := p.advance()
		right, err := p.parseExpr(bp + 1)
		if err != nil {
			return nil, err
		}
		left = &ast.Binary{Op: op, Left: left, Right: right, Pos: opTok.pos}
	}
	return left, nil
}

// matchKeywordInfix recognises STARTS WITH / ENDS WITH / CONTAINS / IN and
// returns the operator name plus the number of tokens to advance.
func (p *parser) matchKeywordInfix() (string, int) {
	switch {
	case p.atKw("STARTS") && p.peekKw(1, "WITH"):
		return "STARTS WITH", 2
	case p.atKw("ENDS") && p.peekKw(1, "WITH"):
		return "ENDS WITH", 2
	case p.atKw("CONTAINS"):
		return "CONTAINS", 1
	case p.atKw("IN"):
		return "IN", 1
	}
	return "", 0
}

func (p *parser) parsePrefix() (ast.Expr, error) {
	if p.atKw("NOT") {
		pos := p.advance().pos
		operand, err := p.parseExpr(bpNot)
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Op: "NOT", Expr: operand, Pos: pos}, nil
	}
	if p.at(tMinus) {
		pos := p.advance().pos
		operand, err := p.parseExpr(bpUnaryMinus)
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Op: "-", Expr: operand, Pos: pos}, nil
	}
	prim, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	return p.parsePostfix(prim)
}

// parsePostfix applies property access (.key) and the label predicate (:Label).
func (p *parser) parsePostfix(left ast.Expr) (ast.Expr, error) {
	for {
		switch {
		case p.at(tDot):
			pos := p.advance().pos
			key, err := p.identName()
			if err != nil {
				return nil, err
			}
			left = &ast.PropertyAccess{Target: left, Key: key, Pos: pos}
		case p.at(tColon):
			pos := p.cur().pos
			labels, err := p.parseLabels()
			if err != nil {
				return nil, err
			}
			return &ast.LabelsPredicate{Expr: left, Labels: labels, Pos: pos}, nil
		default:
			return left, nil
		}
	}
}

func (p *parser) parsePrimary() (ast.Expr, error) {
	t := p.cur()
	switch t.typ {
	case tInt:
		p.advance()
		return &ast.Literal{Value: t.val, Pos: t.pos}, nil
	case tFloat:
		p.advance()
		return &ast.Literal{Value: t.val, Pos: t.pos}, nil
	case tString:
		p.advance()
		return &ast.Literal{Value: t.val, Pos: t.pos}, nil
	case tDollar:
		p.advance()
		name, err := p.paramName()
		if err != nil {
			return nil, err
		}
		return &ast.Param{Name: name, Pos: t.pos}, nil
	case tLParen:
		p.advance()
		e, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tRParen); err != nil {
			return nil, err
		}
		return e, nil
	case tLBracket:
		return p.parseListLiteral()
	case tLBrace:
		return p.parseMapLiteralExpr()
	case tIdent:
		switch {
		case strings.EqualFold(t.text, "true"):
			p.advance()
			return &ast.Literal{Value: true, Pos: t.pos}, nil
		case strings.EqualFold(t.text, "false"):
			p.advance()
			return &ast.Literal{Value: false, Pos: t.pos}, nil
		case strings.EqualFold(t.text, "null"):
			p.advance()
			return &ast.Literal{Value: nil, Pos: t.pos}, nil
		case strings.EqualFold(t.text, "case"):
			return p.parseCase()
		}
		if p.peek(1).typ == tLParen {
			return p.parseFunctionCall()
		}
		p.advance()
		return &ast.Variable{Name: t.text, Pos: t.pos}, nil
	default:
		return nil, p.errf(t.pos, "expression expected, found %s", p.describe(t))
	}
}

func (p *parser) paramName() (string, error) {
	t := p.cur()
	switch t.typ {
	case tIdent:
		p.advance()
		return t.text, nil
	case tInt:
		p.advance()
		return t.text, nil
	default:
		return "", p.errf(t.pos, "expected parameter name after $")
	}
}

func (p *parser) parseListLiteral() (ast.Expr, error) {
	pos := p.advance().pos // [
	list := &ast.ListLiteral{Pos: pos}
	if !p.at(tRBracket) {
		for {
			e, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			list.Elements = append(list.Elements, e)
			if !p.at(tComma) {
				break
			}
			p.advance()
		}
	}
	if _, err := p.expect(tRBracket); err != nil {
		return nil, err
	}
	return list, nil
}

func (p *parser) parseMapLiteralExpr() (ast.Expr, error) {
	pos := p.cur().pos
	m, err := p.parsePropertyMap()
	if err != nil {
		return nil, err
	}
	return &ast.MapLiteral{Entries: m, Pos: pos}, nil
}

// parseCase parses both the simple form (CASE x WHEN v THEN ...) and the
// searched form (CASE WHEN cond THEN ...).
func (p *parser) parseCase() (ast.Expr, error) {
	pos := p.advance().pos // CASE
	c := &ast.Case{Pos: pos}
	if !p.atKw("WHEN") {
		operand, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		c.Operand = operand
	}
	for p.atKw("WHEN") {
		p.advance() // WHEN
		cond, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("THEN"); err != nil {
			return nil, err
		}
		result, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, ast.CaseAlternative{Cond: cond, Result: result})
	}
	if len(c.Whens) == 0 {
		return nil, p.errf(pos, "CASE expression requires at least one WHEN clause")
	}
	if p.atKw("ELSE") {
		p.advance()
		elseExpr, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		c.Else = elseExpr
	}
	if err := p.expectKw("END"); err != nil {
		return nil, err
	}
	return c, nil
}

func (p *parser) parseFunctionCall() (ast.Expr, error) {
	nameTok := p.advance() // ident
	if _, err := p.expect(tLParen); err != nil {
		return nil, err
	}
	fn := &ast.FunctionCall{Name: nameTok.text, Pos: nameTok.pos}
	if p.atKw("DISTINCT") {
		fn.Distinct = true
		p.advance()
	}
	if p.at(tStar) {
		fn.Star = true
		p.advance()
	} else if !p.at(tRParen) {
		for {
			arg, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			fn.Args = append(fn.Args, arg)
			if !p.at(tComma) {
				break
			}
			p.advance()
		}
	}
	if _, err := p.expect(tRParen); err != nil {
		return nil, err
	}
	return fn, nil
}
