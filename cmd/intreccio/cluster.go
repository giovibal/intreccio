//go:build cluster

// This file is compiled only with `-tags cluster`, which turns the CLI into a
// cluster-capable binary by linking the clustering package (and Raft). The
// default build excludes it, keeping the standard binary embedded-only and
// dependency-light.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/giovibal/intreccio"
	"github.com/giovibal/intreccio/cluster"
)

var (
	clusterID        = flag.String("cluster-id", "", "node id; setting it enables cluster mode")
	clusterData      = flag.String("cluster-data", "", "data directory (voters)")
	clusterBind      = flag.String("cluster-bind", "", "Raft bind address host:port (voters)")
	clusterForward   = flag.String("cluster-forward", "", "forwarding RPC address host:port (voters)")
	clusterRole      = flag.String("cluster-role", "voter", "voter|client")
	clusterBootstrap = flag.Bool("cluster-bootstrap", false, "bootstrap a new cluster (one voter only)")
	clusterPeers     = flag.String("cluster-peers", "",
		"comma-separated voter set: id=raftAddr=forwardAddr,...")
)

func init() { clusterOpen = openCluster }

// openCluster opens a clustered DB when -cluster-id is set.
func openCluster() (*intreccio.DB, bool, error) {
	if *clusterID == "" {
		return nil, false, nil // not cluster mode
	}
	peers, err := parsePeers(*clusterPeers)
	if err != nil {
		return nil, true, err
	}
	role := cluster.RoleVoter
	switch *clusterRole {
	case "voter":
		role = cluster.RoleVoter
	case "client":
		role = cluster.RoleClient
	default:
		return nil, true, fmt.Errorf("invalid -cluster-role %q (voter|client)", *clusterRole)
	}

	db, err := cluster.Open(cluster.Config{
		NodeID:      *clusterID,
		DataDir:     *clusterData,
		BindAddr:    *clusterBind,
		ForwardAddr: *clusterForward,
		Role:        role,
		Bootstrap:   *clusterBootstrap,
		Peers:       peers,
		LogOutput:   os.Stderr,
	})
	if err == nil {
		fmt.Fprintf(os.Stderr, "connected to cluster node %q (%s)\n", *clusterID, *clusterRole)
	}
	return db, true, err
}

// parsePeers parses "id=raftAddr=forwardAddr,..." into cluster peers.
func parsePeers(s string) ([]cluster.Peer, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var peers []cluster.Peer
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, "=")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid peer %q (want id=raftAddr=forwardAddr)", item)
		}
		peers = append(peers, cluster.Peer{
			ID:          parts[0],
			RaftAddr:    parts[1],
			ForwardAddr: parts[2],
		})
	}
	return peers, nil
}
