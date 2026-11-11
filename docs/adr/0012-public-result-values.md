# ADR 0012 — A canonical row encoding for result values

Status: accepted — 2026-11-10 (revised after #71 defined the public value model)

## Context
Live queries (ADR 0010) and the framed transport (ADR 0011) both need result
rows that can be **compared** and **shipped**: the live-query diff needs an
identity for a row, and the wire needs a format that carries every result
value, nulls nested in lists and maps included.

The public value model is the one of `values.go`: a result value is `nil`,
`bool`, `int64`, `float64`, `string`, `[]any`, `map[string]any`,
`intreccio.Node`, `intreccio.Relationship` or `intreccio.Path`. Before the
transport of ADR 0011, rows crossed the cluster through gob, which needs every
type registered, cannot encode a nil inside a container without a stand-in,
and gives a row no stable identity.

## Decisions

### A canonical row encoding
One row encodes to one self-delimiting byte string: a tag per value, varints for
integers and lengths, map entries in ascending key order, nodes, relationships
and paths as their fields (layout in `rowcodec.go`). It is **canonical** — equal
rows give identical bytes, and the decoder rejects any other form — so an
encoded row is also the row's identity. That is what the live-query diff keys
on and what the wire carries, for rows and for query parameters alike.

The decoder treats its input as untrusted: bounded nesting, counts checked
against the remaining bytes, no panics (fuzzed).

It is deliberately separate from the on-disk record format of
`internal/storage/codec`: that one stores dictionary IDs and is private to the
store; this one stores names and is meant to leave the process.

### The cluster layer carries rows as opaque bytes
The seam between the root package and a clustered backend (`Forward`,
`SetLocalExecutor`, and their live-query counterparts) exchanges encoded rows
and parameters. The value types and the codec live in the root package;
`cluster` neither knows the value model nor registers types with anything.

Rejected: keeping gob with the value types registered. It works for plain
forwarding but keeps the nil stand-in and gives no row identity.

## Consequences
- The forwarding wire format changed with ADR 0011: the members of a cluster
  must run the same version across that change.
- A node's labels are encoded in the order the store returns them (by label
  ID), which is deterministic for a given node state.
