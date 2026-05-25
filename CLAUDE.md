# CLAUDE.md

Contesto di progetto per Claude Code. Leggere `DESIGN.md` per l'architettura e
`PLAN.md` per le fasi. Questo file contiene le **regole operative** e gli
**invarianti** da non violare mai.

## Cos'è
`mycypher` — graph DB **embedded**, **single-binary**, in **puro Go**, che parla un
sottoinsieme di **openCypher 9**. Carico target: **OLTP / knowledge-graph**
(lookup e traversal a poche hop su grafi medi). Non è un motore analitico OLAP.

## Vincoli rigidi (non negoziabili)
- **Puro Go. Niente cgo.** Nessuna dipendenza che richieda un toolchain C.
  Se una libreria utile richiede cgo, va scartata o sostituita.
- **Single binary**: deve compilare in un eseguibile autosufficiente.
- **Storage engine**: BadgerDB di default, dietro l'interfaccia `Store`. bbolt
  come adapter alternativo. Lo strato superiore **non** dipende dall'engine
  concreto.
- Niente OLAP, niente vettorizzazione, niente distribuzione in v1.

## Invarianti di correttezza
1. **Coerenza indici**: ogni mutazione aggiorna il record base **e tutte** le sue
   chiavi-indice (`l`, `p`, `o`, `i`) **nella stessa transazione** (`Store.Update`).
   Tutte le scritture passano da un unico punto in `internal/graph`. Mai scrivere
   un record senza aggiornare i suoi indici nello stesso atto.
2. **Encoding ordinabile**: le chiavi usano ID big-endian; i valori in `valEnc`
   usano l'encoding order-preserving definito in `DESIGN.md §5`. Mai introdurre
   length-prefix sulle stringhe indicizzate (rompe l'ordine).
3. **No relazioni ripetute** nei path a lunghezza variabile (semantica trail di
   openCypher 9): tracciare gli **ID di relazione** attraversati, non i nodi.
4. **Adiacenza doppia**: ogni arco è scritto sia in `o` (uscenti) sia in `i`
   (entranti). Le due viste devono restare sincronizzate.

## Layout
```
cmd/mycypher/         entrypoint CLI/REPL
internal/storage/     interfaccia Store + codec + adapter (badger, bolt)
internal/catalog/     dizionari, contatori ID, registry indici
internal/graph/       modello + CRUD transazionale + primitive traversal
internal/cypher/      ast, parser, sema, plan, exec
mycypher.go           API pubblica embeddable (package mycypher)
```
API pubblica solo nel package radice; tutto il resto in `internal/`.

## Comandi
```bash
go build ./...                # build
go test ./...                 # test
go test -race ./...           # test con race detector (usare spesso)
go test -run TestCodec ./internal/storage/codec   # singolo pacchetto
golangci-lint run             # lint
go test -bench . ./...        # benchmark
```

## Convenzioni di codice
- Errori: wrapping con `fmt.Errorf("...: %w", err)`; errori sentinella per i casi
  gestibili (es. `ErrNotFound`). Niente `panic` nel percorso normale.
- `context.Context` come primo parametro nei metodi pubblici di query.
- Niente stato globale; il `*DB` incapsula lo `Store`.
- Test: table-driven dove sensato; **property test** per il codec (round-trip +
  ordinamento); usare `testing/quick` o `gopkg.in/check` solo puro Go.
- Niente dipendenze inutili: preferire stdlib. Ogni nuova dipendenza va
  giustificata e verificata che sia puro Go.
- Nomi e identificatori in inglese; commenti in italiano va bene.

## Come lavorare (per l'agente)
- Procedere per **fasi** come in `PLAN.md`; non saltare i test di fondazione del
  codec (Fase 1).
- Puntare presto a una **vertical slice** end-to-end (Fase 6) anche con copertura
  Cypher minima, poi allargare una clausola alla volta.
- Prima di estendere la copertura Cypher, controllare che lo slice in
  `DESIGN.md §8` non sia già sufficiente: **evitare scope creep**.
- Per ogni nuovo operatore o encoding: scrivere prima il test, poi
  l'implementazione.
- Quando una scelta architetturale non è ovvia (es. parser ANTLR-gen vs a mano,
  formato di serializzazione dei record), annotarla in un breve ADR in
  `docs/adr/` e procedere.

## Dipendenze previste (tutte puro Go)
- `github.com/dgraph-io/badger/v4` — storage engine default.
- `go.etcd.io/bbolt` — adapter alternativo (opzionale).
- Parser: runtime ANTLR Go (`github.com/antlr4-go/antlr/v4`) **se** si sceglie la
  via ANTLR-gen; altrimenti nessuna dipendenza (parser a mano).
- Serializzazione record: da decidere (stdlib `encoding/binary` custom, o un
  CBOR/MessagePack puro Go).

## Cosa NON fare
- Non introdurre cgo per nessun motivo.
- Non scrivere record senza aggiornarne gli indici nella stessa transazione.
- Non implementare costrutti fuori dallo slice MVP finché lo slice non è solido.
- Non aggiungere un processore vettorizzato/colonnare: fuori scope per design.
