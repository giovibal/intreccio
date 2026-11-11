package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/hashicorp/raft"

	"github.com/giovibal/intreccio/internal/wire"
)

// localExecutor runs a request entirely on this node (no forwarding) and
// returns the columns plus the rows in the root package's canonical row
// encoding. It is injected by the root package, which owns the Cypher pipeline
// and the result value model; the cluster layer carries parameters and rows as
// opaque bytes and stays ignorant of both. ctx ends when the requester gives
// up, so the execution can stop too.
type localExecutor func(ctx context.Context, req wire.Request) (cols []string, rows [][]byte, err error)

// needsLeader reports whether the request must be served by the leader: writes
// (replicated) and linearizable reads. Default reads can be served by any voter.
func needsLeader(req wire.Request) bool { return req.Write || req.Linearizable }

// serve handles a request received from another member. Followers dial the
// leader directly; dataless clients dial any voter, which serves default reads
// locally and proxies leader-bound requests to the leader.
func (n *Node) serve(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
	// Leader-bound request received by a non-leader voter: proxy to the leader,
	// bounding the wait by the apply timeout.
	if needsLeader(req) && n.raft.State() != raft.Leader {
		ctx, cancel := context.WithTimeout(ctx, n.applyTimeout)
		defer cancel()
		return n.callLeader(ctx, req)
	}

	if req.Linearizable {
		if err := n.raft.Barrier(n.applyTimeout).Error(); err != nil {
			return nil, nil, fmt.Errorf("cluster: read barrier: %w", err)
		}
	}
	exec := n.localExec.Load()
	if exec == nil {
		return nil, nil, errors.New("cluster: no local executor wired")
	}
	return (*exec)(ctx, req)
}

// localSubscriber runs a live query on this node for a subscriber elsewhere. It
// is injected by the root package, like localExecutor.
type localSubscriber func(ctx context.Context, req wire.SubscribeRequest, sink wire.BatchSink) error

// serveSubscription serves a live query to another member — a dataless client —
// from this voter's own replica. Any voter can: a live query follows the local
// apply stream, so there is nothing to proxy to the leader.
func (n *Node) serveSubscription(ctx context.Context, req wire.SubscribeRequest, sink wire.BatchSink) error {
	sub := n.localSub.Load()
	if sub == nil {
		return &wire.Error{Code: wire.CodeUnavailable, Msg: "cluster: no live-query server wired"}
	}
	return (*sub)(ctx, req, sink)
}

// startForwardServer starts serving forwarded requests on cfg.ForwardAddr.
func (n *Node) startForwardServer() error {
	var (
		l   net.Listener
		err error
	)
	if n.tlsServer != nil {
		l, err = tls.Listen("tcp", n.cfg.ForwardAddr, n.tlsServer)
	} else {
		l, err = net.Listen("tcp", n.cfg.ForwardAddr)
	}
	if err != nil {
		return fmt.Errorf("cluster: forward listen: %w", err)
	}
	n.fwd = newForwardServer(l, n.serve)
	n.fwd.subscribe = n.serveSubscription
	n.fwd.start()
	return nil
}

// callLeader resolves the current leader's forwarding address and sends req to
// it (used by a voter for leader-bound requests, its own or proxied).
func (n *Node) callLeader(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
	_, leaderID := n.raft.LeaderWithID()
	if leaderID == "" {
		return nil, nil, errors.New("cluster: no leader available")
	}
	addr, ok := n.forwardAddrByID[string(leaderID)]
	if !ok {
		return nil, nil, fmt.Errorf("cluster: no forwarding address for leader %q", leaderID)
	}
	return n.pool.call(ctx, addr, req)
}
