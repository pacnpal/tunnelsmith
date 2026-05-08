package upstream_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// mockUpstream is a test-only upstream whose Dial returns whatever the
// caller programmed via the dial function. The attempts counter exposes
// per-test "was this upstream tried?" assertions without instrumenting
// the pool itself.
type mockUpstream struct {
	id       string
	kind     config.UpstreamKind
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	attempts atomic.Int64
}

func (m *mockUpstream) ID() string                { return m.id }
func (m *mockUpstream) Kind() config.UpstreamKind { return m.kind }
func (m *mockUpstream) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	m.attempts.Add(1)
	return m.dial(ctx, network, addr)
}

func newMock(id string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *mockUpstream {
	return &mockUpstream{id: id, kind: config.KindDirect, dial: dial}
}

// alwaysRefused returns an error that IsConnectionRefused identifies.
func alwaysRefused() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(_ context.Context, _, _ string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
}

// alwaysTimeout returns an error that IsTimeout identifies.
func alwaysTimeout() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(_ context.Context, _, _ string) (net.Conn, error) {
		return nil, &timeoutError{}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// alwaysSucceeds returns a conn pair where the upstream side is closed
// immediately. Tests just need a non-nil net.Conn back.
func alwaysSucceeds() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(_ context.Context, _, _ string) (net.Conn, error) {
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func mustPool(t *testing.T, retryCap int, entries ...upstream.PoolEntry) *upstream.Pool {
	t.Helper()
	p, err := upstream.NewPool(entries, retryCap, quietLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

func TestNewPoolValidation(t *testing.T) {
	t.Parallel()

	t.Run("rejects empty pool", func(t *testing.T) {
		t.Parallel()
		_, err := upstream.NewPool(nil, 5, quietLogger())
		if err == nil {
			t.Fatal("NewPool with no entries returned nil err")
		}
	})

	t.Run("rejects retry cap below 1", func(t *testing.T) {
		t.Parallel()
		entry := upstream.PoolEntry{Up: newMock("a", alwaysSucceeds()), Priority: 10}
		_, err := upstream.NewPool([]upstream.PoolEntry{entry}, 0, quietLogger())
		if err == nil {
			t.Fatal("NewPool with retryCap=0 returned nil err")
		}
	})

	t.Run("rejects nil upstream entry", func(t *testing.T) {
		t.Parallel()
		_, err := upstream.NewPool([]upstream.PoolEntry{{Up: nil, Priority: 10}}, 5, quietLogger())
		if err == nil {
			t.Fatal("NewPool with nil upstream returned nil err")
		}
	})
}

func TestPoolPriorityOrderingStable(t *testing.T) {
	t.Parallel()
	a := newMock("a", alwaysRefused())
	b := newMock("b", alwaysRefused())
	c := newMock("c", alwaysRefused())
	// b at priority 10, a at 20, c at 20. Stable sort keeps a before c.
	pool := mustPool(t, 5,
		upstream.PoolEntry{Up: a, Priority: 20},
		upstream.PoolEntry{Up: b, Priority: 10},
		upstream.PoolEntry{Up: c, Priority: 20},
	)
	got := pool.IDs()
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("IDs() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("IDs() = %v, want %v", got, want)
		}
	}
}

func TestPoolFirstSuccess(t *testing.T) {
	t.Parallel()
	first := newMock("first", alwaysSucceeds())
	second := newMock("second", alwaysSucceeds())

	pool := mustPool(t, 5,
		upstream.PoolEntry{Up: first, Priority: 10},
		upstream.PoolEntry{Up: second, Priority: 20},
	)

	conn, id, err := pool.DialFor(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialFor: %v", err)
	}
	_ = conn.Close()
	if id != "first" {
		t.Fatalf("served upstream = %q, want %q", id, "first")
	}
	if got := first.attempts.Load(); got != 1 {
		t.Fatalf("first.attempts = %d, want 1", got)
	}
	if got := second.attempts.Load(); got != 0 {
		t.Fatalf("second.attempts = %d, want 0 (pool should not look past success)", got)
	}
}

func TestPoolAdvancesOnRefused(t *testing.T) {
	t.Parallel()
	bad := newMock("bad", alwaysRefused())
	good := newMock("good", alwaysSucceeds())

	pool := mustPool(t, 5,
		upstream.PoolEntry{Up: bad, Priority: 10},
		upstream.PoolEntry{Up: good, Priority: 20},
	)
	conn, id, err := pool.DialFor(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialFor: %v", err)
	}
	_ = conn.Close()
	if id != "good" {
		t.Fatalf("served upstream = %q, want %q", id, "good")
	}
	if got := bad.attempts.Load(); got != 1 {
		t.Fatalf("bad.attempts = %d, want 1", got)
	}
	if got := good.attempts.Load(); got != 1 {
		t.Fatalf("good.attempts = %d, want 1", got)
	}
}

func TestPoolAdvancesOnTimeout(t *testing.T) {
	t.Parallel()
	slow := newMock("slow", alwaysTimeout())
	good := newMock("good", alwaysSucceeds())

	pool := mustPool(t, 5,
		upstream.PoolEntry{Up: slow, Priority: 10},
		upstream.PoolEntry{Up: good, Priority: 20},
	)
	conn, id, err := pool.DialFor(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialFor: %v", err)
	}
	_ = conn.Close()
	if id != "good" {
		t.Fatalf("served upstream = %q, want %q", id, "good")
	}
}

func TestPoolAllRefusedAggregatesError(t *testing.T) {
	t.Parallel()
	a := newMock("a", alwaysRefused())
	b := newMock("b", alwaysRefused())

	pool := mustPool(t, 5,
		upstream.PoolEntry{Up: a, Priority: 10},
		upstream.PoolEntry{Up: b, Priority: 20},
	)
	conn, id, err := pool.DialFor(context.Background(), "tcp", "example.com:443")
	if err == nil {
		_ = conn.Close()
		t.Fatal("DialFor: expected error, got nil")
	}
	if conn != nil {
		t.Fatalf("DialFor returned non-nil conn alongside error: %T", conn)
	}
	if id != "" {
		t.Fatalf("DialFor returned id %q on error, want empty string", id)
	}
	if !failure.IsConnectionRefused(err) {
		t.Errorf("aggregated err does not classify as connection refused: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "a:") || !strings.Contains(msg, "b:") {
		t.Fatalf("aggregated err missing per-upstream ids: %q", msg)
	}
}

func TestPoolAllTimeoutAggregatesError(t *testing.T) {
	t.Parallel()
	a := newMock("a", alwaysTimeout())
	b := newMock("b", alwaysTimeout())

	pool := mustPool(t, 5,
		upstream.PoolEntry{Up: a, Priority: 10},
		upstream.PoolEntry{Up: b, Priority: 20},
	)
	_, _, err := pool.DialFor(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("DialFor: expected error, got nil")
	}
	if !failure.IsTimeout(err) {
		t.Errorf("aggregated err does not classify as timeout: %v", err)
	}
}

func TestPoolMixedFailureSurfacesUpstreamIDs(t *testing.T) {
	t.Parallel()
	// Mixed kinds. Joining short-circuits errors.As on the first net.Error
	// it finds, so we don't assert classification across the join here;
	// per-attempt classification is the scoreboard's job in Phase 4. What
	// the aggregated message must do is name every upstream that failed,
	// so operators can read it from a log line.
	a := newMock("a", alwaysRefused())
	b := newMock("b", alwaysTimeout())

	pool := mustPool(t, 5,
		upstream.PoolEntry{Up: a, Priority: 10},
		upstream.PoolEntry{Up: b, Priority: 20},
	)
	_, _, err := pool.DialFor(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("DialFor: expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "a:") || !strings.Contains(msg, "b:") {
		t.Fatalf("aggregated err missing per-upstream ids: %q", msg)
	}
}

func TestPoolRetryCapRespected(t *testing.T) {
	t.Parallel()
	// Five upstreams, all refuse. Retry cap of 2 means the pool only tries
	// the first two, returns the aggregated error, and never touches the
	// remaining three.
	mocks := []*mockUpstream{
		newMock("u0", alwaysRefused()),
		newMock("u1", alwaysRefused()),
		newMock("u2", alwaysRefused()),
		newMock("u3", alwaysRefused()),
		newMock("u4", alwaysRefused()),
	}
	entries := make([]upstream.PoolEntry, len(mocks))
	for i, m := range mocks {
		entries[i] = upstream.PoolEntry{Up: m, Priority: 10 + i}
	}
	pool := mustPool(t, 2, entries...)

	_, _, err := pool.DialFor(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("DialFor: expected error, got nil")
	}
	for i, m := range mocks {
		want := int64(0)
		if i < 2 {
			want = 1
		}
		if got := m.attempts.Load(); got != want {
			t.Errorf("%s.attempts = %d, want %d", m.id, got, want)
		}
	}
}

func TestPoolStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	a := newMock("a", alwaysRefused())
	b := newMock("b", alwaysSucceeds())

	pool := mustPool(t, 5,
		upstream.PoolEntry{Up: a, Priority: 10},
		upstream.PoolEntry{Up: b, Priority: 20},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := pool.DialFor(ctx, "tcp", "example.com:443")
	if err == nil {
		t.Fatal("DialFor on canceled context: expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DialFor on canceled context: err = %v, want context.Canceled", err)
	}
	if got := a.attempts.Load(); got != 0 {
		t.Errorf("a.attempts = %d, want 0 (canceled before first dial)", got)
	}
}

func TestPoolLenAndIDs(t *testing.T) {
	t.Parallel()
	pool := mustPool(t, 5,
		upstream.PoolEntry{Up: newMock("a", alwaysSucceeds()), Priority: 30},
		upstream.PoolEntry{Up: newMock("b", alwaysSucceeds()), Priority: 10},
	)
	if got := pool.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	got := pool.IDs()
	want := []string{"b", "a"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("IDs = %v, want %v", got, want)
	}
}

// TestPoolWithRealUpstreams plugs in two real upstream.Upstream instances
// (one socks5 dialer pointed at nothing, one direct) to confirm the pool
// works against the production Upstream impls and not just the mock.
func TestPoolWithRealUpstreams(t *testing.T) {
	t.Parallel()

	// Real direct upstream succeeds against a freshly-bound test server.
	dest, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dest: %v", err)
	}
	t.Cleanup(func() { _ = dest.Close() })
	go func() {
		for {
			c, err := dest.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	// SOCKS5 upstream pointed at a closed listener address: any dial through
	// it errors out, exercising the real socks5 dial path's error wrapping.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closed: %v", err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	bad, err := upstream.New(
		config.UpstreamConfig{ID: "socks-bad", Kind: config.KindSOCKS5, Addr: closedAddr},
		2*time.Second,
	)
	if err != nil {
		t.Fatalf("build bad upstream: %v", err)
	}
	good, err := upstream.New(
		config.UpstreamConfig{ID: "direct", Kind: config.KindDirect},
		2*time.Second,
	)
	if err != nil {
		t.Fatalf("build good upstream: %v", err)
	}

	pool, err := upstream.NewPool([]upstream.PoolEntry{
		{Up: bad, Priority: 10},
		{Up: good, Priority: 20},
	}, 5, quietLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	conn, id, err := pool.DialFor(context.Background(), "tcp", dest.Addr().String())
	if err != nil {
		t.Fatalf("DialFor: %v", err)
	}
	_ = conn.Close()
	if id != "direct" {
		t.Fatalf("served upstream = %q, want %q", id, "direct")
	}
}
