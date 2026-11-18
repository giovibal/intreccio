package intreccio

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentWritesToOneNode covers #30: many writers updating the same
// node lose no update. It runs on disk with fsync, where slower commits make
// conflicts frequent.
//
// A write may still fail with ErrConflict once its bounded retries run out:
// the API allows it, and on a slow disk it happens (#91). Such a write
// committed nothing, so the counter must equal the successful increments,
// not the attempted ones. Unexpected errors still fail the test. That the
// retries make progress is covered deterministically in the Badger adapter
// (TestUpdateRetriesUntilCommit).
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
	var (
		wg                sync.WaitGroup
		succeeded, gaveUp atomic.Int64
		unexpected        = make(chan error, writers*per)
	)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				_, err := db.Query(ctx, "MATCH (c:Counter) SET c.v = c.v + 1", nil)
				switch {
				case err == nil:
					succeeded.Add(1)
				case errors.Is(err, ErrConflict):
					gaveUp.Add(1)
				default:
					unexpected <- err
				}
			}
		}()
	}
	wg.Wait()
	close(unexpected)
	for err := range unexpected {
		t.Errorf("write failed: %v", err)
	}
	if n := gaveUp.Load(); n > 0 {
		t.Logf("%d of %d writes gave up with ErrConflict after their retries", n, writers*per)
	}
	if succeeded.Load() == 0 {
		t.Fatal("no write succeeded: the retries made no progress")
	}
	res, err := db.Query(ctx, "MATCH (c:Counter) RETURN c.v", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Rows[0][0], succeeded.Load(); got != want {
		t.Errorf("counter = %v, want %d successful increments (lost or phantom updates)", got, want)
	}
}
