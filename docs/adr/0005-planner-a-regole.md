# ADR 0005 — Planner a regole (Fase 5)

Stato: accettato — 2026-05-25

## Contesto
Fase 5 (`PLAN.md`): dall'AST risolto a un piano fisico eseguibile. In v1 il
planner è **a regole**, senza cost-based né statistiche (DESIGN §9): le regole
producono direttamente operatori legati agli access method.

## Decisioni

### Operatori (op.go)
Foglie: `AllNodesScan`, `NodeByLabelScan`, `NodeByProperty`. Interni: `Expand`,
`Filter`, `Project`, `Aggregate`, `Sort`, `Skip`, `Limit`, `CartesianProduct`.
Piano logico e fisico coincidono (niente fase logica separata in v1).

### Scelta dell'anchor (selettività decrescente)
Per ogni nodo non ancora legato: `NodeByProperty` (label + equality su proprietà
**indicizzata**) > `NodeByLabelScan` (label) > `AllNodesScan`. Le equality sono
estratte dalle proprietà inline e dai congiunti del WHERE (`var.prop = costante`).
L'informazione sugli indici arriva da un'interfaccia `Catalog.HasIndex(label,key)`
(in produzione backed dalla txn — ADR 0004; nei test un fake).

### Espansione
Dall'anchor si espande verso destra e verso sinistra lungo la catena del pattern;
attraversando una relazione "all'indietro" si **inverte la direzione**
(`flip`: out↔in, both invariato). Le relazioni e i nodi anonimi ricevono nomi
sintetici (`_n0`, `_r0`).

### Push-down dei filtri
La equality consumata dall'anchor non viene riapplicata. Tutto il resto diventa un
`Filter` sopra scan/expand: congiunti WHERE non consumati, proprietà inline non
consumate, proprietà delle relazioni e **label dei nodi raggiunti via Expand**
(l'access method garantisce solo la label del nodo scansionato).

### Proiezione
`RETURN`/`WITH` → `Project` (o `Aggregate` se compaiono funzioni di aggregazione;
group key = item non aggregati). Segue la coda `Sort`/`Skip`/`Limit`. `WITH`
resetta lo scope alle colonne proiettate; `WITH ... WHERE` filtra dopo la
proiezione.

### EXPLAIN
`Explain(Op)` rende il piano come albero testuale indentato (radice in alto),
ispezionabile e usato dai test.

## Limiti noti (accettati per la v1)
- Scrittura (`CREATE`/`MERGE`/`SET`/`DELETE`) e `CREATE INDEX` non ancora
  pianificati (Fase 7/9): il planner restituisce un errore esplicito.
- L'esecuzione di `Aggregate` e `VarLengthExpand` è completata in Fase 6/8; qui il
  planner li produce per renderli ispezionabili.
- Join tra pattern non connessi via `CartesianProduct` (niente riordino dei join).
