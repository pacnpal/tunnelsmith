package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
)

// fakeHealer is a provider.Healer stub the tests drive deterministically.
// healFn lets tests inject success/failure paths or sleep semantics.
type fakeHealer struct {
	healFn  func(ctx context.Context) ([]config.UpstreamConfig, error)
	calls   atomic.Int64
	gotCtx  chan context.Context // optional; tests that care about cancellation
	gotCtx1 sync.Once
}

func (f *fakeHealer) Heal(ctx context.Context) ([]config.UpstreamConfig, error) {
	f.calls.Add(1)
	if f.gotCtx != nil {
		f.gotCtx1.Do(func() { f.gotCtx <- ctx })
	}
	if f.healFn != nil {
		return f.healFn(ctx)
	}
	return nil, nil
}

func quietDriverLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestDriver returns a driver pre-configured with the manual clock so
// tests can exercise the sliding window and cooldown without sleeping.
func newTestDriver(idPrefix string, healer *fakeHealer, now func() time.Time) *authHealDriver {
	d := newAuthHealDriver(idPrefix, healer, quietDriverLogger())
	if now != nil {
		d.clock = now
	}
	return d
}

// waitForCalls polls fakeHealer.calls until it reaches want or the
// timeout fires. The driver dispatches Heal on a goroutine so the test
// can't rely on synchronous completion; this helper keeps the assertion
// readable without sprinkling sleeps.
func waitForCalls(t *testing.T, h *fakeHealer, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.calls.Load() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("expected %d Heal calls within %v, observed %d", want, timeout, h.calls.Load())
}

// TestAuthHealDriverIgnoresNonProxyAuthKinds pins the filter: any kind
// other than KindProxyAuth must NOT increment the counter, so a busy
// pool seeing refused/timeout/etc. never triggers a heal.
func TestAuthHealDriverIgnoresNonProxyAuthKinds(t *testing.T) {
	t.Parallel()
	now := time.Now()
	clock := func() time.Time { return now }
	healer := &fakeHealer{}
	d := newTestDriver("ws", healer, clock)
	// Many events of the wrong kind: should not trigger.
	for _, k := range []failure.Kind{failure.KindRefused, failure.KindTimeout, failure.KindForbidden, failure.KindRateLimit} {
		for i := 0; i < 10; i++ {
			d.Observe("example.com", "ws-d-1", k)
		}
	}
	// Give any errant goroutine a chance to run before asserting.
	time.Sleep(20 * time.Millisecond)
	if got := healer.calls.Load(); got != 0 {
		t.Fatalf("Heal called %d times for non-proxy-auth kinds; want 0", got)
	}
}

// TestAuthHealDriverIgnoresOtherPools pins the id_prefix filter: events
// for upstreams whose IDs don't start with this driver's prefix are
// dropped without bumping the counter. Without this, two pools sharing
// one scoreboard would cross-trigger each other's heals.
func TestAuthHealDriverIgnoresOtherPools(t *testing.T) {
	t.Parallel()
	now := time.Now()
	clock := func() time.Time { return now }
	healer := &fakeHealer{}
	d := newTestDriver("ws", healer, clock)
	for i := 0; i < 10; i++ {
		// "mv-..." belongs to a different pool prefix.
		d.Observe("example.com", "mv-relay-1", failure.KindProxyAuth)
	}
	time.Sleep(20 * time.Millisecond)
	if got := healer.calls.Load(); got != 0 {
		t.Fatalf("Heal called %d times for other-pool events; want 0", got)
	}
}

// TestAuthHealDriverTriggersOnThreshold pins the threshold contract:
// when N KindProxyAuth events arrive within the window, Heal fires
// exactly once. Subsequent events within the cooldown do not re-fire.
func TestAuthHealDriverTriggersOnThreshold(t *testing.T) {
	t.Parallel()
	var nowMu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		now = now.Add(d)
		nowMu.Unlock()
	}
	healer := &fakeHealer{}
	d := newTestDriver("ws", healer, clock)
	// First (threshold-1) events: no trigger yet.
	for i := 0; i < authHealThreshold-1; i++ {
		d.Observe("example.com", "ws-d-1", failure.KindProxyAuth)
	}
	time.Sleep(5 * time.Millisecond)
	if got := healer.calls.Load(); got != 0 {
		t.Fatalf("Heal called %d times below threshold; want 0", got)
	}
	// Tip over the threshold.
	d.Observe("example.com", "ws-d-2", failure.KindProxyAuth)
	waitForCalls(t, healer, 1, 1*time.Second)
	// Further events within cooldown should NOT re-trigger.
	for i := 0; i < authHealThreshold*2; i++ {
		d.Observe("example.com", "ws-d-1", failure.KindProxyAuth)
	}
	time.Sleep(20 * time.Millisecond)
	if got := healer.calls.Load(); got != 1 {
		t.Fatalf("Heal called %d times during cooldown; want 1", got)
	}
	// After cooldown elapses and the threshold is met again, a second
	// heal fires.
	advance(authHealCooldown + 100*time.Millisecond)
	for i := 0; i < authHealThreshold; i++ {
		d.Observe("example.com", "ws-d-1", failure.KindProxyAuth)
	}
	waitForCalls(t, healer, 2, 1*time.Second)
}

// TestAuthHealDriverWindowEviction pins the sliding-window semantics:
// events older than the window do not contribute to the threshold.
// A trickle of one event per (window + epsilon) must NOT fire even
// over many ticks.
func TestAuthHealDriverWindowEviction(t *testing.T) {
	t.Parallel()
	var nowMu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		now = now.Add(d)
		nowMu.Unlock()
	}
	healer := &fakeHealer{}
	d := newTestDriver("ws", healer, clock)
	// Bump once per (window + 1s) — events should always evict before
	// the next bump, leaving the counter at 1.
	for i := 0; i < authHealThreshold*3; i++ {
		d.Observe("example.com", "ws-d-1", failure.KindProxyAuth)
		advance(authHealWindow + 1*time.Second)
	}
	time.Sleep(20 * time.Millisecond)
	if got := healer.calls.Load(); got != 0 {
		t.Fatalf("Heal called %d times despite window eviction; want 0", got)
	}
}

// TestAuthHealDriverHealErrorClearsInFlight pins the recovery behavior:
// when Heal returns an error, the driver does NOT permanently block
// subsequent heals. After the cooldown expires, a new burst can fire
// another heal even though the first one failed.
func TestAuthHealDriverHealErrorClearsInFlight(t *testing.T) {
	t.Parallel()
	var nowMu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		now = now.Add(d)
		nowMu.Unlock()
	}
	healer := &fakeHealer{
		healFn: func(ctx context.Context) ([]config.UpstreamConfig, error) {
			return nil, errors.New("simulated heal failure")
		},
	}
	d := newTestDriver("ws", healer, clock)
	for i := 0; i < authHealThreshold; i++ {
		d.Observe("example.com", "ws-d-1", failure.KindProxyAuth)
	}
	waitForCalls(t, healer, 1, 1*time.Second)
	// Advance past cooldown; a fresh burst should trigger a second heal
	// even though the first errored.
	advance(authHealCooldown + 100*time.Millisecond)
	for i := 0; i < authHealThreshold; i++ {
		d.Observe("example.com", "ws-d-1", failure.KindProxyAuth)
	}
	waitForCalls(t, healer, 2, 1*time.Second)
}

// TestAuthHealDriverSingleInFlight pins the concurrency contract: while
// one Heal is in flight, threshold-crossing bumps must not spawn a
// second concurrent Heal. Otherwise a sustained 407 storm could burn
// API quota with parallel refresh requests.
func TestAuthHealDriverSingleInFlight(t *testing.T) {
	t.Parallel()
	now := time.Now()
	clock := func() time.Time { return now }
	healStarted := make(chan struct{}, 1)
	healCanReturn := make(chan struct{})
	healer := &fakeHealer{
		healFn: func(ctx context.Context) ([]config.UpstreamConfig, error) {
			select {
			case healStarted <- struct{}{}:
			default:
			}
			<-healCanReturn
			return nil, nil
		},
	}
	d := newTestDriver("ws", healer, clock)
	for i := 0; i < authHealThreshold; i++ {
		d.Observe("example.com", "ws-d-1", failure.KindProxyAuth)
	}
	<-healStarted
	// Try to trigger again while the first heal is blocked.
	for i := 0; i < authHealThreshold*3; i++ {
		d.Observe("example.com", "ws-d-1", failure.KindProxyAuth)
	}
	time.Sleep(20 * time.Millisecond)
	if got := healer.calls.Load(); got != 1 {
		t.Fatalf("Heal called %d times while one was in flight; want 1", got)
	}
	// Let the first heal finish.
	close(healCanReturn)
}
