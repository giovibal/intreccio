# ADR 0005 — Rule-based planner (Phase 5)

Status: accepted — 2026-05-25

## Context
Phase 5 (`PLAN.md`): from the resolved AST to an executable physical plan. In v1
the planner is **rule-based**, with no cost model and no statistics (DESIGN §9):
the rules produce operators bound to access methods directly.

## Decisions

### Operators (op.go)
Leaves: `AllNodesScan`, `NodeByLabelScan`, `NodeByProperty`. Internal: `Expand`,
`Filter`, `Project`, `Aggregate`, `Sort`, `Skip`, `Limit`, `CartesianProduct`.
Logical and physical plan coincide (no separate logical phase in v1).

### Anchor selection (decreasing selectivity)
For each not-yet-bound node: `NodeByProperty` (label + equality on an **indexed**
property) > `NodeByLabelScan` (label) > `AllNodesScan`. Equalities are extracted
from inline properties and from the WHERE conjuncts (`var.prop = constant`). Index
information comes from a `Catalog.HasIndex(label,key)` interface (in production
backed by the txn — ADR 0004; a fake in tests).

### Expansion
From the anchor it expands rightward and leftward along the pattern chain;
traversing a relationship "backward" **inverts the direction** (`flip`: out↔in,
both unchanged). Relationships and anonymous nodes get synthetic names (`_n0`,
`_r0`).

### Filter push-down
The equality consumed by the anchor is not re-applied. Everything else becomes a
`Filter` above scan/expand: unconsumed WHERE conjuncts, unconsumed inline
properties, relationship properties and **labels of nodes reached via Expand**
(the access method only guarantees the label of the scanned node).

### Projection
`RETURN`/`WITH` → `Project` (or `Aggregate` if aggregation functions appear; group
key = non-aggregated items). The `Sort`/`Skip`/`Limit` tail follows. `WITH` resets
the scope to the projected columns; `WITH ... WHERE` filters after the projection.

### EXPLAIN
`Explain(Op)` renders the plan as an indented textual tree (root at the top),
inspectable and used by the tests.

## Known limitations (accepted for v1)
- Writes (`CREATE`/`MERGE`/`SET`/`DELETE`) and `CREATE INDEX` are not yet planned
  (Phase 7/9): the planner returns an explicit error.
- Execution of `Aggregate` and `VarLengthExpand` is completed in Phase 6/8; here
  the planner produces them to make them inspectable.
- Join between unconnected patterns via `CartesianProduct` (no join reordering).
