package cluster

import (
	"context"
	"crypto/tls"
	"encoding/gob"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"time"

	"github.com/hashicorp/raft"

	"github.com/giovibal/intreccio"
)

// localExecutor runs a query string entirely on this node (no forwarding) and
// returns serializable columns/rows. It is injected by the root package, which
// owns the Cypher pipeline; the cluster layer stays ignorant of it.
type localExecutor func(ctx context.Context, cypher string, params map[string]any, maxRows int64) (cols []string, rows [][]any, err error)

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
	// MaxRows is the caller's WithMaxRows (0 = no cap).
	MaxRows int64
	// Timeout is what was left of the caller's deadline when the request was
	// sent (0 = no deadline). A duration, not a time, so clock skew between
	// nodes does not matter. Cancellation without a deadline is not
	// propagated: the caller stops waiting, the leader finishes the query.
	Timeout time.Duration
}

// ForwardReply is the leader's response. Exported for the same reason as
// ForwardArgs.
type ForwardReply struct {
	Columns []string
	Rows    [][]any
	Err     string
	// Code names the sentinel error behind Err, if any (see wireErrors), so
	// errors.Is keeps working on the caller.
	Code string
}

// newForwardArgs builds a request carrying ctx's remaining deadline.
func newForwardArgs(ctx context.Context, cypher string, params map[string]any, write, linearizable bool, maxRows int64) (*ForwardArgs, error) {
	args := &ForwardArgs{
		Cypher:       cypher,
		Params:       sanitizeParams(params),
		Write:        write,
		Linearizable: linearizable,
		MaxRows:      maxRows,
	}
	if deadline, ok := ctx.Deadline(); ok {
		args.Timeout = time.Until(deadline)
		if args.Timeout <= 0 {
			return nil, context.DeadlineExceeded
		}
	}
	return args, nil
}

// context returns the context a forwarded request runs under: the caller's
// remaining deadline, or fallback when the caller set none (0 = unbounded).
func (a *ForwardArgs) context(fallback time.Duration) (context.Context, context.CancelFunc) {
	if a.Timeout > 0 {
		return context.WithTimeout(context.Background(), a.Timeout)
	}
	if fallback > 0 {
		return context.WithTimeout(context.Background(), fallback)
	}
	return context.WithCancel(context.Background())
}

// wireErrors are the sentinel errors that keep their identity across a
// forward. The codes are part of the wire format: never reuse or rename one.
var wireErrors = []struct {
	code string
	err  error
}{
	{"result_limit", intreccio.ErrResultLimit},
	{"conflict", intreccio.ErrConflict},
	{"txn_too_large", intreccio.ErrTxnTooLarge},
	{"integer_overflow", intreccio.ErrIntegerOverflow},
	{"deadline_exceeded", context.DeadlineExceeded},
	{"canceled", context.Canceled},
}

// setError stores err in the reply, with its sentinel code when it has one.
func (r *ForwardReply) setError(err error) {
	r.Err = err.Error()
	r.Code = ""
	for _, w := range wireErrors {
		if errors.Is(err, w.err) {
			r.Code = w.code
			return
		}
	}
}

// error rebuilds the reply's error: the original message, unwrapping to the
// sentinel named by Code (an unknown code from a newer node is ignored).
func (r *ForwardReply) error() error {
	for _, w := range wireErrors {
		if w.code == r.Code {
			return &remoteError{msg: r.Err, sentinel: w.err}
		}
	}
	return errors.New(r.Err)
}

// remoteError is an error that happened on another node.
type remoteError struct {
	msg      string
	sentinel error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.sentinel }

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
	// Bound the proxy wait by the caller's deadline, else the apply timeout.
	if args.needsLeader() && n.raft.State() != raft.Leader {
		ctx, cancel := args.context(n.applyTimeout)
		defer cancel()
		rep, err := n.callLeader(ctx, args)
		if err != nil {
			if args.Timeout > 0 {
				reply.setError(err)
			} else {
				// Our own proxy timeout, not the caller's: leave it uncoded so
				// a client may still try another voter.
				reply.Err = err.Error()
			}
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
	ctx, cancel := args.context(0)
	defer cancel()
	cols, rows, err := exec(ctx, args.Cypher, desanitizeParams(args.Params), args.MaxRows)
	if err != nil {
		reply.setError(err)
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
	n.fwdListener = l
	go srv.Accept(l) // returns when the listener is closed
	return nil
}

// callLeader resolves the current leader's forwarding address and forwards args
// to it unchanged (used by a follower/voter that received a leader-bound query).
func (n *Node) callLeader(ctx context.Context, args *ForwardArgs) (*ForwardReply, error) {
	_, leaderID := n.raft.LeaderWithID()
	if leaderID == "" {
		return nil, errors.New("cluster: no leader available")
	}
	addr, ok := n.forwardAddrByID[string(leaderID)]
	if !ok {
		return nil, fmt.Errorf("cluster: no forwarding address for leader %q", leaderID)
	}
	return n.pool.call(ctx, addr, args)
}

// forward is the voter-side entry used by the root router: it forwards a query
// to the leader (writes, linearizable reads) and returns desanitized rows.
func (n *Node) forward(ctx context.Context, cypher string, params map[string]any, write, linearizable bool, maxRows int64) ([]string, [][]any, error) {
	args, err := newForwardArgs(ctx, cypher, params, write, linearizable, maxRows)
	if err != nil {
		return nil, nil, err
	}
	rep, err := n.callLeader(ctx, args)
	if err != nil {
		return nil, nil, err
	}
	if rep.Err != "" {
		return nil, nil, rep.error()
	}
	return rep.Columns, desanitizeRows(rep.Rows), nil
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
