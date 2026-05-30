package cluster

import (
	"io"
	"time"
)

// Role is the part a node plays in the cluster.
type Role int

const (
	// RoleVoter holds a full data replica and votes in consensus. The quorum of
	// voters (typically 3 or 5) provides high availability.
	RoleVoter Role = iota
	// RoleClient holds no data and forwards every query to a voter. Used by extra
	// service instances so they share the database without replicating it. A
	// client needs only NodeID and Peers (with ForwardAddr); it runs no Raft.
	RoleClient
)

// Peer identifies a voter in the cluster: its Raft server identity/address and
// the address of its forwarding RPC endpoint. Every node is configured with the
// full set of voters (including itself) so any node can locate the leader's
// forwarding endpoint.
type Peer struct {
	ID          string // Raft ServerID
	RaftAddr    string // Raft transport address (host:port)
	ForwardAddr string // forwarding RPC address (host:port)
}

// Config configures a clustered node. It is supplied from the library by the
// embedding service (see cluster.Open).
type Config struct {
	// NodeID is a stable, cluster-unique identifier for this node (Raft ServerID).
	NodeID string
	// DataDir holds the local store and the Raft log/snapshots (voters only).
	DataDir string
	// BindAddr is the address the Raft transport listens on (host:port).
	BindAddr string
	// ForwardAddr is the address the forwarding RPC server listens on (host:port).
	// Followers forward writes (and linearizable reads) to the leader here.
	ForwardAddr string
	// Role is the part this node plays. Phases B–C: RoleVoter only.
	Role Role
	// Bootstrap forms a brand-new cluster. Exactly one node bootstraps; it then
	// adds the other voters from Peers. On restart Bootstrap is ignored (the
	// configuration is recovered from the Raft log).
	Bootstrap bool
	// Peers is the full set of voters (including this node), used to add members
	// after bootstrap and to resolve the leader's forwarding address.
	Peers []Peer

	// LogOutput receives Raft's logs. Nil means silent (io.Discard); set it to
	// os.Stderr (or a file) for operational visibility.
	LogOutput io.Writer
	// ApplyTimeout bounds a single replicated write, read barrier and add-voter.
	// Zero uses defaultApplyTimeout.
	ApplyTimeout time.Duration
}
