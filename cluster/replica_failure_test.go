package cluster

import (
	"bytes"
	"context"
	"errors"
	"io"
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

// TestFailedReplicaDoesNotKeepLeadership: a follower that failed can still
// be handed leadership (or win an election) afterwards, since its log is up
// to date. It must give leadership up again, so that writes keep working
// without restarting it.
func TestFailedReplicaDoesNotKeepLeadership(t *testing.T) {
	tc := startCluster(t, 3)
	lead := tc.leaderIndex(t)
	f := tc.aFollower(t)

	failNextApply(tc.nodes[f])
	createPerson(t, tc.dbs[lead], "first")
	waitFailed(t, tc.nodes[f])

	// Hand leadership to the failed member.
	p := tc.peers[f]
	if err := tc.nodes[lead].raft.LeadershipTransferToServer(raft.ServerID(p.ID), raft.ServerAddress(p.RaftAddr)).Error(); err != nil {
		t.Fatalf("transfer leadership to the failed member: %v", err)
	}

	// It gives it up to a healthy voter.
	deadline := time.Now().Add(20 * time.Second)
	healthy := -1
	for healthy < 0 {
		if time.Now().After(deadline) {
			t.Fatal("the failed member kept leadership")
		}
		if tc.nodes[f].raft.State() != raft.Leader {
			for i, nd := range tc.nodes {
				if i != f && nd.raft.State() == raft.Leader {
					healthy = i
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Writes work again, through the healthy leader and through the failed
	// member, which forwards them; nothing was restarted.
	createPerson(t, tc.dbs[healthy], "second")
	createPerson(t, tc.dbs[f], "third")
	if got := personCount(t, tc.dbs[healthy], intreccio.Linearizable()); got != 3 {
		t.Errorf("count = %d, want 3", got)
	}
}

// TestSecondFailureStillShedsLeadership: the leadership monitor started by a
// first failure must still cover a second failure after a restore cleared
// the first. A monitor that exited once the failure was cleared could miss a
// failure arriving while it released its ownership.
func TestSecondFailureStillShedsLeadership(t *testing.T) {
	tc := startCluster(t, 3)
	lead := tc.leaderIndex(t)
	f := tc.aFollower(t)

	failNextApply(tc.nodes[f])
	createPerson(t, tc.dbs[lead], "first")
	waitFailed(t, tc.nodes[f])

	// A snapshot installed from the leader clears the failure, and a second
	// one follows at once.
	var snap bytes.Buffer
	if err := tc.nodes[lead].store.Backup(&snap); err != nil {
		t.Fatalf("back up the leader: %v", err)
	}
	if err := tc.nodes[f].fsm.Restore(io.NopCloser(&snap)); err != nil {
		t.Fatalf("restore the failed member: %v", err)
	}
	if err := tc.nodes[f].fsm.failed(); err != nil {
		t.Fatalf("still failed after a restore: %v", err)
	}
	failNextApply(tc.nodes[f])
	createPerson(t, tc.dbs[lead], "second")
	waitFailed(t, tc.nodes[f])
	if !tc.nodes[f].shedding.Load() {
		t.Fatal("no leadership monitor after the second failure")
	}

	// Handed leadership, the twice-failed member gives it up again.
	p := tc.peers[f]
	if err := tc.nodes[lead].raft.LeadershipTransferToServer(raft.ServerID(p.ID), raft.ServerAddress(p.RaftAddr)).Error(); err != nil {
		t.Fatalf("transfer leadership to the failed member: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		healthy := -1
		if tc.nodes[f].raft.State() != raft.Leader {
			for i, nd := range tc.nodes {
				if i != f && nd.raft.State() == raft.Leader {
					healthy = i
				}
			}
		}
		if healthy >= 0 {
			createPerson(t, tc.dbs[healthy], "third")
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the member that failed twice kept leadership")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
