# ADR 0001 — Base naming and toolchain

Status: accepted — 2026-05-25

## Context
Project bootstrap (Phase 0 of `PLAN.md`). The design documents used the working
name `grafo`, but the repository is `github.com/giovibal/intreccio`. The module
path, the public package/binary name and the build/lint/CI tooling need to be
fixed.

## Decision
- **Uniform naming on `intreccio`**: module `github.com/giovibal/intreccio`, public
  package `intreccio` (`intreccio.go`), binary `cmd/intreccio`. References to `grafo`
  in the documents have been updated. Rationale: consistency with the repository
  name and elimination of the double name.
- **Task runner: Makefile**. No extra dependency (unlike Taskfile, which requires
  the `task` binary), in line with "no needless dependencies".
- **Lint: golangci-lint v2** (config `version: "2"`). CI downloads the v2 binary
  via the official action, deterministically and independently of the local
  environment.

## Consequences
- The root package is named `intreccio` even though it is at the root of the
  `.../intreccio` module: import path and package name coincide.
- The local `golangci-lint 1.59.1` binary is too old for Go 1.26 and the v2
  config format: it must be upgraded to a v2 release to use `make lint` locally.
  CI is unaffected (it uses the version pinned in the action).
- `go.mod` stays dependency-free until Phase 1 (introduction of BadgerDB).
