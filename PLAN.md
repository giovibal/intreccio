# PLAN — Phased development plan

> Each phase has a concrete deliverable and a "done when…" criterion.
> Philosophy: **storage-first**, then an end-to-end **vertical slice** as soon as
> possible (a query that actually runs), then widen the coverage.
> Architectural reference: `DESIGN.md`.

## Working principles
- **Test-first on the codec**: the key/value encoding is the foundation; any bug
  here propagates everywhere. Write the tests first (round-trip + ordering).
- **Vertical slice early**: as soon as the storage holds up, run `MATCH (n:L)
  RETURN n` end-to-end. Having something executable guides everything else.
- **One mutation, one point**: all writes go through a single graph-layer API
  that keeps record + indexes in the same transaction.
- Every phase closes with green tests and, where indicated, a benchmark.

---

## Phase 0 — Scaffolding
**Deliverable:** a compilable, empty but structured project.
- `go mod init`, package layout as in `DESIGN.md §11`.
- Toolchain: `golangci-lint`, `go test`, `Makefile`/`Taskfile` targets.
- Minimal CI (build + test + lint).
- `cmd/intreccio` with a main that opens/closes an empty DB.

**Done when:** `go build ./...`, `go test ./...`, `golangci-lint run` pass in CI.

---

## Phase 1 — Storage layer + codec
**Deliverable:** transactional KV persistence with correct encoding.
- `Store`/`Txn`/`Iterator` interface (`internal/storage`).
- Badger adapter (`internal/storage/badger`).
- `internal/storage/codec`:
  - key encoding (tag + big-endian ID, helper for each `n/e/o/i/l/p` keyspace).
  - order-preserving value encoding (int/float/string with `0x00` escape).
  - encoding/decoding of node and edge records.
- `internal/catalog`: name↔id dictionaries, ID counters, index registry.

**Tests:**
- Round-trip of every encoding.
- **Property test on ordering**: for randomly generated values,
  `bytes.Compare(enc(a), enc(b))` respects the logical order of `a,b` (per type).
- Idempotent and concurrent dictionary interning.

**Done when:** the ordering property tests pass for int, float, string, and the
records round-trip without loss.

---

## Phase 2 — Graph layer (CRUD + traversal primitives)
**Deliverable:** an internal API to manipulate the graph, with always-consistent
indexes.
- `internal/graph`: `CreateNode`, `CreateEdge`, `SetProperty`, `DeleteNode`,
  `DeleteEdge`, `GetNode`, `GetEdge`.
- Every mutation updates record + `l`/`p`/`o`/`i` in the **same** `Update`.
- Traversal primitives: `OutEdges(nodeID, typeID)`, `InEdges(...)`,
  `NodesByLabel(labelID)`, `NodesByProperty(labelID, keyID, value)`.

**Tests:**
- Creating a node with a label → it shows up in `NodesByLabel`.
- Creating an edge → it shows up both in `OutEdges(src)` and `InEdges(dst)`.
- Deleting a node/edge → records **and** all index entries disappear (explicit
  check of Invariant #1).

**Done when:** a test builds a small graph and all read primitives return
consistent results after create/update/delete.

---

## Phase 3 — Parser → AST
**Deliverable:** from Cypher text to the AST for the MVP slice.
- `internal/cypher/ast`: AST types (query, clauses, patterns, expressions).
- `internal/cypher/parser`: choice between
  - **ANTLR-gen** from the official openCypher grammar (a full parser "for free",
    then visitor → custom AST), or
  - **hand-written** (recursive descent + Pratt for expressions) limited to the
    slice.
  - *Recommended decision:* start with ANTLR-gen to cover the grammar, or
    hand-written if you want a clean AST and zero dependencies from the start.
    Record the choice in a short ADR.

**Tests:** parse a corpus of valid MVP queries → expected AST; invalid queries →
errors with position.

**Done when:** all queries in the MVP corpus produce the correct AST.

---

## Phase 4 — Semantic analysis
**Deliverable:** a resolved and validated AST.
- `internal/cypher/sema`: variable scope resolution (including reset on `WITH`),
  binding of labels/types/properties to dictionary IDs, basic type-checking.
- Clear semantic errors (undefined variable, etc.).

**Tests:** correct scoping across `WITH`; errors on unbound variables.

**Done when:** `WITH` scoping is correct and bindings to internal IDs are
resolved.

---

## Phase 5 — Logical plan + rule-based planner
**Deliverable:** from the resolved AST to an executable physical plan.
- `internal/cypher/plan`: logical operators, pattern→plan translation, anchor
  selection by selectivity, filter push-down, binding to access methods.

**Tests:** for known queries, the plan picks the expected anchor (e.g. uses the
`p` index when there is equality on an indexed property instead of a label scan).

**Done when:** the MVP queries produce sensible, inspectable physical plans (a
textual `EXPLAIN` is useful).

---

## Phase 6 — Executor (read path)
**Deliverable:** **vertical slice** — a read query that runs end-to-end.
- `internal/cypher/exec`: iterator operators (`NodeByLabelScan`,
  `NodeByProperty`, `NodeById`, `Expand`, `Filter`, `Project`, `Limit`).
- Wiring of the public `Query` → parser → sema → plan → exec → results.

**Tests:** on a seed graph, `MATCH (p:Person)-[:KNOWS]->(f) WHERE p.email=$e
RETURN f.name` returns the correct results.

**Done when:** the first end-to-end query with an `Expand` returns correct
results from disk.

---

## Phase 7 — Write path (Cypher)
**Deliverable:** mutations via Cypher.
- `CREATE`, `SET`, `DELETE`, `DETACH DELETE`, `MERGE`.
- Decision between a single `Query` vs separate `Query`/`Execute`.
- `MERGE` with correct match-or-create semantics.

**Tests:** create+match in the same flow; `MERGE` does not duplicate;
`DETACH DELETE` removes a node and its incident edges with consistent indexes.

**Done when:** the graph can be populated and modified entirely in Cypher.

---

## Phase 8 — Advanced projection and traversal
**Deliverable:** coverage of the light analytical queries of the MVP slice.
- `ORDER BY`, `SKIP`, `LIMIT`, `DISTINCT`.
- Full `WITH` chaining.
- Aggregations (`count`/`collect`/`sum`/`avg`/`min`/`max`) with implicit
  grouping.
- `VarLengthExpand` `*lo..hi` with relationship-ID tracking (no-repeat).

**Tests:** aggregations with grouping; variable-length paths over a graph with
cycles (verify no-repeated-relationship).

**Done when:** the MVP slice of `DESIGN.md §8` is covered and tested.

---

## Phase 9 — Indexes and integration
**Deliverable:** indexes managed via Cypher and used by the planner.
- `CREATE INDEX` on `(:Label).prop`; backfill of existing data.
- The planner picks the index when available.
- CLI/REPL in `cmd/intreccio` for interactive use.

**Tests:** after `CREATE INDEX`, a query with equality uses the index (verifiable
via `EXPLAIN`) and the results stay identical.

**Done when:** creating an index changes the plan and speeds up the query with
identical results.

---

## Phase 10 — Hardening
**Deliverable:** robustness and confidence.
- Property/fuzz tests on the parser and the codec.
- A subset of the openCypher TCK tests (the relevant Cucumber `.feature` files).
- Benchmarks: insert throughput, 1–3 hop traversal latency, indexed query.
- Crash-recovery test (reopen after a kill during a write).
- Public API documentation + examples.

**Done when:** the chosen TCK subset is green, the benchmarks are tracked and the
reopen after crash is consistent.

---

## Recommended order of attack for CC
1. Phases 0–2 in tight sequence (foundation; do not skip the codec property
   tests).
2. Phases 3→6 aiming for the **first end-to-end query** (vertical slice) as soon
   as possible, even with minimal Cypher coverage.
3. From there widen (7→9) one clause at a time, always with tests.
4. Phase 10 continuously, not only at the end.
