package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giovibal/intreccio/internal/wire"
)

// startServer runs a forwarding server with the given handler on a free port.
func startServer(t *testing.T, handler requestHandler, opts ...func(*forwardServer)) (string, *forwardServer) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newForwardServer(l, handler)
	for _, o := range opts {
		o(srv)
	}
	srv.start()
	t.Cleanup(func() {
		_ = srv.shutdown()
		srv.wait()
	})
	return l.Addr().String(), srv
}

// echo answers with the query text as the only column and one row per
// parameter byte.
func echo(_ context.Context, req wire.Request) ([]string, [][]byte, error) {
	rows := make([][]byte, len(req.Params))
	for i, b := range req.Params {
		rows[i] = []byte{b}
	}
	return []string{req.Cypher}, rows, nil
}

func newTestPool(t *testing.T) *connPool {
	t.Helper()
	pool := newConnPool(nil)
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func (p *connPool) connTo(addr string) *muxConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[addr]; e != nil {
		return e.conn
	}
	return nil
}

func (s *forwardServer) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// TestConnPoolReuseAndRecovery checks that the pool reuses a connection across
// calls and transparently recovers when the pooled connection is dead.
func TestConnPoolReuseAndRecovery(t *testing.T) {
	addr, _ := startServer(t, echo)
	pool := newTestPool(t)
	ctx := context.Background()

	cols, rows, err := pool.call(ctx, addr, wire.Request{Cypher: "ping", Params: []byte{1, 2}})
	if err != nil || len(cols) != 1 || cols[0] != "ping" || len(rows) != 2 {
		t.Fatalf("call 1 = %v, %v, %v", cols, rows, err)
	}
	c1 := pool.connTo(addr)
	if c1 == nil {
		t.Fatal("no pooled connection after the first call")
	}

	if _, _, err := pool.call(ctx, addr, wire.Request{Cypher: "ping"}); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if pool.connTo(addr) != c1 {
		t.Fatal("expected the pooled connection to be reused")
	}

	// Kill the pooled connection; the next call must run on a fresh one.
	_ = c1.conn.Close()
	cols, _, err = pool.call(ctx, addr, wire.Request{Cypher: "again"})
	if err != nil || cols[0] != "again" {
		t.Fatalf("recovery call = %v, %v", cols, err)
	}
	if c := pool.connTo(addr); c == nil || c == c1 {
		t.Fatal("expected a fresh connection after the old one died")
	}
}

// TestConnPoolCallCanceledContext returns promptly without dialing when the
// context is already done.
func TestConnPoolCallCanceledContext(t *testing.T) {
	pool := newTestPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := pool.call(ctx, "127.0.0.1:1", wire.Request{Cypher: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if pool.connTo("127.0.0.1:1") != nil {
		t.Error("dialled although the context was already done")
	}
}

// TestConnPoolDialError surfaces a dial failure to the caller.
func TestConnPoolDialError(t *testing.T) {
	pool := newTestPool(t)
	if _, _, err := pool.call(context.Background(), "127.0.0.1:1", wire.Request{Cypher: "x"}); err == nil {
		t.Fatal("expected a dial error against an unused port")
	}
}

// TestConnPoolMultiplexes runs many concurrent requests and checks that they
// share one connection without mixing up their replies.
func TestConnPoolMultiplexes(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	addr, srv := startServer(t, func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		n := inFlight.Add(1)
		for {
			if p := peak.Load(); n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		return echo(ctx, req)
	})
	pool := newTestPool(t)

	const calls = 40
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("q%d", i)
			cols, rows, err := pool.call(context.Background(), addr, wire.Request{Cypher: want, Params: []byte{byte(i)}})
			if err != nil {
				errs <- err
				return
			}
			if len(cols) != 1 || cols[0] != want || len(rows) != 1 || rows[0][0] != byte(i) {
				errs <- fmt.Errorf("call %d got %v / %v", i, cols, rows)
			}
		}()
	}
	// Wait until every request is being served at once, then let them finish.
	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < calls {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests in flight", inFlight.Load(), calls)
		}
		time.Sleep(time.Millisecond)
	}
	if n := srv.connCount(); n != 1 {
		t.Errorf("%d connections for %d concurrent requests, want 1", n, calls)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if peak.Load() != calls {
		t.Errorf("peak concurrency = %d, want %d", peak.Load(), calls)
	}
}

// TestLargeResultIsChunked sends a result much larger than one Rows frame and
// checks that every row arrives intact and in order.
func TestLargeResultIsChunked(t *testing.T) {
	const n = 3000
	want := make([][]byte, n)
	total := 0
	for i := range want {
		want[i] = bytes.Repeat([]byte{byte(i), byte(i >> 8)}, 256+i%7)
		total += len(want[i])
	}
	if total < 4*wire.RowsChunkTarget {
		t.Fatalf("test result of %d bytes does not span several frames", total)
	}
	addr, _ := startServer(t, func(context.Context, wire.Request) ([]string, [][]byte, error) {
		return []string{"a", "b"}, want, nil
	})
	cols, rows, err := newTestPool(t).call(context.Background(), addr, wire.Request{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(cols) != 2 || len(rows) != n {
		t.Fatalf("got %d columns and %d rows", len(cols), len(rows))
	}
	for i := range want {
		if !bytes.Equal(rows[i], want[i]) {
			t.Fatalf("row %d differs", i)
		}
	}

	// Empty results and empty rows survive too.
	addr, _ = startServer(t, func(context.Context, wire.Request) ([]string, [][]byte, error) {
		return nil, [][]byte{{}, {}}, nil
	})
	cols, rows, err = newTestPool(t).call(context.Background(), addr, wire.Request{})
	if err != nil || len(cols) != 0 || len(rows) != 2 || len(rows[0]) != 0 {
		t.Fatalf("empty rows: %v, %v, %v", cols, rows, err)
	}
}

// TestCancellationReachesTheServer checks that cancelling a call stops the
// execution on the member serving it, and leaves the connection usable.
func TestCancellationReachesTheServer(t *testing.T) {
	started := make(chan struct{}, 1)
	stopped := make(chan error, 1)
	addr, _ := startServer(t, func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		if req.Cypher != "block" {
			return echo(ctx, req)
		}
		started <- struct{}{}
		<-ctx.Done()
		stopped <- ctx.Err()
		return nil, nil, ctx.Err()
	})
	pool := newTestPool(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := pool.call(ctx, addr, wire.Request{Cypher: "block"})
		done <- err
	}()
	<-started
	conn := pool.connTo(addr)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("call error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled call did not return")
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("server context error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancellation never reached the server")
	}

	// The connection, and other requests on it, are unaffected.
	if cols, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "after"}); err != nil || cols[0] != "after" {
		t.Fatalf("call after a cancellation = %v, %v", cols, err)
	}
	if pool.connTo(addr) != conn {
		t.Error("a cancelled request must not cost the connection")
	}
}

// TestDeadlineTravelsWithTheRequest checks that the caller's deadline bounds
// the execution on the serving member even if no Cancel frame arrives.
func TestDeadlineTravelsWithTheRequest(t *testing.T) {
	type seen struct {
		timeout time.Duration
		left    time.Duration
		ok      bool
	}
	got := make(chan seen, 1)
	addr, _ := startServer(t, func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		dl, ok := ctx.Deadline()
		got <- seen{timeout: req.Timeout, left: time.Until(dl), ok: ok}
		return nil, nil, nil
	})
	pool := newTestPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := pool.call(ctx, addr, wire.Request{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	s := <-got
	if !s.ok || s.timeout <= 20*time.Second || s.timeout > 30*time.Second || s.left <= 20*time.Second || s.left > 31*time.Second {
		t.Errorf("server saw timeout=%v, deadline in %v (set=%v); want about 30s", s.timeout, s.left, s.ok)
	}

	// Without a deadline the server gets none.
	if _, _, err := pool.call(context.Background(), addr, wire.Request{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if s := <-got; s.ok || s.timeout != 0 {
		t.Errorf("server saw a deadline (%v) although the caller had none", s.timeout)
	}

	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, _, err := pool.call(expired, addr, wire.Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expired deadline: err = %v, want context.DeadlineExceeded", err)
	}
}

// TestRemoteErrorsKeepTheirCode checks that an error reported by the serving
// member arrives as a *wire.Error with its code and message, and is not retried.
func TestRemoteErrorsKeepTheirCode(t *testing.T) {
	var calls atomic.Int32
	addr, _ := startServer(t, func(_ context.Context, req wire.Request) ([]string, [][]byte, error) {
		calls.Add(1)
		if req.Cypher == "limit" {
			return nil, nil, &wire.Error{Code: wire.CodeResultLimit, Msg: "too many rows"}
		}
		return nil, nil, errors.New("plain failure")
	})
	pool := newTestPool(t)

	_, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "limit"})
	var remote *wire.Error
	if !errors.As(err, &remote) || remote.Code != wire.CodeResultLimit || remote.Msg != "too many rows" {
		t.Errorf("coded error = %#v", err)
	}
	_, _, err = pool.call(context.Background(), addr, wire.Request{Cypher: "other"})
	if !errors.As(err, &remote) || remote.Code != wire.CodeGeneric || err.Error() != "plain failure" {
		t.Errorf("plain error = %#v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("the server ran %d requests for 2 failing calls: an error reply must not be retried", calls.Load())
	}
}

// TestOversizedRowIsAnErrorNotABrokenConnection checks the frame-size limit.
func TestOversizedRowIsAnErrorNotABrokenConnection(t *testing.T) {
	huge := make([]byte, wire.MaxPayload)
	addr, _ := startServer(t, func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		if req.Cypher == "huge" {
			return []string{"x"}, [][]byte{huge}, nil
		}
		return echo(ctx, req)
	})
	pool := newTestPool(t)
	if _, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "huge"}); err == nil {
		t.Fatal("a row over the frame limit: want an error")
	}
	conn := pool.connTo(addr)
	if _, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "ok"}); err != nil {
		t.Fatalf("call after an oversized row: %v", err)
	}
	if pool.connTo(addr) != conn {
		t.Error("an oversized row must not cost the connection")
	}
}

// TestProtocolVersionMismatch checks that members speaking different protocol
// versions fail at the handshake with an explicit error.
func TestProtocolVersionMismatch(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var in [8]byte
		_, _ = conn.Read(in[:])
		_, _ = conn.Write([]byte{'I', 'T', 'R', 'C', 0, byte(wire.Version + 1), 0, 0})
	}()

	_, _, err = newTestPool(t).call(context.Background(), l.Addr().String(), wire.Request{})
	var ve *wire.VersionError
	if !errors.As(err, &ve) || ve.Remote != wire.Version+1 {
		t.Fatalf("err = %v, want a version mismatch", err)
	}
}

// TestServerRejectsForeignPeers checks what the server does with a peer that
// does not speak the protocol, speaks another version, or breaks its rules.
func TestServerRejectsForeignPeers(t *testing.T) {
	addr, srv := startServer(t, echo)
	readAll := func(conn net.Conn) []byte {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var out []byte
		buf := make([]byte, 64)
		for {
			n, err := conn.Read(buf)
			out = append(out, buf[:n]...)
			if err != nil {
				return out
			}
		}
	}
	dial := func() net.Conn {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}

	// Not this protocol: closed without a word.
	conn := dial()
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	if got := readAll(conn); len(got) != 0 {
		t.Errorf("a foreign peer got %q, want nothing", got)
	}

	// Another version: told which version this member runs, then closed.
	conn = dial()
	_, _ = conn.Write([]byte{'I', 'T', 'R', 'C', 0, byte(wire.Version + 1), 0, 0})
	want := []byte{'I', 'T', 'R', 'C', byte(wire.Version >> 8), byte(wire.Version), 0, 0}
	if got := readAll(conn); !bytes.Equal(got, want) {
		t.Errorf("a peer on another version got %v, want %v", got, want)
	}

	// A frame only the server may send: closed.
	conn = dial()
	_ = wire.WriteHandshake(conn)
	_ = wire.WriteFrame(conn, 1, wire.FrameEnd, nil)
	if got := readAll(conn); !bytes.Equal(got, want) {
		t.Errorf("after a protocol violation the peer got %v, want only the handshake", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for srv.connCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d rejected connections still tracked", srv.connCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestKeepaliveDetectsADeadPeer checks both directions: a requester gives up on
// a member that went silent, and a member drops a requester that did, stopping
// the work it was doing for it.
func TestKeepaliveDetectsADeadPeer(t *testing.T) {
	fast := keepalive{interval: 20 * time.Millisecond, timeout: 150 * time.Millisecond}

	t.Run("silent member", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		hold := make(chan struct{})
		defer close(hold)
		go func() {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			var in [8]byte
			_, _ = conn.Read(in[:])
			_ = wire.WriteHandshake(conn)
			<-hold // completes the handshake, then never answers anything
		}()

		c, err := dialMux(context.Background(), l.Addr().String(), nil, fast)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		start := time.Now()
		_, _, err = c.query(context.Background(), wire.Request{Cypher: "x"})
		var ce *connError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want a connection error", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("took %v to give up on a silent member", elapsed)
		}
	})

	t.Run("silent requester", func(t *testing.T) {
		started := make(chan struct{}, 1)
		stopped := make(chan struct{}, 1)
		addr, srv := startServer(t, func(ctx context.Context, _ wire.Request) ([]string, [][]byte, error) {
			started <- struct{}{}
			<-ctx.Done()
			stopped <- struct{}{}
			return nil, nil, ctx.Err()
		}, func(s *forwardServer) { s.ka = fast })

		// A raw peer: handshake, one request, then silence (no pings).
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = wire.WriteHandshake(conn)
		_ = wire.WriteFrame(conn, 1, wire.FrameQuery, wire.EncodeRequest(wire.Request{Cypher: "x"}))
		<-started
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("the request of a silent requester was never cancelled")
		}
		deadline := time.Now().Add(5 * time.Second)
		for srv.connCount() != 0 {
			if time.Now().After(deadline) {
				t.Fatal("the silent requester's connection is still tracked")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	t.Run("pings keep an idle connection alive", func(t *testing.T) {
		addr, _ := startServer(t, echo, func(s *forwardServer) { s.ka = fast })
		pool := newTestPool(t)
		pool.ka = fast
		if _, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "a"}); err != nil {
			t.Fatal(err)
		}
		conn := pool.connTo(addr)
		time.Sleep(4 * fast.timeout)
		if _, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "b"}); err != nil {
			t.Fatal(err)
		}
		if pool.connTo(addr) != conn {
			t.Error("an idle connection did not survive several keepalive timeouts")
		}
	})
}

// TestShutdownFailsPendingCalls checks that closing either end releases the
// callers waiting on it.
func TestShutdownFailsPendingCalls(t *testing.T) {
	block := func(ctx context.Context, _ wire.Request) ([]string, [][]byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	pending := func(pool *connPool, addr string) chan error {
		done := make(chan error, 1)
		go func() {
			_, _, err := pool.call(context.Background(), addr, wire.Request{})
			done <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for pool.connTo(addr) == nil {
			if time.Now().After(deadline) {
				t.Fatal("the call never connected")
			}
			time.Sleep(time.Millisecond)
		}
		return done
	}
	expectError := func(name string, done chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: the pending call succeeded", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the pending call never returned", name)
		}
	}

	addr, srv := startServer(t, block)
	pool := newTestPool(t)
	done := pending(pool, addr)
	_ = srv.shutdown()
	srv.wait()
	expectError("server shutdown", done)

	addr, _ = startServer(t, block)
	pool = newConnPool(nil)
	done = pending(pool, addr)
	_ = pool.Close()
	expectError("pool close", done)
	if _, _, err := pool.call(context.Background(), addr, wire.Request{}); !errors.Is(err, errPoolClosed) {
		t.Errorf("call on a closed pool: err = %v, want errPoolClosed", err)
	}
}
