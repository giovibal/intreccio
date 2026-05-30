package cluster

import (
	"encoding/gob"
	"errors"
	"fmt"
	"net"
	"net/rpc"

	"github.com/hashicorp/raft"
)

// localExecutor runs a query string entirely on this node (no forwarding) and
// returns serializable columns/rows. It is injected by the root package, which
// owns the Cypher pipeline; the cluster layer stays ignorant of it.
type localExecutor func(cypher string, params map[string]any) (cols []string, rows [][]any, err error)

// nullValue is a gob-encodable stand-in for a nil interface element, since gob
// cannot encode a nil interface inside a slice/map. Rows and params are
// sanitized on the wire and restored at the destination.
type nullValue struct{}

func init() {
	// Register the scalar types that may appear in query results and parameters.
	gob.Register(nullValue{})
	gob.Register(int(0))
	gob.Register(int64(0))
	gob.Register(float64(0))
	gob.Register(false)
	gob.Register("")
	gob.Register([]any(nil))
	gob.Register(map[string]any(nil))
}

// ForwardArgs is the request sent to the leader. It is exported only because
// net/rpc requires exported argument types; it is not part of the user API.
type ForwardArgs struct {
	Cypher       string
	Params       map[string]any
	Linearizable bool
}

// ForwardReply is the leader's response. Exported for the same reason as
// ForwardArgs.
type ForwardReply struct {
	Columns []string
	Rows    [][]any
	Err     string
}

// forwardService is the net/rpc receiver registered on every node. Followers
// dial it on the leader.
type forwardService struct{ node *Node }

// Query executes a forwarded query on the leader. Errors are returned in
// reply.Err (not as the RPC error) so the client sees the original message.
func (s *forwardService) Query(args *ForwardArgs, reply *ForwardReply) error {
	if s.node.raft.State() != raft.Leader {
		reply.Err = errNotLeader.Error()
		return nil
	}
	if args.Linearizable {
		if err := s.node.raft.Barrier(applyTimeout).Error(); err != nil {
			reply.Err = fmt.Sprintf("cluster: read barrier: %v", err)
			return nil
		}
	}
	exec := s.node.localExec
	if exec == nil {
		reply.Err = "cluster: no local executor wired"
		return nil
	}
	cols, rows, err := exec(args.Cypher, desanitizeParams(args.Params))
	if err != nil {
		reply.Err = err.Error()
		return nil
	}
	reply.Columns = cols
	reply.Rows = sanitizeRows(rows)
	return nil
}

// startForwardServer starts serving forwarding RPCs on cfg.ForwardAddr.
func (n *Node) startForwardServer() error {
	srv := rpc.NewServer()
	if err := srv.RegisterName("Forward", &forwardService{node: n}); err != nil {
		return fmt.Errorf("cluster: register forward service: %w", err)
	}
	l, err := net.Listen("tcp", n.cfg.ForwardAddr)
	if err != nil {
		return fmt.Errorf("cluster: forward listen: %w", err)
	}
	n.fwdListener = l
	go srv.Accept(l) // returns when the listener is closed
	return nil
}

// forwardToLeader sends a query to the current leader's forwarding endpoint.
func (n *Node) forwardToLeader(cypher string, params map[string]any, linearizable bool) ([]string, [][]any, error) {
	_, leaderID := n.raft.LeaderWithID()
	if leaderID == "" {
		return nil, nil, errors.New("cluster: no leader available")
	}
	addr, ok := n.forwardAddrByID[string(leaderID)]
	if !ok {
		return nil, nil, fmt.Errorf("cluster: no forwarding address for leader %q", leaderID)
	}

	client, err := rpc.Dial("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("cluster: dial leader: %w", err)
	}
	defer func() { _ = client.Close() }()

	args := &ForwardArgs{Cypher: cypher, Params: sanitizeParams(params), Linearizable: linearizable}
	var reply ForwardReply
	if err := client.Call("Forward.Query", args, &reply); err != nil {
		return nil, nil, fmt.Errorf("cluster: forward call: %w", err)
	}
	if reply.Err != "" {
		return nil, nil, errors.New(reply.Err)
	}
	return reply.Columns, desanitizeRows(reply.Rows), nil
}

// --- nil-safe wire sanitization -------------------------------------------------

func sanitizeValue(v any) any {
	if v == nil {
		return nullValue{}
	}
	return v
}

func desanitizeValue(v any) any {
	if _, ok := v.(nullValue); ok {
		return nil
	}
	return v
}

func sanitizeRows(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		r := make([]any, len(row))
		for j, v := range row {
			r[j] = sanitizeValue(v)
		}
		out[i] = r
	}
	return out
}

func desanitizeRows(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		r := make([]any, len(row))
		for j, v := range row {
			r[j] = desanitizeValue(v)
		}
		out[i] = r
	}
	return out
}

func sanitizeParams(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = sanitizeValue(v)
	}
	return out
}

func desanitizeParams(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = desanitizeValue(v)
	}
	return out
}
