# DESIGN — Embedded graph DB in pure Go (`mycypher`)

> High-level design document. It is the source of truth for the architecture.
> `PLAN.md` translates this design into development phases; `CLAUDE.md` extracts
> the conventions and operating invariants for the agent.

## 1. Goal and scope

An **embedded**, **single-binary** graph database in **pure Go** that speaks a
subset of **openCypher 9**. Designed for **OLTP / knowledge-graph** workloads:
point lookups and few-hop traversals over medium-sized graphs.

### Goals
- Pure Go, **no cgo**, deploy as a single binary.
- Property graph (nodes with labels and properties, typed edges with properties).
- A useful and solid subset of openCypher 9 (see §8).
- ACID transactions with always-consistent indexes.
- A clean embeddable API + a CLI/REPL.

### Non-goals (explicit)
- **No OLAP**: no vectorized/factorized processor, no massive analytical joins.
  That was Kùzu's value and it is the part that makes no sense to reimplement in
  Go.
- No full openCypher 9 TCK conformance in v1 (see §8).
- No distribution/replication in v1 (see §11, future evolution via NATS).
- No post-9 / GQL extensions (`CALL { }`, `EXISTS { }`, quantified path
  patterns).

## 2. Layered architecture

Query pipeline top to bottom, entirely in-process:

```
Cypher query (text)
   → Parser            → AST
   → Semantic analysis  (scoping, binding labels/types/properties to internal IDs)
   → Logical plan       (tree of logical operators)
   → Rule-based planner (anchor selection, filter push-down)
   → Physical plan      (operators bound to concrete access methods)
   → Executor           (iterator / Volcano model: Next())
   → Graph storage API  (transactional CRUD of nodes/edges/indexes)
   → Key encoding       (prefixed tables over an ordered KV store)
   → Storage engine     (Badger; pure Go; ACID SSI)
```

## 3. Decision: storage engine

**Default: BadgerDB.** Pure Go LSM, fast writes, ACID transactions with
serializable snapshot isolation, iterators with ordered prefix scans — exactly
what is needed for adjacency.

**Alternative: bbolt.** mmap B+tree, excellent reads, native ordered range scans,
maximum stability (etcd's storage). Limitation: single-writer (one write
transaction at a time), acceptable for read-heavy workloads.

**Abstraction.** The upper layer must not depend on the concrete engine. We
define a minimal `Store` interface (Get/Set/Delete/prefix Iterator + read/write
transactions) and provide a Badger adapter (default) and, if needed, a bbolt
adapter. This makes it possible to swap engines without touching the graph layer.

```go
type Store interface {
    View(func(Txn) error) error          // read-only
    Update(func(Txn) error) error        // read-write, atomic
    Close() error
}
type Txn interface {
    Get(key []byte) ([]byte, error)
    Set(key, val []byte) error
    Delete(key []byte) error
    Scan(prefix []byte) Iterator         // lexicographic order
}
```

## 4. Data model

Property graph:
- **Node**: internal `uint64` ID, set of **labels**, map of **properties**.
- **Edge** (relationship): internal `uint64` ID, single **type**, source and
  destination nodes, map of **properties**, direction.
- **Property values**: null, bool, int64, float64, string, list, map (the last
  two only as a serialized value, not indexable in v1).

Labels, relationship types and property keys are **interned strings** into
integer IDs (`uint32`) via dictionaries (§6), so they fit fixed-width into keys.

## 5. Key encoding — the heart of the design

Principle: over a lexicographically ordered KV store, choose an encoding where
**the byte order coincides with the desired logical order**, and **group by
prefix** so that hot queries become a single range scan.

Conventions:
- Internal node/edge IDs: `uint64` **big-endian** (8 bytes) → byte order = numeric
  order.
- Dictionary IDs (label/type/propKey): `uint32` big-endian (4 bytes).
- First byte = **table tag**.

| Tag | Key | Value | Purpose |
|-----|-----|-------|---------|
| `n` | `n` + nodeID(8) | node record (labels + properties) | node record |
| `e` | `e` + edgeID(8) | typeID, srcID, dstID, properties | edge record |
| `o` | `o` + srcID(8) + typeID(4) + dstID(8) + edgeID(8) | ∅ | **outgoing edges** (hot path) |
| `i` | `i` + dstID(8) + typeID(4) + srcID(8) + edgeID(8) | ∅ | incoming edges |
| `l` | `l` + labelID(4) + nodeID(8) | ∅ | label index |
| `p` | `p` + labelID(4) + propKeyID(4) + valEnc + nodeID(8) | ∅ | secondary property index |

### Adjacency (the `o` and `i` rows)
Every edge is written **twice**: once in `o` (for outgoing traversals) and once
in `i` (for incoming ones). "All outgoing KNOWS edges of X" becomes a prefix scan
on `o` + X.id + KNOWS.typeID, in time proportional to the fan-out and not to the
graph size. `edgeID` as the last component guarantees unique keys even in
multigraphs (multiple edges of the same type between the same pair).

### Records (the `n` and `e` rows)
The value is a compact serialization (e.g. a custom binary encoding or a format
like MessagePack/CBOR — to be decided in Phase 1; prefer something zero-alloc on
read). Records hold the "heavy" data; indexes hold only the keys needed to find
them.

### Order-preserving value encoding (`valEnc`)
This is the only truly delicate encoding. It is needed if you want range queries
(`age > 30`) and not just equality. Scheme:
- 1 **type tag** byte, in ordered bands: `NULL(0x00) < BOOL(0x01) < INT(0x02) <
  FLOAT(0x03) < STRING(0x04)`.
- **int64**: `binary.BigEndian(uint64(v) ^ (1<<63))` — flipping the sign bit
  makes two's complement orderable as unsigned.
- **float64**: `bits = math.Float64bits(v)`; if the sign is negative
  `bits = ^bits`, otherwise `bits |= 1<<63`; then big-endian.
- **string**: raw UTF-8. Since in the `p` key the string is **followed** by the
  nodeID, an unambiguous boundary that preserves order is required: escape
  `0x00` → `0x00 0xFF`, terminator `0x00 0x00` ("ordered bytes" encoding in the
  style of CockroachDB/FoundationDB). **Do not** use a length-prefix: it breaks
  lexicographic order.

For equality only (point lookup, e.g. find a node by `email`) the value can also
be hashed: simpler, but no ranges. Start with order-preserving only where it is
needed.

## 6. Catalog, dictionaries, ID allocation
- **Dictionaries** name↔id for labels, types, property keys (dedicated
  keyspaces, e.g. `L`/`T`/`K` for name→id and the reverses). Idempotent interning
  within a transaction.
- **Counters** for allocating monotonic nodeID/edgeID (`uint64`), in a keyspace
  `c`.
- **Index registry**: which `(label, propKey)` pairs have an active `p` index, so
  the write path knows which indexes to maintain.

## 7. Transactions and consistency invariants

> **Invariant #1 (non-negotiable):** every write updates the base record **and
> all** of its derived index keys (`l`, `p`, `o`, `i`) **within the same
> transaction**. If record and indexes do not commit atomically, the indexes
> diverge and the queries lie.

- Writes run in `Store.Update` (atomic transaction).
- Reads run in `Store.View` (consistent snapshot).
- On Badger, SSI provides serializable isolation; on bbolt the single-writer
  naturally serializes writes.

## 8. openCypher subset (MVP scope)

Included in v1:
- DML: `CREATE`, `MERGE`, `SET`, `DELETE`, `DETACH DELETE`.
- `MATCH` with node/edge patterns, directions, labels and types.
- `WHERE`: comparisons, `AND`/`OR`/`NOT`, property access, label predicates.
- `RETURN` with projection, aliases, `DISTINCT`.
- `ORDER BY`, `SKIP`, `LIMIT`.
- `WITH` for chaining and scope reset.
- **Bounded** variable-length paths: `-[:T*1..3]->`.
- Aggregations: `count`, `collect`, `sum`, `avg`, `min`, `max` with implicit
  grouping (keys = non-aggregated projection items).
- `CREATE INDEX` on `(:Label).prop`.
- Parameters: `$param`.

Deferred (post-v1, in likely priority order):
- A complete function library (added incrementally).
- `shortestPath` / `allShortestPaths`.
- Subqueries: `EXISTS { }`, `CALL { }`.
- `CALL` on defined procedures / functions.
- Full TCK conformance.

### Semantics not to get wrong
- **No repeated relationships** within the same path of a `MATCH` (openCypher 9
  "trail" semantics): while expanding variable-length paths, track the
  **relationship IDs** already traversed, not the nodes.
- `MERGE` = match-or-create: it first attempts a full match of the pattern, and
  only creates if that fails; mind the races with the transaction.
- `WITH` introduces a new scope: variables that are not re-projected are not
  visible downstream.
- `OPTIONAL MATCH` produces `null` on bindings that are not found.

## 9. Query pipeline — operators and mapping onto keyspaces

**Iterator (Volcano)** model: each operator exposes `Next() (Record, bool)`, the
root pulls. No vectorization: for a few hops the pull model is adequate.

Operators and access methods:
- `NodeByLabelScan(:L)` → prefix scan on `l` + labelID.
- `NodeByProperty(:L, k=v)` → prefix scan on `p` + labelID + keyID + valEnc.
- `NodeById(id)` → direct get on `n` + id.
- `AllNodesScan` → prefix scan on `n` (fallback, avoid if possible).
- `Expand(a)-[:T]->(b)` → for each `a`, prefix scan on `o` + a.id + typeID →
  produces the `b`; fetch `n` + b.id only if properties are needed.
- Reverse `Expand` → same mechanism on `i`.
- `VarLengthExpand(*lo..hi)` → depth-bounded BFS/DFS on `o`/`i`, tracking
  relationship IDs (see §8).
- `Filter`, `Project`, `OrderBy`, `Skip`, `Limit`, `Aggregate` → in-memory
  operators over the stream.

### Rule-based planner (v1)
Sufficient to be usable. Heuristic:
1. **Anchor selection** by decreasing selectivity:
   `NodeById` > equality on an indexed property (`p`) > `NodeByLabelScan` (`l`)
   > `AllNodesScan`.
2. **Expansion** outward following edges starting from the cheapest anchor.
3. **Push-down** of `WHERE` predicates onto scans as low as possible.

Cost-based with statistics (cardinalities, histograms) is **phase 2**, not needed
for v1.

## 10. Embeddable API (draft)

```go
db, err := mycypher.Open("data/")   // open/create the database
defer db.Close()

res, err := db.Query(ctx, `
    MATCH (p:Person {email: $email})-[:KNOWS]->(f)
    RETURN f.name AS name
    ORDER BY name LIMIT 10
`, map[string]any{"email": "a@b.com"})

for res.Next() {
    rec := res.Record()
    // rec.Get("name")
}
```

Reads and writes can share `Query` (the planner knows whether the plan writes) or
stay separate (`Query` read-only, `Execute` write). Decision in Phase 7.

## 11. Project layout (Go)

```
mycypher/
  cmd/mycypher/         # CLI/REPL entrypoint (single binary)
  internal/
    storage/            # Store interface + engine adapters
      codec/            # key & value encoding (order-preserving)
      badger/           # BadgerDB adapter (default)
      bolt/             # bbolt adapter (optional)
    catalog/            # dictionaries, counters, index registry
    graph/              # model + transactional CRUD + traversal primitives
    cypher/
      ast/              # AST types
      parser/           # parser (ANTLR-gen or hand-written)
      sema/             # semantic analysis / binding
      plan/             # logical+physical plan, rule-based planner
      exec/             # executor operators (Volcano)
  mycypher.go           # public embeddable API (package mycypher)
  CLAUDE.md DESIGN.md PLAN.md
  go.mod
```

> `internal/` for everything that is not public API; the embeddable API lives in
> the root `mycypher` package. Module name: `github.com/giovibal/mycypher`.

## 12. Future evolution (outside v1)
- **Replication / distribution (v2, opt-in)**: a Raft-replicated state machine
  configured from the library, for high availability. A small quorum of data
  nodes (3/5) holds the data; other instances join as stateless clients. One
  Cypher write = one Raft log entry = one atomic apply on every replica, by
  replicating the transaction's *effects* (write-set) rather than re-executing the
  query. Lives in the opt-in public `cluster` package and is linked only when
  used, so the embedded
  single-binary default stays pure Go and dependency-light. See ADR 0007. (NATS
  JetStream was considered as the replication log and rejected for this purpose.)
- Cost-based planner with statistics.
- Full-text and vector indexes (for GraphRAG), if the use case requires it.
- Extension toward GQL / post-9 constructs.

## 13. Main risks
- **Planner**: where ~70% of the engineering lives. Mitigation: an early vertical
  slice (a single pattern end-to-end) before widening Cypher coverage.
- **Order-preserving string encoding**: easy to get the boundary wrong.
  Mitigation: dedicated property tests on the codec (round-trip + ordering).
- **Index consistency**: see Invariant #1. Mitigation: all mutations go through a
  single point of the graph layer that updates record + indexes together.
- **Cypher scope creep**: the temptation to implement everything. Mitigation:
  stick to the MVP slice of §8.
