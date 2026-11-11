package cluster

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/giovibal/intreccio"
)

const liveTimeout = 10 * time.Second

func subscribeTo(t *testing.T, db *intreccio.DB, cypher string) *intreccio.Subscription {
	t.Helper()
	sub, err := db.Subscribe(context.Background(), cypher, nil)
	if err != nil {
		t.Fatalf("Subscribe(%q): %v", cypher, err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}

func nextBatch(t *testing.T, sub *intreccio.Subscription) intreccio.Change {
	t.Helper()
	select {
	case c, ok := <-sub.Changes():
		if !ok {
			t.Fatalf("subscription ended: %v", sub.Err())
		}
		return c
	case <-time.After(liveTimeout):
		t.Fatal("timed out waiting for a change")
	}
	return intreccio.Change{}
}

func noBatch(t *testing.T, sub *intreccio.Subscription) {
	t.Helper()
	select {
	case c, ok := <-sub.Changes():
		if ok {
			t.Fatalf("unexpected change: %+v", c)
		}
		t.Fatalf("subscription ended: %v", sub.Err())
	case <-time.After(300 * time.Millisecond):
	}
}

// liveSet follows a single-column subscription and tracks its current rows.
type liveSet struct {
	sub  *intreccio.Subscription
	rows map[any]int
}

func follow(sub *intreccio.Subscription) *liveSet {
	return &liveSet{sub: sub, rows: map[any]int{}}
}

// until applies batches until the tracked rows equal want (as a set).
func (l *liveSet) until(t *testing.T, want ...any) {
	t.Helper()
	target := map[any]int{}
	for _, w := range want {
		target[w]++
	}
	deadline := time.After(liveTimeout)
	for !reflect.DeepEqual(l.rows, target) {
		select {
		case c, ok := <-l.sub.Changes():
			if !ok {
				t.Fatalf("subscription ended: %v", l.sub.Err())
			}
			for _, row := range c.Removed {
				if l.rows[row[0]]--; l.rows[row[0]] <= 0 {
					delete(l.rows, row[0])
				}
			}
			for _, row := range c.Added {
				l.rows[row[0]]++
			}
		case <-deadline:
			t.Fatalf("rows = %v, want %v", l.rows, target)
		}
	}
}

func run(t *testing.T, db *intreccio.DB, cypher string) {
	t.Helper()
	if _, err := db.Query(context.Background(), cypher, nil); err != nil {
		t.Fatalf("%s: %v", cypher, err)
	}
}

const liveNames = "MATCH (p:Person) RETURN p.name AS name"

// TestSubscribeOnVoters verifies that every voter serves live queries from its
// own apply stream: a write made anywhere reaches a subscription on the leader
// and one on a follower.
func TestSubscribeOnVoters(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	follower := tc.aFollower(t)
	run(t, tc.dbs[leader], "CREATE (:Person {name: 'Ann'})")
	waitForLocal(t, tc.dbs[follower], []string{"Ann"})

	onLeader := subscribeTo(t, tc.dbs[leader], liveNames)
	onFollower := subscribeTo(t, tc.dbs[follower], "STREAM "+liveNames)
	for _, sub := range []*intreccio.Subscription{onLeader, onFollower} {
		first := nextBatch(t, sub)
		if !first.Initial || !reflect.DeepEqual(first.Added, [][]any{{"Ann"}}) {
			t.Fatalf("initial = %+v", first)
		}
	}

	// A write issued on the follower is forwarded, replicated and applied on
	// both: each subscription sees it.
	run(t, tc.dbs[follower], "CREATE (:Person {name: 'Bob'})")
	for _, sub := range []*intreccio.Subscription{onLeader, onFollower} {
		c := nextBatch(t, sub)
		if c.Initial || !reflect.DeepEqual(c.Added, [][]any{{"Bob"}}) || len(c.Removed) != 0 {
			t.Fatalf("after create = %+v", c)
		}
	}

	// An update is one batch: old row out, new row in.
	run(t, tc.dbs[leader], "MATCH (p:Person {name: 'Bob'}) SET p.name = 'Rob'")
	for _, sub := range []*intreccio.Subscription{onLeader, onFollower} {
		c := nextBatch(t, sub)
		if !reflect.DeepEqual(c.Added, [][]any{{"Rob"}}) || !reflect.DeepEqual(c.Removed, [][]any{{"Bob"}}) {
			t.Fatalf("after update = %+v", c)
		}
	}

	// Writes outside the footprint wake nobody.
	run(t, tc.dbs[leader], "CREATE (:Company {name: 'Acme'})")
	run(t, tc.dbs[leader], "MATCH (p:Person) SET p.age = 1")
	noBatch(t, onLeader)
	noBatch(t, onFollower)

	run(t, tc.dbs[follower], "MATCH (p:Person {name: 'Ann'}) DELETE p")
	for _, sub := range []*intreccio.Subscription{onLeader, onFollower} {
		c := nextBatch(t, sub)
		if len(c.Added) != 0 || !reflect.DeepEqual(c.Removed, [][]any{{"Ann"}}) {
			t.Fatalf("after delete = %+v", c)
		}
	}
}

// TestSubscriptionSurvivesLeaderChange verifies that a follower's subscription
// does not depend on who the leader is: it keeps receiving after a failover.
func TestSubscriptionSurvivesLeaderChange(t *testing.T) {
	tc := startCluster(t, 3)
	oldLeader := tc.leaderIndex(t)
	follower := tc.aFollower(t)

	live := follow(subscribeTo(t, tc.dbs[follower], liveNames))
	run(t, tc.dbs[oldLeader], "CREATE (:Person {name: 'Ann'})")
	live.until(t, "Ann")

	if err := tc.nodes[oldLeader].Close(); err != nil {
		t.Fatalf("close leader: %v", err)
	}
	tc.nodes[oldLeader] = nil
	newLeader := tc.leaderIndex(t)
	if newLeader == oldLeader {
		t.Fatal("leader did not change")
	}

	run(t, tc.dbs[newLeader], "CREATE (:Person {name: 'Bob'})")
	live.until(t, "Ann", "Bob")

	// Still working for writes forwarded from the subscribing node itself.
	run(t, tc.dbs[follower], "MATCH (p:Person {name: 'Ann'}) DELETE p")
	live.until(t, "Bob")
}

// TestSubscriptionFollowsSnapshotRestore replaces the whole state through a
// Raft snapshot restore — which reaches the leader's FSM directly and the
// followers through snapshot installation, with no write-set to describe it —
// and checks that subscriptions on both converge on the restored state.
func TestSubscriptionFollowsSnapshotRestore(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	follower := tc.aFollower(t)

	// Company is interned before Person here, so a restored state is read
	// through the same dictionary the snapshot carries.
	run(t, tc.dbs[leader], "CREATE (:Company {name: 'Acme'})")
	run(t, tc.dbs[leader], "CREATE (:Person {name: 'Ann'}), (:Person {name: 'Bob'})")

	snap := tc.nodes[leader].raft.Snapshot()
	if err := snap.Error(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	meta, rc, err := snap.Open()
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = rc.Close() }()

	onLeader := follow(subscribeTo(t, tc.dbs[leader], liveNames))
	onFollower := follow(subscribeTo(t, tc.dbs[follower], liveNames))
	onLeader.until(t, "Ann", "Bob")
	onFollower.until(t, "Ann", "Bob")

	// Diverge from the snapshot.
	run(t, tc.dbs[leader], "MATCH (p:Person {name: 'Ann'}) DELETE p")
	run(t, tc.dbs[leader], "CREATE (:Person {name: 'Cy'})")
	onLeader.until(t, "Bob", "Cy")
	onFollower.until(t, "Bob", "Cy")

	// Roll the cluster back to the snapshot.
	if err := tc.nodes[leader].raft.Restore(meta, rc, 30*time.Second); err != nil {
		t.Fatalf("restore: %v", err)
	}
	onLeader.until(t, "Ann", "Bob")
	onFollower.until(t, "Ann", "Bob")

	// Subscriptions keep working on top of the restored state.
	run(t, tc.dbs[tc.leaderIndex(t)], "CREATE (:Person {name: 'Dee'})")
	onLeader.until(t, "Ann", "Bob", "Dee")
	onFollower.until(t, "Ann", "Bob", "Dee")
}
