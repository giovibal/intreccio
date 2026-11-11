package intreccio

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func countPeople(t *testing.T, db *DB) int64 {
	t.Helper()
	res, err := db.Query(context.Background(), "MATCH (p:Person) RETURN count(p)", nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Rows[0][0].(int64)
}

// TestUpdateRollsBackOnError covers #36: when fn fails, none of the queries
// it ran are committed.
func TestUpdateRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	boom := errors.New("boom")
	err = db.Update(ctx, func(tx *Tx) error {
		if _, err := tx.Query(ctx, "CREATE (:Person {name: 'a'})", nil); err != nil {
			return err
		}
		if _, err := tx.Query(ctx, "CREATE (:Person {name: 'b'})", nil); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update error = %v, want boom", err)
	}
	if n := countPeople(t, db); n != 0 {
		t.Fatalf("people = %d after rollback, want 0", n)
	}
}

// TestUpdateReadYourWritesAndTransfer: later queries see earlier writes, and
// a read-check-write sequence commits atomically.
func TestUpdateReadYourWritesAndTransfer(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Query(ctx, "CREATE (:Account {id: 1, bal: 100}), (:Account {id: 2, bal: 0})", nil); err != nil {
		t.Fatal(err)
	}
	errInsufficient := errors.New("insufficient funds")
	transfer := func(amount int64) error {
		return db.Update(ctx, func(tx *Tx) error {
			if _, err := tx.Query(ctx, "MATCH (a:Account {id: 1}) SET a.bal = a.bal - $x", map[string]any{"x": amount}); err != nil {
				return err
			}
			res, err := tx.Query(ctx, "MATCH (a:Account {id: 1}) RETURN a.bal", nil)
			if err != nil {
				return err
			}
			if res.Rows[0][0].(int64) < 0 {
				return errInsufficient
			}
			_, err = tx.Query(ctx, "MATCH (b:Account {id: 2}) SET b.bal = b.bal + $x", map[string]any{"x": amount})
			return err
		})
	}
	if err := transfer(70); err != nil {
		t.Fatal(err)
	}
	if err := transfer(70); !errors.Is(err, errInsufficient) {
		t.Fatalf("second transfer = %v, want insufficient funds", err)
	}
	res, err := db.Query(ctx, "MATCH (a:Account) RETURN a.id, a.bal ORDER BY a.id", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][1] != int64(30) || res.Rows[1][1] != int64(70) {
		t.Fatalf("balances = %v, want 30 and 70", res.Rows)
	}
}

func TestViewIsReadOnly(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Query(ctx, "CREATE (:Person {name: 'a'})", nil); err != nil {
		t.Fatal(err)
	}
	err = db.View(ctx, func(tx *Tx) error {
		res, err := tx.Query(ctx, "MATCH (p:Person) RETURN p.name", nil)
		if err != nil {
			return err
		}
		if len(res.Rows) != 1 {
			t.Errorf("rows = %v, want one", res.Rows)
		}
		_, err = tx.Query(ctx, "CREATE (:Person {name: 'b'})", nil)
		return err
	})
	if !errors.Is(err, ErrReadOnlyTx) {
		t.Fatalf("write in View = %v, want ErrReadOnlyTx", err)
	}
	if n := countPeople(t, db); n != 1 {
		t.Fatalf("people = %d, want 1", n)
	}
}

func TestTxUnusableAfterReturn(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var kept *Tx
	if err := db.View(ctx, func(tx *Tx) error { kept = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := kept.Query(ctx, "RETURN 1", nil); !errors.Is(err, ErrTxDone) {
		t.Fatalf("query after View = %v, want ErrTxDone", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error { kept = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := kept.Query(ctx, "RETURN 1", nil); !errors.Is(err, ErrTxDone) {
		t.Fatalf("query after Update = %v, want ErrTxDone", err)
	}
}

// TestUpdateConcurrentReadModifyWrite: the value is read into Go and written
// back in a later query. Conflicts retry fn on fresh data, so no increment is
// lost.
func TestUpdateConcurrentReadModifyWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Query(ctx, "CREATE (:Counter {v: 0})", nil); err != nil {
		t.Fatal(err)
	}
	const writers, per = 8, 20
	var wg sync.WaitGroup
	errs := make(chan error, writers*per)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				err := db.Update(ctx, func(tx *Tx) error {
					res, err := tx.Query(ctx, "MATCH (c:Counter) RETURN c.v", nil)
					if err != nil {
						return err
					}
					next := res.Rows[0][0].(int64) + 1
					_, err = tx.Query(ctx, "MATCH (c:Counter) SET c.v = $v", map[string]any{"v": next})
					return err
				})
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("update failed: %v", err)
	}
	res, err := db.Query(ctx, "MATCH (c:Counter) RETURN c.v", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(writers*per) {
		t.Errorf("counter = %v, want %d (lost updates)", got, writers*per)
	}
}
