# ADR 0002 — Formato record, encoding valori e keyspace catalog

Stato: accettato — 2026-05-25

## Contesto
Fase 1 (`PLAN.md`): storage layer + codec. Vanno fissati (a) il formato di
serializzazione dei record nodo/arco (lasciato aperto in `DESIGN.md §5`), (b) il
rapporto tra encoding order-preserving dei valori e serializzazione dei record,
(c) i keyspace del catalog, (d) la semantica di `Store.Update` sotto conflitto.

## Decisioni

### Due encoding di valore distinti
- **Index value (`valEnc`, order-preserving)** — solo scalari (null, bool, int64,
  float64, string), usato nelle chiavi `p`. Tag di tipo in bande ordinate, int con
  flip del bit di segno, float normalizzato, string "ordered bytes" (escape
  `0x00`→`0x00 0xFF`, terminatore `0x00 0x00`). È l'unico encoding in cui l'ordine
  dei byte deve coincidere con l'ordine logico.
- **Record value (length-prefixed)** — usato nei valori dei record `n`/`e`. Conta
  solo il round-trip, non l'ordine: formato self-describing con uvarint/varint e
  length-prefix; supporta anche `list` e `map` (non indicizzabili). Più semplice e
  veloce dell'order-preserving dove l'ordine non serve.

### Formato record: binario custom, stdlib-only
Scartati CBOR/MessagePack: la serializzazione richiesta è semplice e
`encoding/binary` (uvarint/varint) basta, evitando una dipendenza (CLAUDE.md:
"preferire stdlib"). Encoding deterministico (label e chiavi-proprietà ordinate).
- Nodo: `uvarint(nLabels) | labelID... | props`.
- Arco: `uvarint(typeID) | uvarint(src) | uvarint(dst) | props`.
- props: `uvarint(n) | (uvarint(keyID) | recValue)...`.

### Keyspace catalog
Tag disgiunti da quelli del grafo (`n/e/o/i/l/p`):
`c`+kind (contatori), `L`/`T`/`K`+name (dizionari name→id), `R`+kind+id (reverse),
`X`+label+propKey (registry indici). **Gli ID partono da 1**; 0 è riservato come
"nessuno".

### Store.Update ritenta sui conflitti
Su Badger (SSI) due `Update` concorrenti che toccano le stesse chiavi generano
`ErrConflict`. L'adapter ritenta `fn` (cap a 100) finché non commita. Conseguenza:
`fn` deve essere priva di effetti collaterali esterni alla transazione. Questo
rende corretto e semplice l'internamento concorrente dei dizionari senza logica di
retry nel catalog.

## Conseguenze
- Il graph layer (Fase 2) userà `codec` per chiavi/record e `catalog` per gli ID,
  senza conoscere l'engine.
- L'aggiunta di `list`/`map` indicizzabili è fuori scope (v1): restano solo nei
  record.
