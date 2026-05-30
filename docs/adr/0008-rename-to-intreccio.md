# ADR 0008 — Rename the project from `mycypher` to `intreccio`

Status: accepted — 2026-05-30

Supersedes the naming decision of ADR 0001.

## Context
The project was named `mycypher` (ADR 0001), with module path
`github.com/giovibal/mycypher`, public package `mycypher`, and binary
`cmd/mycypher`. Before any public/`v0.2.0` release we reassessed the name.

**Cypher** is a **registered trademark of Neo4j, Inc.** (US trademark, serial
86266998), and the registration explicitly covers *"software that implements a
computer programming query language for use in querying or accessing a computer
database"* — i.e. exactly this project's category. Implementing the **openCypher**
language is permitted and encouraged, but using "Cypher" as a *product/brand name*
(`mycypher`) in the same field creates real likelihood-of-confusion trademark
risk. Other Cypher implementations (Memgraph, FalkorDB, Apache AGE, KùzuDB)
advertise Cypher support but do not put "Cypher" in their names.

Renaming later — after the module has importers — is a breaking change to the Go
import path. Doing it now, while the repository is private and unreleased, is
cheap.

## Decision
Rename the project to **`intreccio`** (Italian for *interweaving / a web of
relationships*) — a fitting metaphor for a graph (an interweaving of nodes and
edges), distinctive, and clear in the software/database space (no conflicting
GitHub project, Go module, or software trademark found).

- Module path: `github.com/giovibal/intreccio`.
- Public package: `intreccio` (root `intreccio.go`).
- Binary / CLI: `cmd/intreccio`.
- The GitHub repository is renamed to `giovibal/intreccio` to match the module
  path.

We continue to **implement openCypher** and describe the project as speaking a
subset of Cypher (nominative/descriptive use); we simply no longer use "Cypher"
as the brand.

## Consequences
- All import paths, the package name, the binary name, and forward-looking docs
  (README, DESIGN, PLAN, CLAUDE) are updated to `intreccio`.
- Earlier ADRs (0001–0007) are historical records and are **left unchanged**;
  they still reference the `mycypher` name and the old module path as they were
  written.
- Consumers (once the project is public) import `github.com/giovibal/intreccio`
  and `github.com/giovibal/intreccio/cluster`.
- This is a one-time pre-release rename; no compatibility shim is provided.
