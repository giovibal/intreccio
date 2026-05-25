# PLAN — Piano di sviluppo a fasi

> Ogni fase ha un deliverable concreto e un criterio di "fatto quando…".
> Filosofia: **storage-first**, poi una **vertical slice** end-to-end il prima
> possibile (una query che gira davvero), poi si allarga la copertura.
> Riferimento architetturale: `DESIGN.md`.

## Principi di lavoro
- **Test-first sul codec**: il key/value encoding è la fondazione; ogni bug qui si
  propaga ovunque. Scrivere prima i test (round-trip + ordinamento).
- **Vertical slice presto**: appena lo storage regge, far girare `MATCH (n:L)
  RETURN n` end-to-end. Avere qualcosa di eseguibile guida tutto il resto.
- **Una mutazione, un punto**: tutte le scritture passano da un unico API del
  graph layer che mantiene record + indici nella stessa transazione.
- Ogni fase chiude con test verdi e, dove indicato, un benchmark.

---

## Fase 0 — Scaffolding
**Deliverable:** progetto compilabile e vuoto ma strutturato.
- `go mod init`, layout package come in `DESIGN.md §11`.
- Toolchain: `golangci-lint`, `go test`, target `Makefile`/`Taskfile`.
- CI minima (build + test + lint).
- `cmd/grafo` con un main che apre/chiude un DB vuoto.

**Fatto quando:** `go build ./...`, `go test ./...`, `golangci-lint run` passano in CI.

---

## Fase 1 — Storage layer + codec
**Deliverable:** persistenza KV transazionale con encoding corretto.
- Interfaccia `Store`/`Txn`/`Iterator` (`internal/storage`).
- Adapter Badger (`internal/storage/badger`).
- `internal/storage/codec`:
  - encoding chiavi (tag + ID big-endian, helper per ogni keyspace `n/e/o/i/l/p`).
  - encoding valori order-preserving (int/float/string con escape `0x00`).
  - encoding/decoding record nodo e arco.
- `internal/catalog`: dizionari name↔id, contatori ID, registry indici.

**Test:**
- Round-trip di ogni encoding.
- **Property test sull'ordinamento**: per valori generati casualmente,
  `bytes.Compare(enc(a), enc(b))` rispetta l'ordine logico di `a,b` (per tipo).
- Internamento dizionari idempotente e concorrente.

**Fatto quando:** i property test sull'ordine passano per int, float, string, e i
record fanno round-trip senza perdita.

---

## Fase 2 — Graph layer (CRUD + primitive di traversal)
**Deliverable:** API interna per manipolare il grafo, indici sempre coerenti.
- `internal/graph`: `CreateNode`, `CreateEdge`, `SetProperty`, `DeleteNode`,
  `DeleteEdge`, `GetNode`, `GetEdge`.
- Ogni mutazione aggiorna record + `l`/`p`/`o`/`i` nella **stessa** `Update`.
- Primitive di traversal: `OutEdges(nodeID, typeID)`, `InEdges(...)`,
  `NodesByLabel(labelID)`, `NodesByProperty(labelID, keyID, value)`.

**Test:**
- Creazione nodo con label → compare in `NodesByLabel`.
- Creazione arco → compare sia in `OutEdges(src)` sia in `InEdges(dst)`.
- Cancellazione nodo/arco → spariscono record **e** tutte le entry indice
  (verifica esplicita dell'Invariante #1).

**Fatto quando:** un test costruisce un piccolo grafo e tutte le primitive di
lettura restituiscono risultati coerenti dopo create/update/delete.

---

## Fase 3 — Parser → AST
**Deliverable:** dal testo Cypher all'AST per lo slice MVP.
- `internal/cypher/ast`: tipi dell'AST (query, clausole, pattern, espressioni).
- `internal/cypher/parser`: scelta tra
  - **ANTLR-gen** dalla grammatica ufficiale openCypher (parser completo "gratis",
    poi visitor → AST custom), oppure
  - **a mano** (recursive descent + Pratt per le espressioni) limitato allo slice.
  - *Decisione consigliata:* iniziare ANTLR-gen per coprire la grammatica, oppure
    a mano se si vuole AST pulito e zero dipendenze fin da subito. Annotare la
    scelta in un breve ADR.

**Test:** parse di un corpus di query MVP valide → AST atteso; query invalide →
errori con posizione.

**Fatto quando:** tutte le query del corpus MVP producono l'AST corretto.

---

## Fase 4 — Analisi semantica
**Deliverable:** AST risolto e validato.
- `internal/cypher/sema`: risoluzione scope variabili (incluso reset su `WITH`),
  binding di label/tipi/proprietà agli ID di dizionario, type-check di base.
- Errori semantici chiari (variabile non definita, ecc.).

**Test:** scoping corretto attraverso `WITH`; errori su variabili non legate.

**Fatto quando:** lo scoping `WITH` è corretto e i binding agli ID interni sono
risolti.

---

## Fase 5 — Piano logico + planner a regole
**Deliverable:** dall'AST risolto a un piano fisico eseguibile.
- `internal/cypher/plan`: operatori logici, traduzione pattern→piano,
  scelta anchor per selettività, push-down filtri, binding agli access method.

**Test:** per query note, il piano sceglie l'anchor atteso (es. usa l'indice `p`
quando c'è equality su proprietà indicizzata anziché un label scan).

**Fatto quando:** le query MVP producono piani fisici sensati e ispezionabili
(utile un `EXPLAIN` testuale).

---

## Fase 6 — Executor (read path)
**Deliverable:** **vertical slice** — query di lettura che gira end-to-end.
- `internal/cypher/exec`: operatori iterator (`NodeByLabelScan`,
  `NodeByProperty`, `NodeById`, `Expand`, `Filter`, `Project`, `Limit`).
- Collegamento `Query` pubblico → parser → sema → plan → exec → risultati.

**Test:** su un grafo seed, `MATCH (p:Person)-[:KNOWS]->(f) WHERE p.email=$e
RETURN f.name` restituisce i risultati corretti.

**Fatto quando:** la prima query end-to-end con un `Expand` restituisce risultati
corretti dal disco.

---

## Fase 7 — Write path (Cypher)
**Deliverable:** mutazioni via Cypher.
- `CREATE`, `SET`, `DELETE`, `DETACH DELETE`, `MERGE`.
- Decisione `Query` unico vs `Query`/`Execute` separati.
- `MERGE` con semantica match-or-create corretta.

**Test:** create+match nello stesso flusso; `MERGE` non duplica; `DETACH DELETE`
rimuove nodo e archi incidenti con indici coerenti.

**Fatto quando:** si può popolare e modificare il grafo interamente in Cypher.

---

## Fase 8 — Proiezione avanzata e traversal
**Deliverable:** copertura delle query analitiche leggere dello slice MVP.
- `ORDER BY`, `SKIP`, `LIMIT`, `DISTINCT`.
- `WITH` chaining completo.
- Aggregazioni (`count`/`collect`/`sum`/`avg`/`min`/`max`) con grouping implicito.
- `VarLengthExpand` `*lo..hi` con tracciamento ID relazione (no-repeat).

**Test:** aggregazioni con grouping; path a lunghezza variabile su grafo con cicli
(verifica no-repeated-relationship).

**Fatto quando:** lo slice MVP di `DESIGN.md §8` è coperto e testato.

---

## Fase 9 — Indici e integrazione
**Deliverable:** indici gestiti via Cypher e usati dal planner.
- `CREATE INDEX` su `(:Label).prop`; backfill degli esistenti.
- Il planner sceglie l'indice quando disponibile.
- CLI/REPL in `cmd/grafo` per uso interattivo.

**Test:** dopo `CREATE INDEX`, una query con equality usa l'indice (verificabile
via `EXPLAIN`) e i risultati restano identici.

**Fatto quando:** creare un indice cambia il piano e velocizza la query a parità
di risultati.

---

## Fase 10 — Hardening
**Deliverable:** robustezza e fiducia.
- Property/fuzz test sul parser e sul codec.
- Sottoinsieme dei test TCK openCypher (i `.feature` Cucumber pertinenti).
- Benchmark: insert throughput, latenza traversal 1–3 hop, query con indice.
- Crash-recovery test (riapertura dopo kill durante scrittura).
- Documentazione API pubblica + esempi.

**Fatto quando:** il sottoinsieme TCK scelto è verde, i benchmark sono tracciati e
la riapertura dopo crash è consistente.

---

## Ordine di attacco consigliato per CC
1. Fasi 0–2 in sequenza stretta (fondazione; non saltare i property test del codec).
2. Fasi 3→6 puntando alla **prima query end-to-end** (vertical slice) il prima
   possibile, anche con copertura Cypher minima.
3. Da lì allargare (7→9) una clausola alla volta, sempre con test.
4. Fase 10 in continuo, non solo alla fine.
