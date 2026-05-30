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
	Write        bool
	Linearizable bool
}

// ForwardReply is the leader's response. Exported for the same reason as
// ForwardArgs.
type ForwardReply struct {
	Columns []string
	Rows    [][]any
	Err     string
}

// needsLeader reports whether the request must be served by the leader: writes
// (replicated) and linearizable reads. Default reads can be served by any voter.
func (a *ForwardArgs) needsLeader() bool { return a.Write || a.Linearizable }

// forwardService is the net/rpc receiver registered on every voter. Followers
// dial the leader directly; dataless clients dial any voter, which serves
// default reads locally and proxies leader-bound requests to the leader.
type forwardService struct{ node *Node }

// Query serves a forwarded query. Errors are returned in reply.Err (not as the
// RPC error) so the caller sees the original message.
func (s *forwardService) Query(args *ForwardArgs, reply *ForwardReply) error {
	n := s.node

	// Leader-bound request received by a non-leader voter: proxy to the leader.
	if args.needsLeader() && n.raft.State() != raft.Leader {
		rep, err := n.callLeader(args)
		if err != nil {
			reply.Err = err.Error()
			return nil
		}
		*reply = *rep
		return nil
	}

	if args.Linearizable {
		if err := n.raft.Barrier(n.applyTimeout).Error(); err != nil {
			reply.Err = fmt.Sprintf("cluster: read barrier: %v", err)
			return nil
		}
	}
	exec := n.localExec
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

// callLeader resolves the current leader's forwarding address and forwards args
// to it unchanged (used by a follower/voter that received a leader-bound query).
func (n *Node) callLeader(args *ForwardArgs) (*ForwardReply, error) {
	_, leaderID := n.raft.LeaderWithID()
	if leaderID == "" {
		return nil, errors.New("cluster: no leader available")
	}
	addr, ok := n.forwardAddrByID[string(leaderID)]
	if !ok {
		return nil, fmt.Errorf("cluster: no forwarding address for leader %q", leaderID)
	}
	return dialAndCall(addr, args)
}

// forward is the voter-side entry used by the root router: it forwards a query
// to the leader (writes, linearizable reads) and returns desanitized rows.
func (n *Node) forward(cypher string, params map[string]any, write, linearizable bool) ([]string, [][]any, error) {
	args := &ForwardArgs{
		Cypher:       cypher,
		Params:       sanitizeParams(params),
		Write:        write,
		Linearizable: linearizable,
	}
	rep, err := n.callLeader(args)
	if err != nil {
		return nil, nil, err
	}
	if rep.Err != "" {
		return nil, nil, errors.New(rep.Err)
	}
	return rep.Columns, desanitizeRows(rep.Rows), nil
}

// dialAndCall performs a single forwarding RPC to addr.
func dialAndCall(addr string, args *ForwardArgs) (*ForwardReply, error) {
	client, err := rpc.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cluster: dial %s: %w", addr, err)
	}
	defer func() { _ = client.Close() }()

	var reply ForwardReply
	if err := client.Call("Forward.Query", args, &reply); err != nil {
		return nil, fmt.Errorf("cluster: forward call: %w", err)
	}
	return &reply, nil
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
