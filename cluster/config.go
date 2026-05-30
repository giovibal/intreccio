package cluster

// Role is the part a node plays in the cluster.
type Role int

const (
	// RoleVoter holds a full data replica and votes in consensus. The quorum of
	// voters (typically 3 or 5) provides high availability.
	RoleVoter Role = iota
	// RoleClient holds no data and forwards queries to a data node. Used by extra
	// service instances so they share the database without replicating it.
	// (Forwarding is implemented in Phase C/D; Phase B supports voters only.)
	RoleClient
)

// Peer is a seed member used to form or join a cluster.
type Peer struct {
	ID      string
	Address string
}

// Config configures a clustered node. It is supplied from the library by the
// embedding service (see cluster.Open).
type Config struct {
	// NodeID is a stable, cluster-unique identifier for this node.
	NodeID string
	// DataDir holds the local store and the Raft log/snapshots (voters only).
	DataDir string
	// BindAddr is the address the Raft transport listens on (host:port).
	BindAddr string
	// Role is the part this node plays. Phase B: RoleVoter only.
	Role Role
	// Bootstrap forms a brand-new single-node cluster to grow from. Exactly one
	// node bootstraps a fresh cluster; the rest join an existing one.
	Bootstrap bool
	// Peers are seed members (used when joining; Phase C+).
	Peers []Peer
}
