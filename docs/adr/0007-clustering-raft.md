# ADR 0007 — Optional Raft-replicated clustering for high availability

Status: accepted — 2026-05-30

## Context
`mycypher` is an embedded, single-binary, pure-Go graph DB. DESIGN §12 listed
distribution as future work and CLAUDE.md stated "no distribution in v1". We now
want multiple services that embed the library to **share one logical database**
with **high availability (HA)**, configured **from the library** (no separate
server to deploy).

Requirements that shape the decision:
- HA is the primary goal. HA *requires* redundancy — a node loss is only
  survivable if the data exists elsewhere — so some replication is unavoidable.
- Avoid replicating to *every* instance if possible.
- Preserve transactionality: the engine is ACID today and the graph invariants
  (index consistency, double adjacency — see DESIGN §7) must not be weakened.
- Stay pure Go, no cgo.

## Decisions

### Clustering is an opt-in capability, not a core change
The default embedded path (`mycypher.Open`/`OpenInMemory`, the CLI) is unchanged
and **does not link** the clustering code or its dependencies. Clustering lives in
the opt-in public `cluster` package (`cluster.Open`, `cluster.Config`), wired into
the core through a small `mycypher.Backend` seam so the core package never imports
the clustering code. This keeps the single-binary, minimal-dependency identity
intact for embedded users. CLAUDE.md and DESIGN §12 are updated to reposition distribution as a **v2,
opt-in** capability rather than a forbidden one.

### Consensus: a Raft-replicated state machine
We replicate via Raft (`github.com/hashicorp/raft`, with
`github.com/hashicorp/raft-boltdb/v2` as the log/stable store). Raft gives a
single linearizable command log: **one Cypher write statement = one Raft log entry
= one atomic apply** on every replica. This maps exactly onto the existing
invariant that all writes go through a single atomic `Store.Update` (DESIGN §7).

Rejected alternatives:
- **Gossip + CRDT** (memberlist/serf, eventually consistent): would lose
  transactionality and make the graph invariants very hard to maintain under
  concurrent conflicting writes. Incompatible with the transactional requirement.
- **libp2p + Raft**: libp2p is a WAN/P2P transport (discovery, NAT traversal),
  unnecessary for a datacenter quorum and a large dependency tree. Revisit only if
  WAN/P2P clustering is ever required.
- **NATS JetStream as the replication log**: pure Go and previously earmarked, but
  adds a broker concept and is itself Raft underneath. More moving parts for no
  gain here.
- **External shared store (FoundationDB/TiKV)**: breaks "no cgo" and/or
  "single binary / embeddable".

### Replicate effects (write-set), not commands
The leader executes a write with the normal parse→sema→plan→exec pipeline inside a
**recording transaction** that captures the ordered `Set`/`Delete` operations.
That write-set is the Raft log entry; every node applies it verbatim via
`Store.Update`.

This is sound because the engine keeps **no in-memory state**: node/edge IDs
(`catalog.NextNodeID/NextEdgeID`), dictionaries (`catalog.InternLabel/Type/Key`)
and the index registry all read/write through the transaction
(`internal/catalog/catalog.go`). Capturing the write-set therefore captures the
*complete* effect of a statement. Replaying effects (not re-executing Cypher)
removes every determinism hazard (clock, rand, map iteration order) and preserves
invariant #1 for free — the leader already emitted all index keys into the
write-set.

### Topology: a small quorum of data nodes plus stateless clients
A fixed set of **3 or 5 voting data nodes** forms the Raft quorum and holds the
data. Other service instances join as **stateless clients** that forward writes to
the leader and reads to a data node, storing nothing. This delivers HA while
avoiding replication to *every* instance (Raft quorums also degrade past ~5–7
voters, so unbounded voting membership is undesirable anyway). A non-voting local
read replica role is a later refinement.

### Replication factor: full replication to the quorum (v1)
Each data node holds the entire dataset. Simple and correct, and sufficient for
the target workload (OLTP / medium graphs). **Sharding** (each node holding only a
subset, with per-shard replication) is explicitly deferred: it requires
cross-shard distributed transactions and a distributed planner — effectively a
different product.

### Reads: configurable per query
Default to fast **local** snapshot reads (possibly slightly stale); offer opt-in
**linearizable** reads via a Raft barrier / ReadIndex (or leader forwarding) for
read-your-writes. Dataless clients always forward reads to a data node.

## Validation (Phase A spike)
A spike (`cluster/spike_test.go`) confirmed, under `-race` and with
`CGO_ENABLED=0`:
- A 3-node in-process Raft cluster (hashicorp/raft + raft-boltdb/v2 log store over
  the inmem transport) replicates commands to all three local Badger stores.
- Badger `Backup`/`Load` round-trips cleanly — the mechanism the real
  `FSM.Snapshot`/`Restore` will use.
- The whole stack builds pure Go (no cgo).

## Consequences
- New direct dependencies: `hashicorp/raft`, `hashicorp/raft-boltdb/v2` (the latter
  brings `go.etcd.io/bbolt`, aligning with the planned bbolt adapter). All pure Go.
  Linked only when the `cluster` package is imported.
- The public API gains the `cluster` package (`cluster.Open`/`cluster.Config`) and
  a `mycypher.Backend` extension seam; a per-query read consistency option arrives
  in Phase C. `Open`/`Query` semantics for embedded use are unchanged.
- Scope boundaries (see the plan): no sharding, no multi-statement interactive
  transactions across round-trips, no WAN/dynamic discovery in this iteration.
