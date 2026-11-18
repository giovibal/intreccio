package cluster

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/giovibal/intreccio/internal/wire"
)

// A member of the previous protocol version speaks only that version: it
// answers any other with its own and hangs up. These tests stand one in with
// forwardServer.onlyVersion (for its accepting side) and a raw connection
// (for its dialling side), and check that forwarding keeps working between
// it and a current member, both ways, as during a rolling upgrade.

const legacyVersion = wire.VersionStaleWriteCheck - 1

// TestForwardingToAnOlderMember: a current member dials an older one, is
// turned down, dials again in the older version and is served.
func TestForwardingToAnOlderMember(t *testing.T) {
	addr, _ := startServer(t, echo, func(s *forwardServer) { s.onlyVersion = legacyVersion })
	pool := newTestPool(t)
	cols, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "from a current member"})
	if err != nil || len(cols) != 1 || cols[0] != "from a current member" {
		t.Fatalf("call = %v, %v", cols, err)
	}
	if v := pool.connTo(addr).version; v != legacyVersion {
		t.Errorf("negotiated version = %d, want %d", v, legacyVersion)
	}

	// The version probe reports the older member as such.
	n := &raftNode{}
	if v, err := n.dialVersion(context.Background(), addr); err != nil || v != legacyVersion {
		t.Errorf("probe = %d, %v; want %d", v, err, legacyVersion)
	}
}

// TestForwardingFromAnOlderMember: an older member dials a current one in its
// own version, expects that version back, and is served.
func TestForwardingFromAnOlderMember(t *testing.T) {
	addr, _ := startServer(t, echo)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteHandshakeVersion(conn, legacyVersion); err != nil {
		t.Fatal(err)
	}
	got, err := wire.ReadHandshakeVersion(conn)
	if err != nil || got != legacyVersion {
		t.Fatalf("handshake answer = %d, %v; want %d (an older member requires its own version back)", got, err, legacyVersion)
	}
	c := &muxConn{conn: conn, ka: defaultKeepalive, version: got, streams: map[uint32]*muxStream{}, closed: make(chan struct{})}
	go c.readLoop()
	go c.pingLoop()
	defer c.Close()
	cols, _, _, err := c.query(context.Background(), wire.Request{Cypher: "from an older member"})
	if err != nil || len(cols) != 1 || cols[0] != "from an older member" {
		t.Fatalf("query = %v, %v", cols, err)
	}

	// The probe of a current member reports the current version.
	n := &raftNode{}
	if v, err := n.dialVersion(context.Background(), addr); err != nil || v != wire.Version {
		t.Errorf("probe = %d, %v; want %d", v, err, wire.Version)
	}
}
