package cluster

import (
	"net"
	"net/rpc"
	"testing"
)

// poolEcho is a minimal Forward service that echoes the query text, used to
// drive the connection pool in isolation.
type poolEcho struct{}

func (poolEcho) Query(args *ForwardArgs, reply *ForwardReply) error {
	reply.Columns = []string{args.Cypher}
	return nil
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName("Forward", poolEcho{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Accept(l)
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}

// TestConnPoolReuseAndRecovery checks that the pool reuses a connection across
// calls and transparently recovers when a pooled connection is dead.
func TestConnPoolReuseAndRecovery(t *testing.T) {
	addr := startEchoServer(t)
	pool := newConnPool(2)
	defer func() { _ = pool.Close() }()

	args := &ForwardArgs{Cypher: "ping"}

	// First call: dials, succeeds, returns the connection to the pool.
	rep, err := pool.call(addr, args)
	if err != nil || len(rep.Columns) != 1 || rep.Columns[0] != "ping" {
		t.Fatalf("call 1 = %v, %v", rep, err)
	}
	pool.mu.Lock()
	idle := len(pool.idle[addr])
	var c1 *rpc.Client
	if idle == 1 {
		c1 = pool.idle[addr][0]
	}
	pool.mu.Unlock()
	if idle != 1 {
		t.Fatalf("after call 1: %d idle conns, want 1", idle)
	}

	// Second call must reuse the very same pooled connection.
	if _, err := pool.call(addr, args); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	pool.mu.Lock()
	c2 := pool.idle[addr][0]
	pool.mu.Unlock()
	if c1 != c2 {
		t.Fatal("expected the pooled connection to be reused")
	}

	// Kill the pooled connection; the next call must retry on a fresh one.
	_ = c2.Close()
	rep, err = pool.call(addr, args)
	if err != nil || len(rep.Columns) != 1 || rep.Columns[0] != "ping" {
		t.Fatalf("recovery call = %v, %v", rep, err)
	}
}

// TestConnPoolDialError surfaces a dial failure to the caller.
func TestConnPoolDialError(t *testing.T) {
	pool := newConnPool(0)
	defer func() { _ = pool.Close() }()
	if _, err := pool.call("127.0.0.1:1", &ForwardArgs{Cypher: "x"}); err == nil {
		t.Fatal("expected a dial error against an unused port")
	}
}
