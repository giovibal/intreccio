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
`Cancel`, `Ping`. A connection starts with a handshake carrying the
**protocol version**, so incompatible members fail fast and explicitly. The
exact layout is in "The protocol as implemented" below.

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

## The protocol as implemented (version 1)
Encoding lives in `internal/wire`; scheduling in `cluster` (`mux.go` for the
dialling side, `server.go` for the accepting side, `pool.go` for one shared
connection per member address).

    connection = handshake frame*
    handshake  = "ITRC" version(2, big-endian) reserved(2)
    frame      = length(4, big-endian) stream(4, big-endian) type(1) payload

- **Handshake**: the dialling side sends first and the other side always
  answers, even on a mismatch, so the dialler reports which version it met.
  A peer that does not send the magic is dropped without a reply.
- **Frames**: `Query` (opens a stream), `Result` (column names), `Rows`
  (a chunk of rows), `End`, `Error`, `Cancel`, `Ping` / `Pong` on stream 0, and
  `Subscribe`, `Change`, `Ack` for live queries (below).
  The dialling side allocates stream IDs. A frame the receiver may not be sent
  closes the connection.
- **Request**: flags (write, linearizable), row cap, relative timeout, query
  text, parameters. The timeout is the caller's remaining deadline: the member
  that executes the query stops at it even if no `Cancel` arrives. Cancelling
  the caller's context sends `Cancel`, which cancels the execution's context;
  a proxying voter passes both on to the leader.
- **Errors** carry a code next to the message, so the sentinels
  (`ErrResultLimit`, `ErrConflict`, `ErrTxnTooLarge`, `ErrIntegerOverflow`,
  `ErrConstraintViolation`, `context.DeadlineExceeded`, `context.Canceled`)
  still match with `errors.Is` after forwarding. A deadline is coded only when
  it is the requester's own: a timeout of the serving member's making stays
  uncoded, so a client may still try another voter.
- **Limits**: 32 MiB per frame (so per row and per request); a result is split
  into `Rows` frames of about 256 KiB. A row over the limit is an error reply,
  not a broken connection. A frame is never trusted to deliver the length it
  announces. The decoders are fuzzed.
- **Keepalive**: the dialling side pings every 3 s; either side drops a
  connection silent for 10 s, cancelling the requests it was serving. This is
  what detects a member that died without closing, since a multiplexed
  connection cannot be abandoned just because one request timed out.
- **Retry**: as before, a request whose connection fails before the reply is
  complete is retried once on a fresh connection, and a dataless client then
  moves on to the next voter. See the caveat below.

### Subscriptions as implemented
A subscription is a second kind of stream, served by any voter from its own
replica (there is nothing to proxy to the leader):

    Subscribe  ->
               <-  Result            column names
               <-  Change+           one batch, in one or more frames
    Ack        ->                    "ready for the next batch"
               <-  Change+ ...
    Cancel     ->                    or  <- Error

- **Flow control is one batch at a time.** The voter sends a batch and waits
  for the `Ack`, which the client sends when the application has taken the
  batch. A slow application therefore holds the query on the voter, where
  commits coalesce (ADR 0010), and can never fill the connection its
  subscription shares with the client's other requests. A large batch — an
  initial result — is split over several `Change` frames.
- **The applied index is stored with the data.** `fsm.Apply` writes the Raft
  index of each write in the same transaction (catalog key `A`), so every
  evaluation reads the exact index of the state it saw, and every batch
  carries it. This replaces the "upper bound" of the original decision: an
  in-memory counter cannot be exact across a snapshot restore.
- **Failover** is in the client (`cluster/remote_sub.go`). It keeps a mirror of
  the result as a multiset of encoded rows. When the stream is lost, or the
  voter answers that it cannot serve (`CodeUnavailable`), the client
  resubscribes to the next voter with the last index it saw. That voter waits
  up to 5 s to have applied it (else `CodeBehind`), then sends its result; the
  client diffs it against the mirror and passes on only the difference — or
  nothing. An error about the query itself (`CodeGeneric`, `CodeResultLimit`)
  ends the subscription instead.
- **Bounds**: the client retries over the voters with backoff for 30 s, then
  ends the subscription with an error. If *every* voter answers `CodeBehind`,
  the cluster itself went back (a restore): the client drops the index
  requirement and follows it rather than stall.
- **Cleanup**: when the client cancels, closes or just disappears (keepalive),
  the stream's context ends and the voter unregisters the watcher.

What it does **not** do yet: a result is chunked on the wire but still
materialised on both ends — the executor returns whole results. Streaming rows
end to end needs an iterator API in the executor.

## Consequences
- **At-least-once writes (unchanged, now explicit)**: retrying after a
  connection failure cannot tell "never arrived" from "applied, reply lost", so
  a forwarded write can in rare cases be applied twice. This was already true
  with `net/rpc`; making forwarded writes idempotent is a separate decision.
  *Superseded by ADR 0013: a write that may have been applied is no longer
  retried, and the caller gets an explicit outcome-unknown error.*
- The forwarding wire format changes incompatibly: all members of a cluster must
  run the same minor version across this change (pre-1.0, a minor bump).
- `net/rpc`, gob and the nil-sanitising helpers are removed from the `cluster`
  package.
- Delivered in two steps: first the transport at feature parity (plus
  cancellation and row caps reaching the executing node), then subscriptions on
  top of it.
