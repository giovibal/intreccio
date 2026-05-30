# ADR 0002 — Record format, value encoding and catalog keyspace

Status: accepted — 2026-05-25

## Context
Phase 1 (`PLAN.md`): storage layer + codec. We need to fix (a) the serialization
format of node/edge records (left open in `DESIGN.md §5`), (b) the relationship
between order-preserving value encoding and record serialization, (c) the catalog
keyspaces, (d) the semantics of `Store.Update` under conflict.

## Decisions

### Two distinct value encodings
- **Index value (`valEnc`, order-preserving)** — scalars only (null, bool, int64,
  float64, string), used in the `p` keys. Type tag in ordered bands, int with
  sign-bit flip, normalized float, string "ordered bytes" (escape
  `0x00`→`0x00 0xFF`, terminator `0x00 0x00`). It is the only encoding in which
  byte order must coincide with logical order.
- **Record value (length-prefixed)** — used in the values of the `n`/`e` records.
  Only the round-trip matters, not the order: self-describing format with
  uvarint/varint and length-prefix; it also supports `list` and `map` (not
  indexable). Simpler and faster than the order-preserving one where order is not
  needed.

### Record format: custom binary, stdlib-only
CBOR/MessagePack discarded: the required serialization is simple and
`encoding/binary` (uvarint/varint) is enough, avoiding a dependency (CLAUDE.md:
"prefer the stdlib"). Deterministic encoding (sorted labels and property keys).
- Node: `uvarint(nLabels) | labelID... | props`.
- Edge: `uvarint(typeID) | uvarint(src) | uvarint(dst) | props`.
- props: `uvarint(n) | (uvarint(keyID) | recValue)...`.

### Catalog keyspace
Tags disjoint from the graph ones (`n/e/o/i/l/p`):
`c`+kind (counters), `L`/`T`/`K`+name (name→id dictionaries), `R`+kind+id
(reverse), `X`+label+propKey (index registry). **IDs start at 1**; 0 is reserved
as "none".

### Store.Update retries on conflicts
On Badger (SSI) two concurrent `Update`s touching the same keys produce
`ErrConflict`. The adapter retries `fn` (capped at 100) until it commits.
Consequence: `fn` must be free of side effects external to the transaction. This
makes concurrent dictionary interning correct and simple without retry logic in
the catalog.

## Consequences
- The graph layer (Phase 2) will use `codec` for keys/records and `catalog` for
  IDs, without knowing the engine.
- Adding indexable `list`/`map` is out of scope (v1): they remain only in the
  records.
