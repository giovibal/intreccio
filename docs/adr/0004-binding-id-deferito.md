# ADR 0004 — Binding nome→ID deferito a plan/exec

Stato: accettato — 2026-05-25

## Contesto
La Fase 4 (`PLAN.md`) prevede, oltre allo scoping e alle validazioni, il "binding
di label/tipi/proprietà agli ID di dizionario". DESIGN §2 colloca questo binding
nell'analisi semantica. Però:
- la risoluzione nome→ID passa dal `catalog`, che richiede una `storage.Txn` viva;
- nel **read path** una label/tipo/chiave **inesistente non è un errore**: la query
  matcha semplicemente zero righe. Un binding "rigido" in sema fallirebbe a torto;
- nel **write path** (`CREATE`/`MERGE`/`SET`) i nomi vanno **internati** al momento
  dell'esecuzione, non risolti in anticipo.

## Decisione
La Fase 4 (`internal/cypher/sema`) resta **pura, senza dipendenze dallo storage**:
fa scoping (con reset su WITH), validazione dei riferimenti a variabile, controlli
di base (alias obbligatori in WITH, RETURN finale, niente aggregazioni annidate,
CREATE INDEX da solo) e calcola le **colonne di output**.

Il **binding nome→ID** è spostato in plan/exec (Fase 5/6), dove la query gira
dentro una transazione: il read path userà `catalog.Lookup*` (assente ⇒ risultato
vuoto), il write path userà `catalog.Intern*`.

## Conseguenze
- `sema.Analyze(*ast.Query) (*Result, error)` è testabile senza aprire un DB.
- La pipeline pubblica (`Query`) aprirà una `View`/`Update`, e dentro eseguirà
  parse → sema → plan(con risoluzione ID via txn) → exec.
- È una deviazione dalla lettera di `PLAN.md` (binding in sema) ma ne rispetta lo
  scopo: lo scoping è corretto e la risoluzione agli ID avviene, solo nello strato
  dove la transazione esiste ed è semanticamente corretta.
