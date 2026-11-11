package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/giovibal/intreccio"
)

// TestNewLeaderWritesOnAppliedState covers #31. A freshly elected leader may
// hold committed log entries that its state machine has not applied yet. A
// write staged in that window reads stale data, and its write-set (e.g.
// c.v = c.v + 1) overwrites newer values: an acknowledged update is lost.
// The leader must wait for its state machine before the first write of its
// term.
func TestNewLeaderWritesOnAppliedState(t *testing.T) {
	tc := startCluster(t, 3)
	ctx := context.Background()
	lead := tc.leaderIndex(t)
	if _, err := tc.dbs[lead].Query(ctx, "CREATE (:Counter {v: 0})", nil); err != nil {
		t.Fatal(err)
	}

	// Make one follower's state machine lag: its log stays current (so it can
	// take over), but each command takes a while to be applied.
	lagging := tc.aFollower(t)
	waitForCounterLocal(t, tc.dbs[lagging])
	delay := func() { time.Sleep(300 * time.Millisecond) }
	tc.nodes[lagging].fsm.beforeApply.Store(&delay)

	const increments = 5
	for range increments {
		if _, err := tc.dbs[lead].Query(ctx, "MATCH (c:Counter) SET c.v = c.v + 1", nil); err != nil {
			t.Fatal(err)
		}
	}

	// Hand leadership to the lagging node while it is still applying.
	target := tc.peers[lagging]
	if err := tc.nodes[lead].raft.LeadershipTransferToServer(
		raft.ServerID(target.ID), raft.ServerAddress(target.RaftAddr)).Error(); err != nil {
		t.Fatalf("leadership transfer: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !tc.nodes[lagging].IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("lagging node did not become leader")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The first write of the new term.
	if _, err := tc.dbs[lagging].Query(ctx, "MATCH (c:Counter) SET c.v = c.v + 1", nil); err != nil {
		t.Fatal(err)
	}
	tc.nodes[lagging].fsm.beforeApply.Store(nil)

	res, err := tc.dbs[lagging].Query(ctx, "MATCH (c:Counter) RETURN c.v", nil, intreccio.Linearizable())
	if err != nil {
		t.Fatal(err)
	}
	if got := toInt64(t, res.Rows[0][0]); got != increments+1 {
		t.Fatalf("counter = %d, want %d: the new leader staged its write on stale state", got, increments+1)
	}
}

// waitForCounterLocal waits until the :Counter node has reached db's store.
func waitForCounterLocal(t *testing.T, db *intreccio.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := db.Query(context.Background(), "MATCH (c:Counter) RETURN count(c)", nil)
		if err == nil && toInt64(t, res.Rows[0][0]) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("counter did not replicate")
}
