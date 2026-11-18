package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/giovibal/intreccio"
)

// failNextApply makes node's state machine fail to store the next committed
// write, as a disk failure would.
func failNextApply(node *raftNode) {
	var done atomic.Bool
	fault := func() error {
		if done.CompareAndSwap(false, true) {
			return errors.New("injected storage failure")
		}
		return nil
	}
	node.fsm.applyFault.Store(&fault)
}

// otherLeader waits for a leader other than old and returns its index.
func (tc *testCluster) otherLeader(t *testing.T, old int) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for i, nd := range tc.nodes {
			if i != old && nd != nil && nd.raft.State() == raft.Leader {
				return i
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no leader other than node %d", old)
	return -1
}

// waitFailed waits for node's state machine to have failed.
func waitFailed(t *testing.T, node *raftNode) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for node.fsm.failed() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the state machine never reported the injected failure")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLeaderFailsToStoreACommittedWrite covers #97 on the leader: the write
// is committed (the other replicas apply it) but the leader cannot store it.
// The caller must not be told it succeeded, the leader must stop serving and
// hand over leadership, and a restart must recover it.
func TestLeaderFailsToStoreACommittedWrite(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	old := tc.leaderIndex(t)

	failNextApply(tc.nodes[old])
	_, err := tc.dbs[old].Query(ctx, "CREATE (:Person {name: 'first'})", nil)
	if !errors.Is(err, intreccio.ErrWriteOutcomeUnknown) {
		t.Fatalf("write on the failing leader: err = %v, want ErrWriteOutcomeUnknown", err)
	}

	// The member stops serving its incomplete state, and takes no snapshot.
	if _, err := tc.dbs[old].Query(ctx, "MATCH (p:Person) RETURN count(p)", nil); !errors.Is(err, ErrReplicaFailed) {
		t.Errorf("read on the failed member: err = %v, want ErrReplicaFailed", err)
	}
	if _, err := tc.nodes[old].fsm.Snapshot(); !errors.Is(err, ErrReplicaFailed) {
		t.Errorf("snapshot of the failed member: err = %v, want ErrReplicaFailed", err)
	}

	// It hands leadership over; the write it could not store is committed,
	// and the cluster goes on.
	lead := tc.otherLeader(t, old)
	createPerson(t, tc.dbs[lead], "second")
	if got := personCount(t, tc.dbs[lead], intreccio.Linearizable()); got != 2 {
		t.Fatalf("count on the new leader = %d, want 2", got)
	}

	// Recovery: a restart replays the log into the state machine.
	if err := tc.nodes[old].Close(); err != nil {
		t.Fatalf("close the failed member: %v", err)
	}
	tc.nodes[old] = nil
	tc.restart(t, old)
	waitForCountLocal(t, tc.dbs[old], 2, 20*time.Second)
}

// TestFollowerFailsToStoreACommittedWrite covers #97 on a follower: the
// write succeeds for the caller (the leader stored it), and the follower
// stops serving instead of answering from a state missing that write.
func TestFollowerFailsToStoreACommittedWrite(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	lead := tc.leaderIndex(t)
	f := tc.aFollower(t)

	failNextApply(tc.nodes[f])
	createPerson(t, tc.dbs[lead], "first")
	waitFailed(t, tc.nodes[f])

	if _, err := tc.dbs[f].Query(ctx, "MATCH (p:Person) RETURN count(p)", nil); !errors.Is(err, ErrReplicaFailed) {
		t.Errorf("read on the failed follower: err = %v, want ErrReplicaFailed", err)
	}

	// Later commands still commit, and it applies none of them meanwhile.
	createPerson(t, tc.dbs[lead], "second")
	if got := personCount(t, tc.dbs[lead], intreccio.Linearizable()); got != 2 {
		t.Fatalf("count on the leader = %d, want 2", got)
	}

	// A dataless client is served by the healthy voters: the failed one
	// answers "unavailable" and the client moves on.
	client := tc.openClient(t, "client-replica-failure")
	for range 6 {
		if got := personCount(t, client); got != 2 {
			t.Fatalf("client count = %d, want 2", got)
		}
	}

	// Recovery.
	if err := tc.nodes[f].Close(); err != nil {
		t.Fatalf("close the failed follower: %v", err)
	}
	tc.nodes[f] = nil
	tc.restart(t, f)
	waitForCountLocal(t, tc.dbs[f], 2, 20*time.Second)
	createPerson(t, tc.dbs[lead], "third")
	waitForCountLocal(t, tc.dbs[f], 3, 20*time.Second)
}
