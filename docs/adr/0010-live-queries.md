# ADR 0010 — Live queries (reactive changefeeds)

Status: accepted — 2026-11-10

## Context
Applications that embed `intreccio` want to **react** to graph changes instead of
polling: a UI bound to a query, a cache to invalidate, an agent watching a
knowledge graph. We want a client to register a read query once and then receive
the changes to its result as writes are committed — embedded or clustered.

Facts about the code base that shape the decision:
- Every write commits through one `Store.Update`: `localBackend.ApplyWrite` when
  embedded, `fsm.Apply` on every voter when clustered.
- The Raft log carries the **physical write-set** (ordered `Set`/`Delete` on KV
  keys, ADR 0007), not Cypher. Followers never run `internal/graph`.
- The logical state lives in two tables only: `n` (labels + properties) and `e`
  (type, src, dst, properties). `l`/`p`/`o`/`i` are derived, catalog keys are
  bookkeeping.
- Relationships are immutable after creation (`SET` targets nodes only).

## Decisions

### Semantics: result-set diff with coalescing
A live query is a read query whose **result set** is kept up to date. The
subscriber receives batches `{Added, Removed}` of rows in exactly the shape of
`Result.Rows`:
- the first batch is the current result, all in `Added`, flagged as initial — a
  subscription starts like a normal read and then stays open;
- a row that stops matching is reported in `Removed`; a changed row arrives as
  the old row in `Removed` plus the new row in `Added` **in the same batch**.

Batches are **coalesced**: a watcher is only ever "dirty". A slow consumer does
not queue deltas; at its next evaluation it receives the net difference, so
intermediate states may be skipped. Memory stays bounded and the commit path is
never blocked by a subscriber.

Rejected: one notification per commit. It needs a per-watcher queue and a policy
for slow consumers, and most consumers only want the current state.

### Deltas are derived from the write-set at the commit point
A `storage.Txn` wrapper observes `Set`/`Delete` on `n`/`e` keys and reads the
before-image in the same transaction, producing a logical `ChangeSet` (node
created/updated/deleted with labels and properties before and after;
relationship created/deleted). It is published only after the commit succeeds,
and rebuilt on every attempt because `Store.Update` may retry on conflict.

The same wrapper is hooked at the two commit points, so the feature is identical
embedded and clustered, and **each voter serves its own watchers from its own
apply stream** — a leader change does not affect them. The cost is one extra
`Get` per touched record, paid only while at least one watcher exists.

Rejected:
- a hook in `internal/graph`: it would only fire on the leader;
- logical deltas inside the Raft log entry: changes and inflates the replicated
  format for something derivable locally.

### Evaluation: footprint filter, then re-run and diff (v1)
From the query AST each watcher extracts a **footprint**: the labels,
relationship types and property keys it references, with a wildcard when a
pattern has an unlabelled node or an untyped relationship. A commit whose
`ChangeSet` does not intersect the footprint does not wake the watcher.

A woken watcher re-runs its query in a read transaction and diffs the rows (as a
multiset keyed by the canonical row encoding of ADR 0012) against the last result
it emitted. This is valid for any read query, including aggregations and
`ORDER BY`/`LIMIT`.

v2 (an optimisation, same semantics): evaluate plain `MATCH … WHERE … RETURN`
queries **anchored** on the changed entity, reusing the planner's support for a
pre-bound variable (`Argument` leaf), so the cost follows the neighbourhood of
the change instead of the whole query. See "Anchored evaluation as
implemented" below.

### Anchored evaluation as implemented (v2)
`anchored.go` re-evaluates a live query around the nodes that changed.

- **Which queries**: `MATCH pattern [WHERE …] RETURN items` — one non-optional
  `MATCH`, fixed-length relationships, no path variable, a projection without
  aggregation, `DISTINCT`, `ORDER BY`, `SKIP` or `LIMIT`. For these the result
  is one row per match, and a match can only appear, disappear or change when
  a node it binds changes; a relationship change counts as a change to both of
  its ends. Every other query keeps the v1 path. (A variable-length match is
  excluded because it depends on nodes it does not bind.)
- **How**: the query is rewritten so that every pattern element has a name and
  the projection ends with each element's ID. Those IDs are the match's
  identity. The anchor keeps every match's row and node IDs, and an index from
  node to matches. Given the changed nodes, the stale rows are the matches the
  index lists for them; the rows that replace them come from matching the
  pattern outward from each changed node, once per node position, with a plan
  whose leaf is that node (`plan.PlanSeeded`, `exec.RunSeeded`).
- **Which nodes changed**: a tracking watcher records the node IDs of the
  commits that concern it. More than 4096 of them, or any change the hub
  cannot describe (a commit without capture, a snapshot restore), asks for a
  full run instead.
- **Still one consistent snapshot per batch.** A write is visible to readers a
  moment before it is reported. An evaluation therefore takes the dirty set,
  opens its read transaction, waits until every write that transaction could
  contain has been reported (`Hub.AwaitReported`, which blocks the evaluation,
  never a write), and takes the dirty set again. The union covers the
  snapshot; what the second take saw is re-marked, costing one extra round.
  Without this an evaluation could deliver rows of two different states.
- **The executor is the reference.** A match carries a count, so if the
  executor returns the same binding twice the anchor does too. A property test
  checks, over random mutation sequences and at every step, that the anchored
  result equals the query run from scratch and that each reported difference
  is exact.
- **Cost**: the benchmark renames one node in a graph of N disjoint
  `(a)-[:KNOWS]->(b)` pairs with a live query over all of them, and measures
  the write plus the next batch:

      pairs     anchored      full re-run
        100     0.14 ms         3.8 ms
       1000     0.11 ms        33 ms
      10000     0.16 ms       325 ms

  Anchored cost follows the neighbourhood of the change, not the result. The
  price is memory: the index roughly triples what a subscription holds per row.

### Syntax and API
- Go API (primary): `db.Subscribe(ctx, cypher, params)` returns a subscription
  exposing the columns, a channel of change batches, an error and `Close`.
- Text surface: a `STREAM` prefix (`STREAM MATCH … RETURN …`) sets a flag on
  `ast.Query`, for the REPL and for remote clients. `Query` rejects it, and a
  streamed query must be read-only.

`STREAM` is **not** openCypher 9. It is a deliberate, single-keyword extension
outside the MVP slice of DESIGN §8.

### Lifecycle and consistency
- A subscription ends on context cancellation, on `Close`, or when the database
  closes; all of its in-memory state is released.
- **No gaps**: a watcher registers *before* it runs its initial query, so any
  later commit marks it dirty.
- **Conservative wake-up**: if a write started without capture (no watcher
  existed yet) and watchers exist at commit, all of them are woken.
- **Snapshot restore** (clustered) replaces the state without deltas: all
  watchers are woken and the diff emits the net change.
- A per-subscription **row cap** (10 000 rows by default) bounds memory;
  exceeding it ends the subscription with an error.
- A backend opts in by accepting the hub its commit points report to
  (`SetChangeHub`); `Subscribe` refuses a backend that does not.

## Consequences
- This is the first **long-lived in-memory state** in the engine (registry,
  goroutines, one result set per watcher). It lives on the read side only: the
  write path still keeps no in-memory state, so the soundness argument of ADR
  0007 (the write-set is the complete effect of a statement) is untouched.
- v1 cost is (relevant watchers) × (write rate) query executions, on the node
  serving them. Fine for tens to hundreds of selective watchers; not for
  thousands of broad ones. v2 removes the per-evaluation cost for plain `MATCH`
  queries; aggregations and ordered or limited queries still pay it.
- Dataless clients need a streaming transport; see ADR 0011.
