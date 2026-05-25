package parser

import "github.com/giovibal/mycypher/internal/cypher/ast"

type tokenType int

const (
	tEOF tokenType = iota
	tIdent
	tInt
	tFloat
	tString

	tLParen
	tRParen
	tLBracket
	tRBracket
	tLBrace
	tRBrace
	tColon
	tComma
	tDot
	tDotDot // ..
	tPipe
	tDollar
	tSemi

	tPlus
	tMinus
	tStar
	tSlash
	tPercent

	tEq  // =
	tNeq // <>
	tLt  // <
	tLe  // <=
	tGt  // >
	tGe  // >=
)

type token struct {
	typ  tokenType
	text string // raw text (identifiers, keywords)
	val  any    // decoded value for tInt/tFloat/tString
	pos  ast.Pos
}

func (t tokenType) String() string {
	switch t {
	case tEOF:
		return "EOF"
	case tIdent:
		return "identifier"
	case tInt:
		return "integer"
	case tFloat:
		return "float"
	case tString:
		return "string"
	case tLParen:
		return "("
	case tRParen:
		return ")"
	case tLBracket:
		return "["
	case tRBracket:
		return "]"
	case tLBrace:
		return "{"
	case tRBrace:
		return "}"
	case tColon:
		return ":"
	case tComma:
		return ","
	case tDot:
		return "."
	case tDotDot:
		return ".."
	case tPipe:
		return "|"
	case tDollar:
		return "$"
	case tSemi:
		return ";"
	case tPlus:
		return "+"
	case tMinus:
		return "-"
	case tStar:
		return "*"
	case tSlash:
		return "/"
	case tPercent:
		return "%"
	case tEq:
		return "="
	case tNeq:
		return "<>"
	case tLt:
		return "<"
	case tLe:
		return "<="
	case tGt:
		return ">"
	case tGe:
		return ">="
	default:
		return "unknown token"
	}
}
