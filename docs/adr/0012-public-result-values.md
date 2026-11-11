# ADR 0012 — Public result values and a canonical row encoding

Status: accepted — 2026-11-10

## Context
`RETURN n` used to put the executor's internal `graph.Node` straight into
`Result.Rows`: a type callers cannot name, with labels and property keys as
dictionary IDs. The same value could not cross the cluster either — gob refused
it (`type not registered for interface`), so a forwarded query returning a whole
node or relationship failed.

Live queries (ADR 0010) and the framed transport (ADR 0011) both need rows that
can be **compared** and **shipped**, so the row format has to be public, stable
and self-contained first.

## Decisions

### A public value model
A result value is one of: `nil`, `bool`, `int64`, `float64`, `string`, `[]any`,
`map[string]any`, `intreccio.Node`, `intreccio.Relationship`.

    type Node struct { ID int64; Labels []string; Props map[string]any }
    type Relationship struct { ID int64; Type string; Start, End int64; Props map[string]any }

IDs are the ones `id()` returns. Labels are **sorted**, so the same node state
always exports identically whatever order its labels were added in. `Props` is
never nil. The relationships of a variable-length match export as a list.

The conversion happens at the single execution path (`runQuery`), while the
transaction is still open because it resolves names through the dictionaries.
The executor keeps working on internal records.

### A canonical row encoding
One row encodes to one self-delimiting byte string: a tag per value, varints for
integers and lengths, map entries in ascending key order (layout in
`rowcodec.go`). It is **canonical** — equal rows give identical bytes, and the
decoder rejects any other form — so an encoded row is also the row's identity.
That is what the live-query diff keys on and what the wire carries.

The decoder treats its input as untrusted: bounded nesting, counts checked
against the remaining bytes, no panics (fuzzed).

It is deliberately separate from the on-disk record format of
`internal/storage/codec`: that one stores dictionary IDs and is private to the
store; this one stores names and is meant to leave the process.

### The cluster layer carries rows as opaque bytes
The seam between the root package and a clustered backend (`Forward`,
`SetLocalExecutor`) now exchanges encoded rows. The types and the codec live in
the root package; `cluster` neither knows the value model nor registers result
types with gob.

Rejected: registering `Node`/`Relationship` with gob. It fixes forwarding but
keeps the nil-in-container limitation and gives no row identity.

## Consequences
- `RETURN n` / `RETURN r` now yield usable public values, embedded and
  forwarded.
- The forwarding reply format changes (`ForwardReply.Rows` is `[][]byte`): the
  members of a cluster must run the same version across this change. Parameters
  travelled through gob until the transport of ADR 0011 replaced it; they now
  use this encoding as well.
- A parameter of an unsupported Go type returned as-is (`RETURN $p`) still works
  embedded and still fails when forwarded, now with an explicit encoding error.
