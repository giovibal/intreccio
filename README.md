# mycypher

An **embedded**, **single-binary** graph database written in **pure Go** (no cgo)
that speaks a useful subset of **openCypher 9**. It targets **OLTP /
knowledge-graph** workloads: point lookups and few-hop traversals over
medium-sized graphs. It is *not* an analytical (OLAP) engine.

> Work in progress. Cypher reads and writes (CREATE/MERGE/SET/DELETE/DETACH
> DELETE) run end-to-end through the public API; advanced projection
> (ORDER BY/DISTINCT chaining), aggregations and variable-length traversal land
> in Phase 8. See the roadmap below.

## Highlights

- **Pure Go, no cgo** — deploys as a single self-contained binary.
- **Property graph** — nodes with labels and properties, typed relationships
  with properties.
- **Pluggable storage** behind a minimal `Store` interface; **BadgerDB** is the
  default engine (bbolt is a planned alternative adapter).
- **Order-preserving key encoding** so hot queries become a single ordered range
  scan, and secondary property indexes support range queries.
- **ACID transactions** with always-consistent indexes (every mutation updates
  the base record and all of its index keys in the same transaction).
- **Rule-based query planner** with an inspectable textual `EXPLAIN`.

## Getting started

Requirements: Go 1.26+.

```bash
go build ./...        # build
go test ./...         # run tests
make race             # tests with the race detector
make lint             # golangci-lint (v2)
go run ./cmd/mycypher # interactive REPL on an in-memory database
```

### CLI / REPL

The `mycypher` binary opens either a directory-backed database or an in-memory
one if no path is given, and accepts Cypher statements interactively (terminated
by `;`) or as a one-shot via `-c`:

```bash
# interactive REPL on an in-memory database
mycypher

# one-shot
mycypher -c "CREATE (n:Person {name: 'Bob'}) RETURN n.name AS name"

# persistent database under data/
mycypher data/
```

REPL commands: `:quit` / `:exit` to leave, `:help` for a short help. Statements
can span multiple lines; the terminator is `;` (note: a `;` inside a string
literal is not currently recognized as a statement boundary).

### Embeddable API

The library supports both reads and writes through `Query`:

```go
db, err := mycypher.Open("data/") // open/create the database
if err != nil {
    log.Fatal(err)
}
defer db.Close()

res, err := db.Query(ctx, `
    MATCH (p:Person)-[:KNOWS]->(f)
    WHERE p.email = $email
    RETURN f.name AS name
    ORDER BY name LIMIT 10
`, map[string]any{"email": "a@b.com"})
// res.Columns is []string; res.Rows is [][]any in column order.
```

## Architecture

The query pipeline runs entirely in-process, top to bottom:

```
Cypher text
  → Parser            → AST
  → Semantic analysis   (scoping, validation, output columns)
  → Rule-based planner  (anchor selection, filter push-down)
  → Physical plan       (Volcano/iterator operators)
  → Executor            (Next())
  → Graph storage API   (transactional CRUD of nodes/edges/indexes)
  → Key encoding        (prefixed tables over an ordered KV store)
  → Storage engine      (BadgerDB; pure Go; ACID)
```

Storage is keyed so that adjacency and lookups are ordered range scans. Each edge
is written twice — outgoing (`o`) and incoming (`i`) — so traversal in either
direction is proportional to the fan-out, not the graph size. Property values use
an order-preserving encoding so a secondary index supports both equality and
range predicates.

## Project layout

```
cmd/mycypher/            CLI/REPL entrypoint (single binary)
internal/
  storage/               Store interface + engine adapters
    codec/               order-preserving key & value encoding
    badger/              BadgerDB adapter (default)
    bolt/                bbolt adapter (optional)
  catalog/               dictionaries, ID counters, index registry
  graph/                 model + transactional CRUD + traversal primitives
  cypher/
    ast/                 AST types
    parser/              hand-written lexer + recursive-descent/Pratt parser
    sema/                semantic analysis
    plan/                rule-based planner + EXPLAIN
    exec/                executor operators (Volcano)
mycypher.go              public embeddable API (package mycypher)
docs/adr/                architecture decision records
```

The public, embeddable API lives in the root `mycypher` package; everything else
is under `internal/`.

## Supported Cypher (MVP scope)

- `MATCH` / `OPTIONAL MATCH` with node/relationship patterns, directions, labels
  and types; `WHERE` with comparisons, `AND`/`OR`/`NOT`, property access and label
  predicates.
- `RETURN` with projection, aliases, `DISTINCT`, `ORDER BY`, `SKIP`, `LIMIT`.
- `WITH` chaining and scope reset.
- Variable-length paths `-[:T*1..3]->` (trail semantics: no repeated
  relationships).
- Aggregations: `count`, `collect`, `sum`, `avg`, `min`, `max` with implicit
  grouping.
- Write clauses `CREATE`, `MERGE`, `SET`, `DELETE`, `DETACH DELETE`, plus
  `CREATE INDEX`, and `$param` parameters.

Parsing, semantic analysis and planning cover the read side today; execution of
the above is delivered incrementally (see the roadmap).

**Out of scope for v1:** OLAP/vectorized execution, distribution/replication,
`shortestPath`, subqueries (`EXISTS { }`, `CALL { }`), and full TCK conformance.

## Roadmap

Development proceeds in phases (details in `PLAN.md`):

- [x] Phase 0 — Scaffolding
- [x] Phase 1 — Storage layer + codec
- [x] Phase 2 — Graph layer (CRUD + traversal primitives)
- [x] Phase 3 — Parser → AST
- [x] Phase 4 — Semantic analysis
- [x] Phase 5 — Logical plan + rule-based planner
- [x] Phase 6 — Executor (read path): first end-to-end query
- [x] Phase 7 — Write path (Cypher)
- [ ] Phase 8 — Advanced projection and traversal
- [ ] Phase 9 — Indexes managed via Cypher + CLI/REPL
- [ ] Phase 10 — Hardening (fuzzing, TCK subset, benchmarks, crash recovery)

## Documentation

- `DESIGN.md` — high-level architecture (source of truth). *(in Italian)*
- `PLAN.md` — phased development plan. *(in Italian)*
- `docs/adr/` — architecture decision records. *(in Italian)*

Code (identifiers, comments and strings) is in English.
