# ADR 0001 — Naming e toolchain di base

Stato: accettato — 2026-05-25

## Contesto
Avvio del progetto (Fase 0 di `PLAN.md`). I documenti di design usavano il nome di
lavoro `grafo`, ma la repository è `github.com/giovibal/mycypher`. Vanno fissati
module path, nome del package pubblico/binario e gli strumenti di build/lint/CI.

## Decisione
- **Naming uniforme su `mycypher`**: module `github.com/giovibal/mycypher`,
  package pubblico `mycypher` (`mycypher.go`), binario `cmd/mycypher`. I
  riferimenti a `grafo` nei documenti sono stati aggiornati. Motivazione:
  coerenza con il nome della repository ed eliminazione del doppio nome.
- **Task runner: Makefile**. Nessuna dipendenza extra (a differenza di Taskfile,
  che richiede il binario `task`), in linea con "niente dipendenze inutili".
- **Lint: golangci-lint v2** (config `version: "2"`). La CI scarica il binario v2
  tramite l'action ufficiale, in modo deterministico e indipendente dall'ambiente
  locale.

## Conseguenze
- Il package radice si chiama `mycypher` pur essendo alla radice del modulo
  `.../mycypher`: import path e package name coincidono.
- Il binario locale `golangci-lint 1.59.1` è troppo vecchio per Go 1.26 e per il
  formato di config v2: va aggiornato a una release v2 per usare `make lint` in
  locale. La CI non è impattata (usa la versione pinnata nell'action).
- `go.mod` resta senza dipendenze fino alla Fase 1 (introduzione di BadgerDB).
