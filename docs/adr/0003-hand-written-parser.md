# ADR 0003 — Hand-written Cypher parser

Status: accepted — 2026-05-25

## Context
Phase 3 (`PLAN.md`): from Cypher text to the AST for the MVP slice. The choice was
between generating the parser from the official openCypher grammar with ANTLR
(pure Go runtime `antlr4-go/antlr/v4` + a Java codegen tool) or writing it by hand
(recursive descent + Pratt for expressions).

## Decision
**Hand-written parser**, limited to the MVP slice.

Rationale:
- **Zero dependencies and no code-gen**: no ANTLR runtime and no Java step in the
  build/CI workflow. Stays in line with "pure Go, single binary, minimal
  toolchain" and "prefer the stdlib" (CLAUDE.md).
- **Clean AST**: the types in `internal/cypher/ast` are modeled on our domain,
  without going through a generic parse tree + visitor.
- **Errors with position** under full control (`ParseError{Pos}`), as required by
  the tests.
- The MVP slice is narrow: the cost of recursive descent is contained.

Accepted trade-off: having to hand-write the lexer and rules; future grammar
extensions must be added manually.

## Implementation
- `parser/lexer.go`: lexer with position (line/column in runes), int/float
  numbers, strings with escapes, identifiers (including backtick), `$param`,
  `//` and `/* */` comments.
- `parser/parser.go`: recursive descent for clauses/patterns, **Pratt** for
  expressions. Precedences (from tightest): postfix `.`/`:` → unary `-` →
  `* / %` → `+ -` → comparisons → `NOT` → `AND` → `OR`.
- MVP coverage: `MATCH`/`OPTIONAL MATCH` + `WHERE`, `WITH`, `RETURN`
  (`DISTINCT`, `*`, `AS`, `ORDER BY`, `SKIP`, `LIMIT`), `CREATE`, `MERGE`, `SET`,
  `DELETE`/`DETACH DELETE`, `CREATE INDEX FOR (v:L) ON (v.p)`; patterns with
  directions, multiple labels/types, property maps and variable length
  `*min..max`; functions with `DISTINCT`/`*` (aggregations).

## Deferred (outside MVP, post-9)
`MERGE ... ON CREATE/ON MATCH SET`, `SET` of labels or maps, list/map literals as
expressions, `IN`, `IS NULL`, `CALL`/`EXISTS`. The semantics (binding, scope) are
Phase 4.
