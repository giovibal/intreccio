package parser

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/giovibal/intreccio/internal/cypher/ast"
)

// ParseError is a parsing error with a position in the source.
type ParseError struct {
	Pos ast.Pos
	Msg string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d:%d: %s", e.Pos.Line, e.Pos.Col, e.Msg)
}

type lexer struct {
	src  string
	pos  int // byte offset
	line int
	col  int
}

func newLexer(src string) *lexer {
	return &lexer{src: src, pos: 0, line: 1, col: 1}
}

func (l *lexer) here() ast.Pos {
	return ast.Pos{Offset: l.pos, Line: l.line, Col: l.col}
}

func (l *lexer) errf(pos ast.Pos, format string, args ...any) *ParseError {
	return &ParseError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

// peekRune returns the current rune without consuming it.
func (l *lexer) peekRune() (rune, int) {
	if l.pos >= len(l.src) {
		return 0, 0
	}
	return utf8.DecodeRuneInString(l.src[l.pos:])
}

// advance consumes one rune, updating line/column.
func (l *lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(l.src[l.pos:])
	l.pos += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

// tokenize produces all tokens up to and including EOF.
func (l *lexer) tokenize() ([]token, error) {
	var toks []token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.typ == tEOF {
			return toks, nil
		}
	}
}

func (l *lexer) next() (token, error) {
	if err := l.skipTrivia(); err != nil {
		return token{}, err
	}
	start := l.here()
	if l.pos >= len(l.src) {
		return token{typ: tEOF, pos: start}, nil
	}

	r, _ := l.peekRune()
	switch {
	case r == '`':
		return l.lexBacktickIdent(start)
	case isIdentStart(r):
		return l.lexIdent(start), nil
	case unicode.IsDigit(r):
		return l.lexNumber(start)
	case r == '.':
		return l.lexDotOrNumber(start)
	case r == '\'' || r == '"':
		return l.lexString(start)
	default:
		return l.lexOperator(start)
	}
}

func (l *lexer) skipTrivia() error {
	for l.pos < len(l.src) {
		r, _ := l.peekRune()
		switch {
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			l.advance()
		case r == '/' && l.peekAhead(1) == '/':
			for l.pos < len(l.src) {
				if c, _ := l.peekRune(); c == '\n' {
					break
				}
				l.advance()
			}
		case r == '/' && l.peekAhead(1) == '*':
			start := l.here()
			l.advance() // /
			l.advance() // *
			closed := false
			for l.pos < len(l.src) {
				if c, _ := l.peekRune(); c == '*' && l.peekAhead(1) == '/' {
					l.advance()
					l.advance()
					closed = true
					break
				}
				l.advance()
			}
			if !closed {
				return l.errf(start, "unterminated block comment")
			}
		default:
			return nil
		}
	}
	return nil
}

// peekAhead returns the byte at distance n (ASCII), or 0.
func (l *lexer) peekAhead(n int) byte {
	if l.pos+n < len(l.src) {
		return l.src[l.pos+n]
	}
	return 0
}

func (l *lexer) lexIdent(start ast.Pos) token {
	begin := l.pos
	for l.pos < len(l.src) {
		r, _ := l.peekRune()
		if !isIdentPart(r) {
			break
		}
		l.advance()
	}
	return token{typ: tIdent, text: l.src[begin:l.pos], pos: start}
}

func (l *lexer) lexBacktickIdent(start ast.Pos) (token, error) {
	l.advance() // `
	var sb strings.Builder
	for {
		if l.pos >= len(l.src) {
			return token{}, l.errf(start, "unterminated backtick identifier")
		}
		r := l.advance()
		if r == '`' {
			break
		}
		sb.WriteRune(r)
	}
	return token{typ: tIdent, text: sb.String(), pos: start}, nil
}

func (l *lexer) lexNumber(start ast.Pos) (token, error) {
	begin := l.pos
	isFloat := false
	for l.pos < len(l.src) {
		r, _ := l.peekRune()
		switch {
		case unicode.IsDigit(r):
			l.advance()
		case r == '.' && l.peekAhead(1) != '.': // avoid swallowing ".." (range)
			isFloat = true
			l.advance()
		case r == 'e' || r == 'E':
			isFloat = true
			l.advance()
			if c, _ := l.peekRune(); c == '+' || c == '-' {
				l.advance()
			}
		default:
			goto done
		}
	}
done:
	text := l.src[begin:l.pos]
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return token{}, l.errf(start, "invalid float %q", text)
		}
		return token{typ: tFloat, text: text, val: f, pos: start}, nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return token{}, l.errf(start, "invalid integer %q", text)
	}
	return token{typ: tInt, text: text, val: n, pos: start}, nil
}

func (l *lexer) lexDotOrNumber(start ast.Pos) (token, error) {
	if l.peekAhead(1) == '.' {
		l.advance()
		l.advance()
		return token{typ: tDotDot, pos: start}, nil
	}
	if c := l.peekAhead(1); c >= '0' && c <= '9' {
		return l.lexNumber(start) // .5
	}
	l.advance()
	return token{typ: tDot, pos: start}, nil
}

func (l *lexer) lexString(start ast.Pos) (token, error) {
	quote := l.advance()
	var sb strings.Builder
	for {
		if l.pos >= len(l.src) {
			return token{}, l.errf(start, "unterminated string")
		}
		r := l.advance()
		if r == quote {
			break
		}
		if r == '\\' {
			if l.pos >= len(l.src) {
				return token{}, l.errf(start, "unterminated string")
			}
			esc := l.advance()
			switch esc {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '\\':
				sb.WriteByte('\\')
			case '\'':
				sb.WriteByte('\'')
			case '"':
				sb.WriteByte('"')
			case '0':
				sb.WriteByte(0)
			default:
				return token{}, l.errf(start, "invalid escape sequence \\%c", esc)
			}
			continue
		}
		sb.WriteRune(r)
	}
	return token{typ: tString, val: sb.String(), pos: start}, nil
}

func (l *lexer) lexOperator(start ast.Pos) (token, error) {
	r := l.advance()
	switch r {
	case '(':
		return token{typ: tLParen, pos: start}, nil
	case ')':
		return token{typ: tRParen, pos: start}, nil
	case '[':
		return token{typ: tLBracket, pos: start}, nil
	case ']':
		return token{typ: tRBracket, pos: start}, nil
	case '{':
		return token{typ: tLBrace, pos: start}, nil
	case '}':
		return token{typ: tRBrace, pos: start}, nil
	case ':':
		return token{typ: tColon, pos: start}, nil
	case ',':
		return token{typ: tComma, pos: start}, nil
	case '|':
		return token{typ: tPipe, pos: start}, nil
	case '$':
		return token{typ: tDollar, pos: start}, nil
	case ';':
		return token{typ: tSemi, pos: start}, nil
	case '+':
		return token{typ: tPlus, pos: start}, nil
	case '-':
		return token{typ: tMinus, pos: start}, nil
	case '*':
		return token{typ: tStar, pos: start}, nil
	case '/':
		return token{typ: tSlash, pos: start}, nil
	case '%':
		return token{typ: tPercent, pos: start}, nil
	case '=':
		return token{typ: tEq, pos: start}, nil
	case '<':
		if c, _ := l.peekRune(); c == '>' {
			l.advance()
			return token{typ: tNeq, pos: start}, nil
		} else if c == '=' {
			l.advance()
			return token{typ: tLe, pos: start}, nil
		}
		return token{typ: tLt, pos: start}, nil
	case '>':
		if c, _ := l.peekRune(); c == '=' {
			l.advance()
			return token{typ: tGe, pos: start}, nil
		}
		return token{typ: tGt, pos: start}, nil
	default:
		return token{}, l.errf(start, "unexpected character %q", r)
	}
}

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentPart(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
