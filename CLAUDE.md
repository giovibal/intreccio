# CLAUDE.md

Project context for Claude Code. Read `DESIGN.md` for the architecture and
`PLAN.md` for the phases. This file holds the **operating rules** and the
**invariants** that must never be violated.

## What it is
`intreccio` — an **embedded**, **single-binary** graph DB in **pure Go** that
speaks a subset of **openCypher 9**. Target workload: **OLTP / knowledge-graph**
(lookups and few-hop traversals over medium-sized graphs). It is not an
analytical OLAP engine.

## Hard constraints (non-negotiable)
- **Pure Go. No cgo.** No dependency that requires a C toolchain. If a useful
  library requires cgo, it must be dropped or replaced.
- **Single binary**: must compile into a self-contained executable.
- **Storage engine**: BadgerDB by default, behind the `Store` interface. bbolt
  as an alternative adapter. The upper layer **must not** depend on the concrete
  engine.
- No OLAP, no vectorization. **Distribution/clustering is a v2, opt-in
  capability** (Raft-replicated, the public `cluster` package; see ADR 0007): the embedded
  single-binary default path must stay pure Go and must **not** link the
  clustering code or its dependencies.

## Correctness invariants
1. **Index consistency**: every mutation updates the base record **and all** of
   its index keys (`l`, `p`, `o`, `i`) **in the same transaction**
   (`Store.Update`). All writes go through a single point in `internal/graph`.
   Never write a record without updating its indexes in the same act.
2. **Order-preserving encoding**: keys use big-endian IDs; values in `valEnc`
   use the order-preserving encoding defined in `DESIGN.md §5`. Never introduce
   a length-prefix on indexed strings (it breaks ordering).
3. **No repeated relationships** in variable-length paths (openCypher 9 trail
   semantics): track the **relationship IDs** traversed, not the nodes.
4. **Double adjacency**: every edge is written both in `o` (outgoing) and in `i`
   (incoming). The two views must stay in sync.

## Layout
```
cmd/intreccio/         CLI/REPL entrypoint
internal/storage/     Store interface + codec + adapters (badger, bolt)
internal/catalog/     dictionaries, ID counters, index registry
internal/graph/       model + transactional CRUD + traversal primitives
internal/exec/        executor operators (Volcano) — the engine
query/                public openCypher front-end: ast, parser, sema, plan
intreccio.go           public embeddable API (package intreccio)
cluster/               opt-in Raft clustering (public, built only when imported)
```
Public surface = the root `intreccio` package + the `query/*` openCypher
front-end + the opt-in `cluster` package. The engine (`internal/exec`)
and the write path (`internal/graph`, `internal/catalog`, `internal/storage`,
`internal/storage/codec`) stay under `internal/` — this is what keeps the
write-path invariants (below) enforceable and the on-disk format private.

## Commands
```bash
go build ./...                # build
go test ./...                 # test
go test -race ./...           # test with the race detector (use often)
go test -run TestCodec ./internal/storage/codec   # single package
golangci-lint run             # lint
go test -bench . ./...        # benchmark
```

## Code conventions
- Errors: wrap with `fmt.Errorf("...: %w", err)`; sentinel errors for
  recoverable cases (e.g. `ErrNotFound`). No `panic` on the normal path.
- `context.Context` as the first parameter in public query methods.
- No global state; the `*DB` encapsulates the `Store`.
- Tests: table-driven where it makes sense; **property tests** for the codec
  (round-trip + ordering); use `testing/quick` or `gopkg.in/check`, pure Go only.
- No needless dependencies: prefer the stdlib. Every new dependency must be
  justified and verified to be pure Go.
- All code in English: names, identifiers, comments and strings (error messages,
  logs, test messages). Documentation (`DESIGN.md`/`PLAN.md`) and the ADRs are
  also in English.

## How to work (for the agent)
- Proceed by **phases** as in `PLAN.md`; do not skip the codec foundation tests
  (Phase 1).
- Aim early for an end-to-end **vertical slice** (Phase 6) even with minimal
  Cypher coverage, then widen one clause at a time.
- Before extending Cypher coverage, check that the slice in `DESIGN.md §8` is not
  already sufficient: **avoid scope creep**.
- For each new operator or encoding: write the test first, then the
  implementation.
- When an architectural choice is not obvious (e.g. ANTLR-gen vs hand-written
  parser, record serialization format), record it in a short ADR under
  `docs/adr/` and proceed.

## Expected dependencies (all pure Go)
- `github.com/dgraph-io/badger/v4` — default storage engine.
- `go.etcd.io/bbolt` — alternative adapter (optional).
- Parser: ANTLR Go runtime (`github.com/antlr4-go/antlr/v4`) **if** the ANTLR-gen
  route is chosen; otherwise no dependency (hand-written parser).
- Record serialization: to be decided (custom stdlib `encoding/binary`, or a pure
  Go CBOR/MessagePack).

## What NOT to do
- Do not introduce cgo for any reason.
- Do not write a record without updating its indexes in the same transaction.
- Do not implement constructs outside the MVP slice until the slice is solid.
- Do not add a vectorized/columnar processor: out of scope by design.
