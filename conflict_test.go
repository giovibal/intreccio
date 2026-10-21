package intreccio

import (
	"context"
	"sync"
	"testing"
)

// TestConcurrentWritesToOneNode covers #30: many writers updating the same
// node all succeed (conflicts are retried with backoff) and no update is lost.
// It runs on disk with fsync: slower commits make conflicts frequent enough
// that tight retries used to give up.
func TestConcurrentWritesToOneNode(t *testing.T) {
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
	const writers, per = 16, 100
	var wg sync.WaitGroup
	errs := make(chan error, writers*per)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				if _, err := db.Query(ctx, "MATCH (c:Counter) SET c.v = c.v + 1", nil); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("write failed: %v", err)
	}
	res, err := db.Query(ctx, "MATCH (c:Counter) RETURN c.v", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(writers*per) {
		t.Errorf("counter = %v, want %d (lost updates)", got, writers*per)
	}
}
