# ADR 0004 — name→ID binding deferred to plan/exec

Status: accepted — 2026-05-25

## Context
Phase 4 (`PLAN.md`) calls for, besides scoping and validations, the "binding of
labels/types/properties to dictionary IDs". DESIGN §2 places this binding in
semantic analysis. However:
- name→ID resolution goes through the `catalog`, which requires a live
  `storage.Txn`;
- on the **read path** a non-existent label/type/key **is not an error**: the
  query simply matches zero rows. A "rigid" binding in sema would fail wrongly;
- on the **write path** (`CREATE`/`MERGE`/`SET`) names must be **interned** at
  execution time, not resolved ahead of time.

## Decision
Phase 4 (`internal/cypher/sema`) stays **pure, with no dependency on storage**: it
does scoping (with reset on WITH), validation of variable references, basic checks
(mandatory aliases in WITH, final RETURN, no nested aggregations, CREATE INDEX on
its own) and computes the **output columns**.

The **name→ID binding** is moved to plan/exec (Phase 5/6), where the query runs
inside a transaction: the read path will use `catalog.Lookup*` (absent ⇒ empty
result), the write path will use `catalog.Intern*`.

## Consequences
- `sema.Analyze(*ast.Query) (*Result, error)` is testable without opening a DB.
- The public pipeline (`Query`) will open a `View`/`Update`, and inside it run
  parse → sema → plan (with ID resolution via the txn) → exec.
- It is a deviation from the letter of `PLAN.md` (binding in sema) but it respects
  its intent: scoping is correct and ID resolution happens, only in the layer
  where the transaction exists and where it is semantically correct.
