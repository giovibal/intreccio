package exec

import (
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/query/parser"
	"github.com/giovibal/intreccio/query/plan"
	"github.com/giovibal/intreccio/query/sema"
)

// countingTxn counts record lookups and iterator steps, to observe how much of
// the store a query actually reads.
type countingTxn struct {
	storage.Txn
	reads int
}

func (c *countingTxn) Get(key []byte) ([]byte, error) {
	c.reads++
	return c.Txn.Get(key)
}

func (c *countingTxn) Scan(prefix []byte) storage.Iterator {
	return &countingIter{Iterator: c.Txn.Scan(prefix), txn: c}
}

type countingIter struct {
	storage.Iterator
	txn *countingTxn
}

func (it *countingIter) Next() {
	it.txn.reads++
	it.Iterator.Next()
}

func readsFor(t *testing.T, s storage.Store, cypher string) (int, int) {
	t.Helper()
	q, err := parser.Parse(cypher)
	if err != nil {
		t.Fatal(err)
	}
	sr, err := sema.Analyze(q)
	if err != nil {
		t.Fatal(err)
	}
	var reads, rows int
	err = s.View(func(txn storage.Txn) error {
		ct := &countingTxn{Txn: txn}
		p, err := plan.Plan(q, testCatalog{txn: txn})
		if err != nil {
			return err
		}
		out, err := Run(p, sr.Columns, &Context{Txn: ct})
		reads, rows = ct.reads, len(out)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return reads, rows
}

// TestScansStopAtLimit covers #25: a LIMIT stops the scan instead of reading
// every node first.
func TestScansStopAtLimit(t *testing.T) {
	s := newStore(t)
	runQuery(t, s, "UNWIND $ids AS i CREATE (:L {i: i})", map[string]any{"ids": intsAsAny(1000)})

	for _, q := range []string{
		"MATCH (n:L) RETURN n.i LIMIT 3",
		"MATCH (n) RETURN n.i LIMIT 3",
		"MATCH (n:L) WHERE n.i >= 0 RETURN n.i LIMIT 3",
		"UNWIND [] AS x MATCH (n:L) RETURN n.i",
	} {
		reads, _ := readsFor(t, s, q)
		if reads > 50 {
			t.Errorf("%s: read %d records/keys, want a handful (the scan did not stop)", q, reads)
		}
	}
	if _, rows := readsFor(t, s, "MATCH (n:L) RETURN n.i LIMIT 3"); rows != 3 {
		t.Errorf("LIMIT 3 returned %d rows", rows)
	}
}

func intsAsAny(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = int64(i)
	}
	return out
}

// TestRangeScanReadsOnlyTheRange covers #26: with an index, a range predicate
// reads the matching index entries, not the whole label.
func TestRangeScanReadsOnlyTheRange(t *testing.T) {
	s := newStore(t)
	runQuery(t, s, "CREATE INDEX FOR (n:L) ON (n.i)", nil)
	runQuery(t, s, "UNWIND $ids AS i CREATE (:L {i: i, s: toString(i)})", map[string]any{"ids": intsAsAny(1000)})
	runQuery(t, s, "CREATE INDEX FOR (n:L) ON (n.s)", nil)

	for _, c := range []struct {
		query    string
		rows     int
		maxReads int
	}{
		{"MATCH (n:L) WHERE n.i > 995 RETURN n.i", 4, 30},
		{"MATCH (n:L) WHERE n.i >= 10 AND n.i < 13 RETURN n.i", 3, 30},
		{"MATCH (n:L) WHERE n.i >= 0 RETURN n.i LIMIT 3", 3, 30},
		{"MATCH (n:L) WHERE n.s STARTS WITH '99' RETURN n.s", 11, 60},
	} {
		reads, rows := readsFor(t, s, c.query)
		if rows != c.rows {
			t.Errorf("%s: %d rows, want %d", c.query, rows, c.rows)
		}
		if reads > c.maxReads {
			t.Errorf("%s: read %d records/keys, want at most %d", c.query, reads, c.maxReads)
		}
	}
}
