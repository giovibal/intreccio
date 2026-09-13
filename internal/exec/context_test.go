package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/query/parser"
	"github.com/giovibal/intreccio/query/plan"
	"github.com/giovibal/intreccio/query/sema"
)

// cancelOnGet cancels a context the first time a key is read, so a query that is
// already under way is interrupted between operator iterations (rather than at
// the entry check).
type cancelOnGet struct {
	storage.Txn
	cancel context.CancelFunc
	once   bool
}

func (c *cancelOnGet) Get(key []byte) ([]byte, error) {
	if !c.once {
		c.once = true
		c.cancel()
	}
	return c.Txn.Get(key)
}

// TestRunStopsWhenCanceledMidQuery verifies that operator loops poll the context
// and abort once it is canceled, not only at the start.
func TestRunStopsWhenCanceledMidQuery(t *testing.T) {
	s := newStore(t)
	seedSocial(t, s)

	q, err := parser.Parse("MATCH (n:Person) RETURN n.name AS name")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		t.Fatalf("sema: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runErr error
	err = s.View(func(txn storage.Txn) error {
		p, err := plan.Plan(q, testCatalog{txn: txn})
		if err != nil {
			return err
		}
		wrapped := &cancelOnGet{Txn: txn, cancel: cancel}
		_, runErr = Run(p, semaRes.Columns, &Context{Txn: wrapped, Ctx: ctx})
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("runErr = %v, want context.Canceled", runErr)
	}
}
