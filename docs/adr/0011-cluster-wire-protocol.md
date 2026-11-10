# ADR 0011 — A framed wire protocol for cluster forwarding and streaming

Status: accepted — 2026-11-10

## Context
Query forwarding between cluster members uses `net/rpc` with gob on
`ForwardAddr`. It is request/response only, so it cannot carry a live query
(ADR 0010) to a dataless client, and it has limits of its own:
- a result travels as one reply holding every row;
- neither cancellation nor `WithMaxRows` reaches the node executing the query;
- gob cannot encode a nil inside a slice or map, hence the `nullValue`
  stand-in and the `sanitize*` helpers.

We want one channel for queries and subscriptions, with no new dependency.

## Decisions

### A hand-written, multiplexed, framed protocol
One TCP (or TLS) connection per peer pair carries many logical streams:

    frame = length | streamID | type | payload

Frame types: `Query`, `Rows` (chunked), `End`, `Error`, `Subscribe`, `Change`,
`Cancel`, `Ping`. The first frame of a connection is a handshake carrying the
**protocol version**, so incompatible members fail fast and explicitly.

Rows and parameters travel in the canonical row encoding of ADR 0012; the
cluster layer treats them as opaque bytes and stays ignorant of the Cypher
pipeline. The existing TLS / mutual-TLS configuration (ADR 0009) applies
unchanged.

Coalescing (ADR 0010) keeps a shared connection healthy: for a slow remote
subscriber the serving voter keeps the watcher dirty instead of buffering.

Rejected:
- **gRPC**: streaming and cancellation for free, but a large dependency tree and
  a `protoc` code-generation step, against the stdlib-first, no-tools-to-install
  stance (ADR 0006). Its polyglot advantage is not needed: dataless clients are
  Go programs using `cluster.Open`.
- **`net/http` (HTTP/2 streaming bodies)**: stdlib and inspectable, but more
  overhead per request and awkward multiplexing on a plaintext cluster.

### Transparent failover for dataless subscriptions
A dataless client's subscription is served by one voter. If that voter is lost
the client library resubscribes to another one, which may be behind. To keep
this invisible to the application:
- every batch carries an **applied Raft index** that is an upper bound for the
  state it reflects;
- on resubscription the client sends the last index it saw, and the new voter
  waits until it has applied at least that index before answering;
- the client keeps a copy of the result set and diffs the fresh snapshot against
  it, so the application only sees the net difference.

The cost is the result set held in memory on the client as well.

## Consequences
- The forwarding wire format changes incompatibly: all members of a cluster must
  run the same minor version across this change (pre-1.0, a minor bump).
- `net/rpc`, gob and the nil-sanitising helpers are removed from the `cluster`
  package.
- Delivered in two steps: first the transport at feature parity (plus
  cancellation and row caps reaching the executing node), then subscriptions on
  top of it.
