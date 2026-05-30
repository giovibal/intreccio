package cluster

import (
	"context"
	"net"
	"testing"
)

// freeAddr returns a currently-free loopback address for the Raft transport.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
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
