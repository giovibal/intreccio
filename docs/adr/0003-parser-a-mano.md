# ADR 0003 — Parser Cypher scritto a mano

Stato: accettato — 2026-05-25

## Contesto
Fase 3 (`PLAN.md`): dal testo Cypher all'AST per lo slice MVP. La scelta era tra
generare il parser da grammatica ufficiale openCypher con ANTLR (runtime puro Go
`antlr4-go/antlr/v4` + tool di codegen in Java) oppure scriverlo a mano
(recursive descent + Pratt per le espressioni).

## Decisione
**Parser scritto a mano**, limitato allo slice MVP.

Motivazioni:
- **Zero dipendenze e nessun code-gen**: niente runtime ANTLR né step Java nel
  workflow di build/CI. Resta in linea con "puro Go, single binary, toolchain
  minima" e "preferire stdlib" (CLAUDE.md).
- **AST pulito**: i tipi in `internal/cypher/ast` sono modellati sul nostro
  dominio, senza passare da un albero di parse generico + visitor.
- **Errori con posizione** sotto pieno controllo (`ParseError{Pos}`), come
  richiesto dai test.
- Lo slice MVP è ristretto: il costo del recursive descent è contenuto.

Compromesso accettato: dover scrivere a mano lexer e regole; estensioni future
della grammatica vanno aggiunte manualmente.

## Implementazione
- `parser/lexer.go`: lexer con posizione (riga/colonna in rune), numeri int/float,
  stringhe con escape, identificatori (anche backtick), `$param`, commenti
  `//` e `/* */`.
- `parser/parser.go`: recursive descent per le clausole/pattern, **Pratt** per le
  espressioni. Precedenze (dal più stretto): postfix `.`/`:` → unario `-` →
  `* / %` → `+ -` → confronti → `NOT` → `AND` → `OR`.
- Copertura MVP: `MATCH`/`OPTIONAL MATCH` + `WHERE`, `WITH`, `RETURN`
  (`DISTINCT`, `*`, `AS`, `ORDER BY`, `SKIP`, `LIMIT`), `CREATE`, `MERGE`, `SET`,
  `DELETE`/`DETACH DELETE`, `CREATE INDEX FOR (v:L) ON (v.p)`; pattern con
  direzioni, label/tipi multipli, mappe di proprietà e lunghezza variabile
  `*min..max`; funzioni con `DISTINCT`/`*` (aggregazioni).

## Rimandato (fuori MVP, post-9)
`MERGE ... ON CREATE/ON MATCH SET`, `SET` di label o di mappe, list/map literal
come espressioni, `IN`, `IS NULL`, `CALL`/`EXISTS`. La semantica (binding, scope)
è la Fase 4.
