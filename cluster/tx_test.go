package cluster

import (
	"context"
	"errors"
	"testing"

	"github.com/giovibal/intreccio"
)

// TestUpdateOnCluster covers #36 in cluster mode: a multi-query Update on the
// leader replicates to every node, a failed one replicates nothing, and
// transactions are refused on followers and clients.
func TestUpdateOnCluster(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	lead := tc.dbs[tc.leaderIndex(t)]

	err := lead.Update(ctx, func(tx *intreccio.Tx) error {
		if _, err := tx.Query(ctx, "CREATE (:Person {name: 'Alice'})", nil); err != nil {
			return err
		}
		res, err := tx.Query(ctx, "MATCH (p:Person) RETURN count(p)", nil)
		if err != nil {
			return err
		}
		if n := res.Rows[0][0].(int64); n != 1 {
			t.Errorf("read-your-writes count = %d, want 1", n)
		}
		_, err = tx.Query(ctx, "CREATE (:Person {name: 'Bob'})", nil)
		return err
	})
	if err != nil {
		t.Fatalf("Update on leader: %v", err)
	}

	boom := errors.New("boom")
	err = lead.Update(ctx, func(tx *intreccio.Tx) error {
		if _, err := tx.Query(ctx, "CREATE (:Person {name: 'Carol'})", nil); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("failed Update = %v, want boom", err)
	}

	for i := range tc.nodes {
		waitForLocal(t, tc.dbs[i], []string{"Alice", "Bob"})
	}

	follower := tc.dbs[tc.aFollower(t)]
	if err := follower.Update(ctx, func(*intreccio.Tx) error { return nil }); !errors.Is(err, intreccio.ErrTxNotOnLeader) {
		t.Fatalf("Update on follower = %v, want ErrTxNotOnLeader", err)
	}
	if err := follower.View(ctx, func(tx *intreccio.Tx) error {
		_, err := tx.Query(ctx, matchNames, nil)
		return err
	}); err != nil {
		t.Fatalf("View on follower: %v", err)
	}
	client := tc.openClient(t, "client-tx")
	if err := client.View(ctx, func(*intreccio.Tx) error { return nil }); !errors.Is(err, intreccio.ErrTxNotOnLeader) {
		t.Fatalf("View on client = %v, want ErrTxNotOnLeader", err)
	}
}

// TestUpdatePanicReleasesTheStoreLock covers #99: a panic in a DB.Update
// callback, recovered by the application, must not leave the store's read
// lock held. A snapshot restore takes that lock exclusively and would wait
// forever, with every later reader queued behind it.
func TestUpdatePanicReleasesTheStoreLock(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 1)
	lead := tc.leaderIndex(t)
	db, node := tc.dbs[lead], tc.nodes[lead]

	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recovered %v, want the callback's panic", r)
			}
		}()
		_ = db.Update(ctx, func(tx *intreccio.Tx) error {
			if _, err := tx.Query(ctx, "CREATE (:Staged)", nil); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if !node.storeMu.TryLock() {
		t.Fatal("the store lock is still held after the panic unwound")
	}
	node.storeMu.Unlock()

	// Nothing was replicated, and the node keeps working.
	if _, err := db.Query(ctx, "CREATE (:After)", nil); err != nil {
		t.Fatalf("write after the panic: %v", err)
	}
	res, err := db.Query(ctx, "MATCH (n) RETURN labels(n)[0] AS l", nil, intreccio.Linearizable())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "After" {
		t.Errorf("rows = %v, want only the node written after the panic", res.Rows)
	}
}
