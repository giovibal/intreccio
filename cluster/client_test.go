package cluster

import (
	"context"
	"testing"

	"github.com/giovibal/intreccio"
)

// openClient starts a dataless client against the cluster's voters.
func (tc *testCluster) openClient(t *testing.T, id string) *intreccio.DB {
	t.Helper()
	db, err := Open(Config{
		NodeID: id,
		Role:   RoleClient,
		Peers:  tc.peers,
	})
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestClientForwarding verifies a dataless client can write and read through the
// cluster without holding any data, and keeps working after a voter fails.
func TestClientForwarding(t *testing.T) {
	tc := startCluster(t, 3)
	tc.leaderIndex(t) // ensure the cluster is up
	clientDB := tc.openClient(t, "client-1")
	ctx := context.Background()

	// Client write: forwarded to a voter, proxied to the leader, replicated.
	if _, err := clientDB.Query(ctx, "CREATE (n:Person {name: 'Alice'})", nil); err != nil {
		t.Fatalf("client write: %v", err)
	}

	// Linearizable client read is deterministic (served via the leader).
	if got := mustQuery(t, clientDB, matchNames, intreccio.Linearizable()); !equal(got, []string{"Alice"}) {
		t.Fatalf("client linearizable read = %v, want [Alice]", got)
	}

	// Default client read reaches the data eventually (any voter, round-robin).
	waitForLocal(t, clientDB, []string{"Alice"})

	// Kill one voter; the client must still operate via the remaining voters.
	lead := tc.leaderIndex(t)
	if err := tc.nodes[lead].Close(); err != nil {
		t.Fatalf("close a voter: %v", err)
	}
	tc.nodes[lead] = nil
	tc.leaderIndex(t) // wait for a new leader among survivors

	if _, err := clientDB.Query(ctx, "CREATE (n:Person {name: 'Bob'})", nil); err != nil {
		t.Fatalf("client write after voter failure: %v", err)
	}
	if got := mustQuery(t, clientDB, matchNames, intreccio.Linearizable()); !equal(got, []string{"Alice", "Bob"}) {
		t.Fatalf("client read after failover = %v, want [Alice Bob]", got)
	}
}
