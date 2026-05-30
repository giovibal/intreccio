package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/giovibal/mycypher"
)

// testCluster is an in-process cluster of voters for integration tests.
type testCluster struct {
	nodes []*Node
	dbs   []*mycypher.DB
	peers []Peer
}

// startCluster brings up n voters (node 0 bootstraps and adds the rest).
func startCluster(t *testing.T, n int) *testCluster {
	t.Helper()
	peers := make([]Peer, n)
	for i := range n {
		peers[i] = Peer{
			ID:          fmt.Sprintf("n%d", i+1),
			RaftAddr:    freeAddr(t),
			ForwardAddr: freeAddr(t),
		}
	}

	tc := &testCluster{peers: peers}
	for i := range n {
		cfg := Config{
			NodeID:      peers[i].ID,
			DataDir:     t.TempDir(),
			BindAddr:    peers[i].RaftAddr,
			ForwardAddr: peers[i].ForwardAddr,
			Role:        RoleVoter,
			Bootstrap:   i == 0,
			Peers:       peers,
		}
		node, err := newNode(cfg)
		if err != nil {
			t.Fatalf("start node %s: %v", cfg.NodeID, err)
		}
		tc.nodes = append(tc.nodes, node)
		tc.dbs = append(tc.dbs, mycypher.New(node))
	}
	t.Cleanup(func() {
		for _, nd := range tc.nodes {
			if nd != nil {
				_ = nd.Close()
			}
		}
	})
	return tc
}

// leaderIndex returns the index of the current leader, waiting for one.
func (tc *testCluster) leaderIndex(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for i, nd := range tc.nodes {
			if nd != nil && nd.IsLeader() {
				return i
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no leader elected")
	return -1
}

// aFollower returns the index of some live non-leader node.
func (tc *testCluster) aFollower(t *testing.T) int {
	t.Helper()
	lead := tc.leaderIndex(t)
	for i, nd := range tc.nodes {
		if nd != nil && i != lead {
			return i
		}
	}
	t.Fatal("no follower found")
	return -1
}

func mustQuery(t *testing.T, db *mycypher.DB, cypher string, opts ...mycypher.QueryOption) []string {
	t.Helper()
	res, err := db.Query(context.Background(), cypher, nil, opts...)
	if err != nil {
		t.Fatalf("query %q: %v", cypher, err)
	}
	return names(res.Rows)
}

const matchNames = "MATCH (p:Person) RETURN p.name AS name ORDER BY name"

// TestThreeVoterForwardingAndReplication verifies a write issued on a follower
// is forwarded to the leader, replicated to every node, and visible via both
// linearizable and (eventually) local reads.
func TestThreeVoterForwardingAndReplication(t *testing.T) {
	tc := startCluster(t, 3)
	follower := tc.aFollower(t)

	// Write on a follower: must be forwarded to the leader and succeed.
	if _, err := tc.dbs[follower].Query(context.Background(),
		"CREATE (n:Person {name: 'Alice'})", nil); err != nil {
		t.Fatalf("forwarded write: %v", err)
	}

	// Linearizable read on another follower sees it (routed via the leader).
	other := tc.aFollower(t)
	if got := mustQuery(t, tc.dbs[other], matchNames, mycypher.Linearizable()); !equal(got, []string{"Alice"}) {
		t.Fatalf("linearizable read = %v, want [Alice]", got)
	}

	// The write replicates to every node's local store (eventually).
	for i := range tc.nodes {
		waitForLocal(t, tc.dbs[i], []string{"Alice"})
	}
}

// TestLinearizableReadYourWrites checks that a linearizable read on a different
// node observes a just-acknowledged write.
func TestLinearizableReadYourWrites(t *testing.T) {
	tc := startCluster(t, 3)
	lead := tc.leaderIndex(t)

	if _, err := tc.dbs[lead].Query(context.Background(),
		"CREATE (n:Person {name: 'Bob'})", nil); err != nil {
		t.Fatalf("write on leader: %v", err)
	}

	follower := tc.aFollower(t)
	if got := mustQuery(t, tc.dbs[follower], matchNames, mycypher.Linearizable()); !equal(got, []string{"Bob"}) {
		t.Fatalf("read-your-writes (linearizable) = %v, want [Bob]", got)
	}
}

// TestLeaderFailover kills the leader and verifies the cluster stays available:
// previously-committed data survives and new writes succeed under a new leader.
func TestLeaderFailover(t *testing.T) {
	tc := startCluster(t, 3)

	if _, err := tc.dbs[tc.leaderIndex(t)].Query(context.Background(),
		"CREATE (n:Person {name: 'Alice'})", nil); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Kill the leader.
	lead := tc.leaderIndex(t)
	if err := tc.nodes[lead].Close(); err != nil {
		t.Fatalf("close leader: %v", err)
	}
	tc.nodes[lead] = nil

	// A new leader must emerge among the survivors.
	newLead := tc.leaderIndex(t)
	if newLead == lead {
		t.Fatalf("leader did not change")
	}

	// Old data survives, readable via a surviving node (linearizable).
	if got := mustQuery(t, tc.dbs[newLead], matchNames, mycypher.Linearizable()); !equal(got, []string{"Alice"}) {
		t.Fatalf("after failover read = %v, want [Alice]", got)
	}

	// New writes succeed under the new leader (issued from a surviving follower).
	survivor := tc.aFollower(t)
	if _, err := tc.dbs[survivor].Query(context.Background(),
		"CREATE (n:Person {name: 'Carol'})", nil); err != nil {
		t.Fatalf("write after failover: %v", err)
	}
	if got := mustQuery(t, tc.dbs[newLead], matchNames, mycypher.Linearizable()); !equal(got, []string{"Alice", "Carol"}) {
		t.Fatalf("after failover write = %v, want [Alice Carol]", got)
	}
}

// waitForLocal polls a node's local (non-linearizable) read until it matches
// want, proving the write replicated to that node's store.
func waitForLocal(t *testing.T, db *mycypher.DB, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		res, err := db.Query(context.Background(), matchNames, nil)
		if err == nil {
			got = names(res.Rows)
			if equal(got, want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("local read = %v, want %v", got, want)
}
