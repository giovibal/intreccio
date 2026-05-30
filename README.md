# mycypher

An **embedded**, **single-binary** graph database written in **pure Go** (no cgo)
that speaks a useful subset of **openCypher 9**. It targets **OLTP /
knowledge-graph** workloads: point lookups and few-hop traversals over
medium-sized graphs. It is *not* an analytical (OLAP) engine.

> Pre-1.0 and under active development, but functional end-to-end: reads,
> writes, aggregations (count/sum/avg/min/max/collect), `DISTINCT`,
> `ORDER BY`/`SKIP`/`LIMIT`, `WITH` chaining, variable-length traversal (trail
> semantics — no repeated relationships) and Cypher-managed indexes all run
> through the public API. An optional **Raft-replicated cluster mode** adds high
> availability (see [Clustering](#clustering-optional-high-availability)).

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

## Install

Requirements: Go 1.26+ (for importers and for building from source).

```bash
# use as a library in your own project
go get github.com/giovibal/mycypher

# install the CLI/REPL
go install github.com/giovibal/mycypher/cmd/mycypher@latest
```

Prebuilt CLI binaries for Linux, macOS and Windows (amd64/arm64) are attached to
each [GitHub Release](https://github.com/giovibal/mycypher/releases); download the
one for your platform and verify it against the published `checksums.txt`.

## Getting started

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

## Clustering (optional, high availability)

For high availability, mycypher can run as a **Raft-replicated** cluster,
configured entirely from the library. A small quorum of **voters** (3 or 5) holds
the data and replicates every write through Raft; additional service instances
join as dataless **clients** that forward queries to the voters — so you get HA
without replicating to every instance. Clustering lives in the opt-in `cluster`
package and is **linked only when you import it**: programs that use
`mycypher.Open` stay embedded-only and never pull in Raft. See
`docs/adr/0007-clustering-raft.md`.

```go
import "github.com/giovibal/mycypher/cluster"

voters := []cluster.Peer{
    {ID: "n1", RaftAddr: "10.0.0.1:7000", ForwardAddr: "10.0.0.1:7001"},
    {ID: "n2", RaftAddr: "10.0.0.2:7000", ForwardAddr: "10.0.0.2:7001"},
    {ID: "n3", RaftAddr: "10.0.0.3:7000", ForwardAddr: "10.0.0.3:7001"},
}

// On each voter (exactly one sets Bootstrap: true to form the cluster):
db, err := cluster.Open(cluster.Config{
    NodeID: "n1", DataDir: "data/n1",
    BindAddr: "10.0.0.1:7000", ForwardAddr: "10.0.0.1:7001",
    Role: cluster.RoleVoter, Bootstrap: true, Peers: voters,
})

// On an extra app instance that should share the DB without storing it:
db, err := cluster.Open(cluster.Config{
    NodeID: "app-7", Role: cluster.RoleClient, Peers: voters,
})

// Same query API. One Cypher write = one linearizable transaction.
db.Query(ctx, "CREATE (n:Person {name: $n})", map[string]any{"n": "Bob"})
db.Query(ctx, "MATCH (p:Person) RETURN p.name", nil)                    // fast local read
db.Query(ctx, "MATCH (p:Person) RETURN p.name", nil, mycypher.Linearizable()) // strong read
```

Notes for operators:
- **Reads** default to fast, possibly slightly stale local snapshots; pass
  `mycypher.Linearizable()` for read-your-writes (served via the leader).
- **Writes** are serialized through the leader and committed by a quorum; losing
  a minority of voters keeps the cluster available, losing a majority halts writes
  (safety over availability — no split brain).
- Set `Config.LogOutput` (e.g. `os.Stderr`) for Raft logs and `Config.ApplyTimeout`
  to tune write/read-barrier deadlines.
- v1 uses **full replication** to the quorum (each voter holds the whole graph);
  sharding is out of scope.

A cluster-capable build of the CLI is available behind a build tag (it links Raft,
so it is not the default binary):

```bash
go build -tags cluster ./cmd/mycypher
mycypher -cluster-id n1 -cluster-data data/n1 \
  -cluster-bind 10.0.0.1:7000 -cluster-forward 10.0.0.1:7001 \
  -cluster-bootstrap -cluster-peers 'n1=10.0.0.1:7000=10.0.0.1:7001,...'
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

## Supported Cypher

Read side:
- `MATCH` / `OPTIONAL MATCH` with node/relationship patterns, directions, labels
  and types; `OPTIONAL MATCH` produces null bindings on no-match.
- `WHERE` with comparisons (`=`, `<>`, `<`, `<=`, `>`, `>=`), boolean ops
  (`AND`/`OR`/`NOT`), property access and label predicates,
  `IS NULL`/`IS NOT NULL`, `STARTS WITH`/`ENDS WITH`/`CONTAINS`, `IN` with list
  literals, `CASE WHEN ... THEN ... ELSE ... END` (simple and searched).
- `RETURN` with projection, aliases, `DISTINCT`, `ORDER BY`, `SKIP`, `LIMIT`.
- `WITH` chaining and scope reset; `UNWIND list AS x`.
- `UNION` / `UNION ALL`.
- Variable-length paths `-[:T*1..3]->` (trail semantics: no repeated
  relationships).
- Aggregations: `count`, `collect`, `sum`, `avg`, `min`, `max` (with `DISTINCT`).
- Scalar functions: `id`, `labels`, `type`, `keys`, `properties`, `size`,
  `length`, `head`, `last`, `tail`, `toInteger`, `toFloat`, `toString`,
  `toUpper`, `toLower`, `trim`, `substring`, `replace`, `split`, `abs`.

Write side:
- `CREATE`, `MERGE` (with `ON CREATE SET` / `ON MATCH SET`).
- `SET`: property assignment (`n.p = v`), label addition (`n:Foo`), map replace
  (`n = {...}`) or merge (`n += {...}`).
- `REMOVE`: property (`n.p`) or labels (`n:Foo`).
- `DELETE` / `DETACH DELETE`.
- `CREATE INDEX FOR (v:Label) ON (v.prop)` with on-demand backfill.
- Parameters (`$name`).

**Out of scope for v1:** OLAP/vectorized execution, distribution/replication,
`shortestPath`/`allShortestPaths`, subqueries (`EXISTS { }`, `CALL { }`), user-defined
procedures, path variables (`p = (...)`) and full TCK conformance.

## Versioning

Releases follow [Semantic Versioning](https://semver.org/) with a `v` prefix
(`vMAJOR.MINOR.PATCH`). The project is **pre-1.0**: within the `0.x` range a
**minor** bump may include breaking changes and a **patch** bump is reserved for
fixes and backward-compatible additions. The **on-disk storage format is not yet
frozen**, so a database created by one `0.x` version is not guaranteed to be
readable by another. A release is cut by pushing a tag; see
`docs/adr/0006-versioning-and-release.md` for the rationale.

## Documentation

- `DESIGN.md` — high-level architecture (source of truth).
- `docs/adr/` — architecture decision records.

All documentation and code (identifiers, comments and strings) is in English.

## License

Released under the [MIT License](LICENSE).
