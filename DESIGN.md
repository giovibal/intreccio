# DESIGN — Graph DB embedded in puro Go (`mycypher`)

> Documento di design di alto livello. È la fonte di verità dell'architettura.
> `PLAN.md` traduce questo design in fasi di sviluppo; `CLAUDE.md` ne estrae le
> convenzioni e gli invarianti operativi per l'agente.

## 1. Obiettivo e scope

Un database a grafo **embedded**, **single-binary**, in **puro Go**, che parla un
sottoinsieme di **openCypher 9**. Pensato per carichi **OLTP / knowledge-graph**:
lookup puntuali e traversal a poche hop su grafi di dimensione media.

### Goals
- Puro Go, **niente cgo**, deploy come singolo binario.
- Property graph (nodi con label e proprietà, archi tipizzati con proprietà).
- Sottoinsieme utile e solido di openCypher 9 (vedi §8).
- Transazioni ACID con indici sempre coerenti.
- API embeddable pulita + una CLI/REPL.

### Non-goals (espliciti)
- **Niente OLAP**: nessun processore vettorizzato/fattorizzato, nessun join
  analitico massivo. Quello era il valore di Kùzu ed è la parte che non ha senso
  reimplementare in Go.
- Niente conformità TCK completa a openCypher 9 in v1 (vedi §8).
- Niente distribuzione/replica in v1 (vedi §11, evoluzione futura via NATS).
- Niente estensioni post-9 / GQL (`CALL { }`, `EXISTS { }`, quantified path
  patterns).

## 2. Architettura a livelli

Pipeline di query dall'alto verso il basso, tutto in-process:

```
Query Cypher (testo)
   → Parser            → AST
   → Analisi semantica  (scoping, binding label/tipi/proprietà a ID interni)
   → Piano logico       (albero di operatori logici)
   → Planner a regole   (scelta anchor, push-down dei filtri)
   → Piano fisico       (operatori legati ad access method concreti)
   → Executor           (modello iterator / Volcano: Next())
   → Graph storage API  (CRUD transazionale di nodi/archi/indici)
   → Key encoding       (tabelle prefissate su KV ordinato)
   → Storage engine     (Badger; puro Go; ACID SSI)
```

## 3. Decisione: storage engine

**Default: BadgerDB.** LSM puro Go, scritture veloci, transazioni ACID con
serializable snapshot isolation, iterator con prefix scan ordinati — esattamente
ciò che serve per l'adiacenza.

**Alternativa: bbolt.** B+tree mmap, letture eccellenti, range scan ordinati
nativi, massima stabilità (storage di etcd). Limite: single-writer (una sola
transazione in scrittura per volta), accettabile per carichi read-heavy.

**Astrazione.** Lo strato superiore non deve dipendere dall'engine concreto. Si
definisce un'interfaccia `Store` minimale (Get/Set/Delete/Iterator a prefisso +
transazioni read/write) e si fornisce un adapter Badger (default) e, se serve, un
adapter bbolt. Questo permette di cambiare engine senza toccare il graph layer.

```go
type Store interface {
    View(func(Txn) error) error          // read-only
    Update(func(Txn) error) error        // read-write, atomica
    Close() error
}
type Txn interface {
    Get(key []byte) ([]byte, error)
    Set(key, val []byte) error
    Delete(key []byte) error
    Scan(prefix []byte) Iterator         // ordine lessicografico
}
```

## 4. Modello dati

Property graph:
- **Nodo**: ID interno `uint64`, insieme di **label**, mappa di **proprietà**.
- **Arco** (relazione): ID interno `uint64`, **tipo** singolo, nodo sorgente e
  destinazione, mappa di **proprietà**, direzione.
- **Valori di proprietà**: null, bool, int64, float64, string, list, map
  (le ultime due solo come valore serializzato, non indicizzabili in v1).

Label, tipi di relazione e chiavi di proprietà sono **stringhe internate** in ID
interi (`uint32`) tramite dizionari (§6), così da entrare a larghezza fissa nelle
chiavi.

## 5. Key encoding — il cuore del design

Principio: su un KV ordinato lessicograficamente, scegliere encoding in cui
**l'ordine dei byte coincide con l'ordine logico desiderato**, e **raggruppare per
prefisso** così che le query calde siano un singolo range scan.

Convenzioni:
- ID interni nodo/arco: `uint64` **big-endian** (8 byte) → ordine byte = ordine
  numerico.
- ID di dizionario (label/tipo/propKey): `uint32` big-endian (4 byte).
- Primo byte = **tag della tabella**.

| Tag | Chiave | Valore | Scopo |
|-----|--------|--------|-------|
| `n` | `n` + nodeID(8) | record nodo (label + proprietà) | record nodo |
| `e` | `e` + edgeID(8) | typeID, srcID, dstID, proprietà | record arco |
| `o` | `o` + srcID(8) + typeID(4) + dstID(8) + edgeID(8) | ∅ | **archi uscenti** (hot path) |
| `i` | `i` + dstID(8) + typeID(4) + srcID(8) + edgeID(8) | ∅ | archi entranti |
| `l` | `l` + labelID(4) + nodeID(8) | ∅ | indice per label |
| `p` | `p` + labelID(4) + propKeyID(4) + valEnc + nodeID(8) | ∅ | indice secondario su proprietà |

### Adiacenza (le righe `o` e `i`)
Ogni arco è scritto **due volte**: una in `o` (per i traversal uscenti) e una in
`i` (per gli entranti). "Tutti gli archi uscenti di X di tipo KNOWS" diventa un
prefix scan su `o` + X.id + KNOWS.typeID, in tempo proporzionale al fan-out e non
alla dimensione del grafo. `edgeID` come ultimo componente garantisce chiavi
uniche anche nei multigrafi (più archi dello stesso tipo tra la stessa coppia).

### Record (le righe `n` ed `e`)
Il valore è una serializzazione compatta (es. un encoding binario custom o un
formato come MessagePack/CBOR — da decidere in Fase 1; preferire qualcosa
zero-alloc in lettura). I record contengono i dati "pesanti"; gli indici
contengono solo le chiavi necessarie a trovarli.

### Encoding order-preserving dei valori (`valEnc`)
È l'unico encoding davvero delicato. Serve se si vogliono range query
(`age > 30`) e non solo equality. Schema:
- 1 byte di **tag di tipo**, in bande ordinate: `NULL(0x00) < BOOL(0x01) <
  INT(0x02) < FLOAT(0x03) < STRING(0x04)`.
- **int64**: `binary.BigEndian(uint64(v) ^ (1<<63))` — il flip del bit di segno
  rende il complemento a due ordinabile come unsigned.
- **float64**: `bits = math.Float64bits(v)`; se segno negativo `bits = ^bits`,
  altrimenti `bits |= 1<<63`; poi big-endian.
- **string**: UTF-8 grezzo. Poiché nella chiave `p` la stringa è **seguita** dal
  nodeID, serve un confine non ambiguo che preservi l'ordine: escape `0x00` →
  `0x00 0xFF`, terminatore `0x00 0x00` (encoding "ordered bytes" stile
  CockroachDB/FoundationDB). **Non** usare length-prefix: rompe l'ordine
  lessicografico.

Per la sola equality (lookup puntuale, es. trova nodo per `email`) si può anche
hashare il valore: più semplice, ma niente range. Iniziare con order-preserving
solo dove serve.

## 6. Catalog, dizionari, ID allocation
- **Dizionari** name↔id per label, tipi, chiavi-proprietà (keyspace dedicati,
  es. `L`/`T`/`K` per name→id e i reverse). Internamento idempotente in
  transazione.
- **Contatori** per l'allocazione di nodeID/edgeID monotòni (`uint64`), in un
  keyspace `c`.
- **Registry degli indici**: quali `(label, propKey)` hanno un indice `p` attivo,
  così il write path sa quali indici mantenere.

## 7. Transazioni e invarianti di consistenza

> **Invariante #1 (non negoziabile):** ogni scrittura aggiorna il record base
> **e tutte** le sue chiavi-indice derivate (`l`, `p`, `o`, `i`) **dentro la
> stessa transazione**. Se record e indici non commitano atomicamente, gli indici
> divergono e le query mentono.

- Le scritture girano in `Store.Update` (transazione atomica).
- Le letture girano in `Store.View` (snapshot coerente).
- Su Badger la SSI fornisce isolamento serializzabile; su bbolt il single-writer
  serializza naturalmente le scritture.

## 8. Sottoinsieme openCypher (scope MVP)

Incluso in v1:
- DML: `CREATE`, `MERGE`, `SET`, `DELETE`, `DETACH DELETE`.
- `MATCH` con pattern di nodi/archi, direzioni, label e tipi.
- `WHERE`: confronti, `AND`/`OR`/`NOT`, accesso a proprietà, predicati di label.
- `RETURN` con proiezione, alias, `DISTINCT`.
- `ORDER BY`, `SKIP`, `LIMIT`.
- `WITH` per il chaining e il reset dello scope.
- Path a lunghezza variabile **limitata**: `-[:T*1..3]->`.
- Aggregazioni: `count`, `collect`, `sum`, `avg`, `min`, `max` con
  raggruppamento implicito (chiavi = item di proiezione non aggregati).
- `CREATE INDEX` su `(:Label).prop`.
- Parametri: `$param`.

Rimandato (post-v1, in ordine di probabile priorità):
- Libreria di funzioni completa (aggiunta incrementale).
- `shortestPath` / `allShortestPaths`.
- Subquery: `EXISTS { }`, `CALL { }`.
- `CALL` su procedure / funzioni definite.
- Conformità TCK completa.

### Semantica da non sbagliare
- **No relazioni ripetute** nello stesso path di un `MATCH` (semantica "trail" di
  openCypher 9): durante l'espansione di path a lunghezza variabile si tracciano
  gli **ID di relazione** già attraversati, non i nodi.
- `MERGE` = match-or-create: prima tenta il match completo del pattern, e solo se
  fallisce crea; attenzione alle race con la transazione.
- `WITH` introduce un nuovo scope: le variabili non riproiettate non sono visibili
  a valle.
- `OPTIONAL MATCH` produce `null` sui binding non trovati.

## 9. Pipeline di query — operatori e mappatura sui keyspace

Modello **iterator (Volcano)**: ogni operatore espone `Next() (Record, bool)`, il
root tira. Niente vettorizzazione: per poche hop il pull model è adeguato.

Operatori e access method:
- `NodeByLabelScan(:L)` → prefix scan su `l` + labelID.
- `NodeByProperty(:L, k=v)` → prefix scan su `p` + labelID + keyID + valEnc.
- `NodeById(id)` → get diretto su `n` + id.
- `AllNodesScan` → prefix scan su `n` (fallback, evitare se possibile).
- `Expand(a)-[:T]->(b)` → per ogni `a`, prefix scan su `o` + a.id + typeID →
  produce i `b`; fetch `n` + b.id solo se servono proprietà.
- `Expand` inverso → stesso meccanismo su `i`.
- `VarLengthExpand(*lo..hi)` → BFS/DFS limitato in profondità su `o`/`i`,
  tracciando gli ID di relazione (vedi §8).
- `Filter`, `Project`, `OrderBy`, `Skip`, `Limit`, `Aggregate` → operatori
  in-memory sopra lo stream.

### Planner a regole (v1)
Sufficiente per essere usabile. Euristica:
1. **Scelta dell'anchor** per selettività decrescente:
   `NodeById` > equality su proprietà indicizzata (`p`) > `NodeByLabelScan` (`l`)
   > `AllNodesScan`.
2. **Espansione** verso l'esterno seguendo gli archi a partire dall'anchor più
   economico.
3. **Push-down** dei predicati `WHERE` sugli scan il più in basso possibile.

Il cost-based con statistiche (cardinalità, istogrammi) è **fase 2**, non serve
per la v1.

## 10. API embeddable (bozza)

```go
db, err := mycypher.Open("data/")   // apre/crea il database
defer db.Close()

res, err := db.Query(ctx, `
    MATCH (p:Person {email: $email})-[:KNOWS]->(f)
    RETURN f.name AS name
    ORDER BY name LIMIT 10
`, map[string]any{"email": "a@b.com"})

for res.Next() {
    rec := res.Record()
    // rec.Get("name")
}
```

Read e write possono condividere `Query` (il planner sa se il piano scrive) oppure
restare separati (`Query` read-only, `Execute` write). Decisione in Fase 7.

## 11. Layout del progetto (Go)

```
mycypher/
  cmd/mycypher/         # entrypoint CLI/REPL (single binary)
  internal/
    storage/            # interfaccia Store + adapter engine
      codec/            # key & value encoding (order-preserving)
      badger/           # adapter BadgerDB (default)
      bolt/             # adapter bbolt (opzionale)
    catalog/            # dizionari, contatori, registry indici
    graph/              # modello + CRUD transazionale + primitive di traversal
    cypher/
      ast/              # tipi dell'AST
      parser/           # parser (ANTLR-gen o a mano)
      sema/             # analisi semantica / binding
      plan/             # piano logico+fisico, planner a regole
      exec/             # operatori executor (Volcano)
  mycypher.go           # API pubblica embeddable (package mycypher)
  CLAUDE.md DESIGN.md PLAN.md
  go.mod
```

> `internal/` per ciò che non è API pubblica; l'API embeddable vive nel package
> radice `mycypher`. Nome modulo: `github.com/giovibal/mycypher`.

## 12. Evoluzione futura (fuori v1)
- **Replica / distribuzione**: NATS JetStream come write-ahead/replication log o
  changefeed event-sourced sopra l'engine — *non* come storage engine. Tenere
  fuori dalla v1 single-binary.
- Planner cost-based con statistiche.
- Indici full-text e vettoriali (per GraphRAG), se il caso d'uso lo richiede.
- Estensione verso GQL / costrutti post-9.

## 13. Rischi principali
- **Planner**: è dove vive ~70% dell'ingegneria. Mitigazione: vertical slice
  precoce (un solo pattern end-to-end) prima di allargare la copertura Cypher.
- **Encoding order-preserving delle stringhe**: facile sbagliare il confine.
  Mitigazione: property-test dedicati sul codec (round-trip + ordinamento).
- **Coerenza indici**: vedi Invariante #1. Mitigazione: tutte le mutazioni
  passano da un unico punto del graph layer che aggiorna record + indici insieme.
- **Scope creep su Cypher**: tentazione di implementare tutto. Mitigazione:
  attenersi allo slice MVP di §8.
