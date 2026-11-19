package cluster

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"testing"
)

// Ports for cluster members in tests come from a range below the kernel's
// ephemeral range (32768-60999 on Linux, 49152-65535 elsewhere). A port taken
// with :0 comes from the ephemeral range, and so does the local port of every
// outgoing connection: between freeAddr closing its probe listener and the
// member binding the port, a connection opened meanwhile could take it, and
// the member failed with "address already in use". Below that range no
// outgoing connection can.
const (
	testPortFirst = 20000
	testPortLast  = 32000
)

var (
	testPortMu   sync.Mutex
	testPortNext = testPortFirst + rand.IntN(testPortLast-testPortFirst)
)

// freeAddr returns a loopback address for a cluster member that is free now
// and that no other call in this process returns.
func freeAddr(t *testing.T) string {
	t.Helper()
	testPortMu.Lock()
	defer testPortMu.Unlock()
	for range testPortLast - testPortFirst {
		port := testPortNext
		if testPortNext++; testPortNext >= testPortLast {
			testPortNext = testPortFirst
		}
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			continue // in use by another process
		}
		_ = l.Close()
		return addr
	}
	t.Fatal("no free port for a cluster member")
	return ""
}

// TestSingleVoterReadWriteAndReopen verifies the Phase B vertical slice: a
// single bootstrapped voter accepts a write replicated through Raft, serves it
// back via a read, and — after closing and reopening the same DataDir — still
// has the data (durability + Raft restart).
func TestSingleVoterReadWriteAndReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		NodeID:      "n1",
		DataDir:     dir,
		BindAddr:    freeAddr(t),
		ForwardAddr: freeAddr(t),
		Role:        RoleVoter,
		Bootstrap:   true,
	}

	ctx := context.Background()

	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := db.Query(ctx, "CREATE (n:Person {name: 'Alice'})", nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if _, err := db.Query(ctx, "CREATE (n:Person {name: 'Bob'})", nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	res, err := db.Query(ctx, "MATCH (p:Person) RETURN p.name AS name ORDER BY name", nil)
	if err != nil {
		t.Fatalf("MATCH: %v", err)
	}
	if got := names(res.Rows); !equal(got, []string{"Alice", "Bob"}) {
		t.Fatalf("after write: names = %v, want [Alice Bob]", got)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the same data directory without bootstrapping; the data must
	// survive (committed to the local store; Raft recovers its log).
	cfg.Bootstrap = false
	db2, err := Open(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()

	res2, err := db2.Query(ctx, "MATCH (p:Person) RETURN p.name AS name ORDER BY name", nil)
	if err != nil {
		t.Fatalf("MATCH after reopen: %v", err)
	}
	if got := names(res2.Rows); !equal(got, []string{"Alice", "Bob"}) {
		t.Fatalf("after reopen: names = %v, want [Alice Bob]", got)
	}
}

func names(rows [][]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) == 0 {
			continue
		}
		s, _ := r[0].(string)
		out = append(out, s)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
