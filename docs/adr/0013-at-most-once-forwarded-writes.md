# ADR 0013 — Forwarded writes are at-most-once

Status: accepted — 2026-11-12

## Context
A write on a member that is not the leader travels to the leader over the
forwarding protocol (ADR 0011). Three things could then go wrong after the
leader had already committed it through Raft:

- the connection failed before the reply arrived, and the connection pool
  sent the request again on a fresh connection;
- a dataless client, given a connection error or any uncoded remote error,
  moved on to the next voter and sent the write there too;
- the leader's Raft apply failed in a way that does not rule out a commit
  (leadership lost while committing, say), was reported as a plain error, and
  was retried like one.

In each case the write could be applied **twice**: a duplicated `CREATE` is
silent corruption. ADR 0011 recorded this as "at-least-once writes" and left
the decision open (issue #77).

Two fixes were on the table: make retries safe with a request ID that the
leader remembers, replicated through the log; or stop retrying a write whose
outcome is not known and say so to the caller.

## Decision
**A write is sent at most once past the point where it may have been applied.
When that point is passed and no answer comes back, the caller gets
`ErrWriteOutcomeUnknown` and nothing is retried.**

`ErrWriteOutcomeUnknown` is the only error after which a write may have taken
effect. Every other error of a write means it did not.

### Where the outcome becomes unknown
- **On the wire.** A request is delivered whole or not at all: the receiver
  acts on a frame only once it has read all of it. So a write is retried, on
  the same member or on another, as long as its request frame did not leave in
  full (the dial failed, the pooled connection was already broken, the frame
  write failed). Once it has left, a connection failure or the end of the
  caller's context is an unknown outcome. An error *reply* from the member is
  an answer, and is passed on as it is.
- **In Raft.** `ErrNotLeader`, `ErrEnqueueTimeout` and
  `ErrLeadershipTransferInProgress` reject an entry before it enters the log:
  the write did not happen. Any other failure of the apply leaves an entry that
  this leader or its successor may still commit: unknown.
- **After the commit.** A write whose result cannot be returned (it does not
  encode, or a row exceeds the frame limit) was applied all the same, and is
  reported as unknown rather than as a failure.

### How it travels
The condition is a wire error code, `CodeOutcomeUnknown`, so a voter that
proxies a write to the leader passes it on unchanged and the client that
receives it stops there instead of trying the next voter. The root package
maps the code to `ErrWriteOutcomeUnknown`, also for a write run on the leader
itself. When the caller's own context ended the wait, the error matches the
context's error as well.

Adding an error code does not change the frame format, and a member that does
not know the code treats it as a definite error and does not retry: the
protocol version stays 1.

Reads are unaffected: they are retried as before.

## Consequences
- A forwarded write is never applied twice. The cost is an ambiguity the
  caller must handle: read back what the write was meant to change, or repeat
  it when it is idempotent (`MERGE`, `SET`).
- Failover is less transparent for writes than for reads. A write sent on a
  connection that had just died, but was not yet known to be dead, is reported
  as unknown although it was never received; until the keepalive notices an
  unreachable leader (ADR 0011), writes sent to it end the same way instead of
  moving to another voter.
- A leader that answers "deadline exceeded" or "cancelled" stopped while
  staging the write, before replication: that stays the plain context error.
- Idempotent writes through a request ID remain possible on top of this: they
  would turn most unknown outcomes into safe automatic retries, and would still
  need this error for the retries that run out.
