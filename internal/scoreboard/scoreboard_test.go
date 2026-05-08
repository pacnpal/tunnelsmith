package scoreboard_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// fakeUpstream is a programmable upstream.Upstream. behavior is the function
// invoked on each Dial; it returns either a fake conn (treated as success)
// or an error (treated as failure). dialCount records how many times Dial
// was called, useful for per-test "this upstream was tried N times"
// assertions.
type fakeUpstream struct {
	id        string
	kind      config.UpstreamKind
	behavior  func() (net.Conn, error)
	dialCount atomic.Int64
}

func (f *fakeUpstream) ID() string                { return f.id }
func (f *fakeUpstream) Kind() config.UpstreamKind { return f.kind }
func (f *fakeUpstream) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	f.dialCount.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.behavior()
}

func newFakeUpstream(id string, behavior func() (net.Conn, error)) *fakeUpstream {
	return &fakeUpstream{
		id:       id,
		kind:     config.KindDirect,
		behavior: behavior,
	}
}

// alwaysOK fake: every dial succeeds, returns a single conn end of a
// net.Pipe so the conn is real and Closeable.
func alwaysOK(id string) *fakeUpstream {
	return newFakeUpstream(id, func() (net.Conn, error) {
		c1, c2 := net.Pipe()
		go func() { _ = c2.Close() }()
		return c1, nil
	})
}

// alwaysRefused fake: every dial returns a wrapped ECONNREFUSED so the
// scoreboard's Classify path picks KindRefused.
func alwaysRefused(id string) *fakeUpstream {
	return newFakeUpstream(id, func() (net.Conn, error) {
		return nil, fmt.Errorf("dial %s: %w", id, syscall.ECONNREFUSED)
	})
}

// manualClock is a goroutine-safe stand-in for time.Now. Tests advance it
// by Add() to drive cooldowns, decay, and cascade TTL deterministically.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock(start time.Time) *manualClock {
	return &manualClock{now: start}
}

func (m *manualClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

func (m *manualClock) Add(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	m.mu.Unlock()
}

// quietLogger discards logs so tests do not spam stderr.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// buildScoreboard builds a Scoreboard wrapping a pool of the given
// fakeUpstreams in declaration order (priority 10, 20, 30, ...). cfg is the
// scoreboard tuning; tests override fields they care about. clock and seed
// are injected so behavior is deterministic.
func buildScoreboard(t *testing.T, ups []*fakeUpstream, cfg scoreboard.Config, clock *manualClock, seed int64) *scoreboard.Scoreboard {
	t.Helper()
	if clock == nil {
		clock = newManualClock(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	}
	entries := make([]upstream.PoolEntry, len(ups))
	for i, u := range ups {
		entries[i] = upstream.PoolEntry{Up: u, Priority: 10 * (i + 1)}
	}
	pool, err := upstream.NewPool(entries, len(ups), quietLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if cfg.KindPolicy == nil {
		cfg.KindPolicy = map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 3, Cooldown: 30 * time.Second},
			failure.KindTimeout: {Penalty: 2, Cooldown: 15 * time.Second},
		}
	}
	if cfg.SuccessWeight == 0 {
		cfg.SuccessWeight = 1
	}
	if cfg.ScoreCap == 0 {
		cfg.ScoreCap = 10
	}
	if cfg.DecayInterval == 0 {
		cfg.DecayInterval = 5 * time.Minute
	}
	if cfg.CascadeTTL == 0 {
		cfg.CascadeTTL = 30 * time.Second
	}
	if cfg.DebounceWindow == 0 {
		cfg.DebounceWindow = 100 * time.Millisecond
	}
	r := rand.New(rand.NewSource(seed))
	sb, err := scoreboard.New(pool, cfg,
		scoreboard.WithLogger(quietLogger()),
		scoreboard.WithClock(clock.Now),
		scoreboard.WithRand(r),
	)
	if err != nil {
		t.Fatalf("scoreboard.New: %v", err)
	}
	return sb
}

// dialOnce drives a single DialFor call and closes the returned conn when
// the dial succeeded. Returns the upstream id that served, or "" + the err.
func dialOnce(t *testing.T, sb *scoreboard.Scoreboard, addr string) (string, error) {
	t.Helper()
	conn, id, err := sb.DialFor(context.Background(), "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	return id, err
}

// TestStickyWinnerSingleHost confirms repeated DialFor calls for the same
// host route to the same upstream when only that upstream succeeds. Also
// checks that the loser's dial count stays at zero because Pick never tries
// it.
func TestStickyWinnerSingleHost(t *testing.T) {
	t.Parallel()
	winner := alwaysOK("winner")
	loser1 := alwaysOK("loser1") // would succeed, but lower priority
	loser2 := alwaysOK("loser2")
	sb := buildScoreboard(t, []*fakeUpstream{winner, loser1, loser2}, scoreboard.Config{}, nil, 1)
	defer sb.Stop()

	for i := 0; i < 10; i++ {
		id, err := dialOnce(t, sb, "example.com:443")
		if err != nil {
			t.Fatalf("iteration %d: dial failed: %v", i, err)
		}
		if id != "winner" {
			t.Fatalf("iteration %d: served by %q, want winner", i, id)
		}
	}
	if got := winner.dialCount.Load(); got != 10 {
		t.Errorf("winner dialCount = %d, want 10", got)
	}
	// Losers must not have been dialed because winner always succeeded
	// and pop'd the first slot. Probe is disabled (ProbeChance default 0).
	if got := loser1.dialCount.Load(); got != 0 {
		t.Errorf("loser1 dialCount = %d, want 0", got)
	}
	if got := loser2.dialCount.Load(); got != 0 {
		t.Errorf("loser2 dialCount = %d, want 0", got)
	}
}

// TestCachedWinnerRefusalPromotesNext confirms the scoreboard advances to
// the next-best upstream when the cached winner refuses, and that the loser
// receives a cooldown that hides it from subsequent picks.
func TestCachedWinnerRefusalPromotesNext(t *testing.T) {
	t.Parallel()
	bad := alwaysRefused("bad")
	good := alwaysOK("good")
	clock := newManualClock(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	sb := buildScoreboard(t, []*fakeUpstream{bad, good}, scoreboard.Config{}, clock, 1)
	defer sb.Stop()

	// First dial: bad refuses, good succeeds. Both get touched.
	id, err := dialOnce(t, sb, "example.com:443")
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	if id != "good" {
		t.Fatalf("first dial served by %q, want good", id)
	}
	if bad.dialCount.Load() != 1 {
		t.Errorf("bad dialCount = %d, want 1", bad.dialCount.Load())
	}
	if good.dialCount.Load() != 1 {
		t.Errorf("good dialCount = %d, want 1", good.dialCount.Load())
	}
	// Verify the cooldown landed on bad.
	snap := snapshotByID(sb, "example.com")
	if e, ok := snap["bad"]; !ok {
		t.Error("bad has no scoreboard entry")
	} else if !e.CooldownUntil.After(clock.Now()) {
		t.Errorf("bad cooldownUntil = %v, want after now (%v)", e.CooldownUntil, clock.Now())
	}

	// Second dial: bad is on cooldown, so Pick skips it entirely. good
	// serves directly. bad.dialCount must NOT increase.
	id, err = dialOnce(t, sb, "example.com:443")
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	if id != "good" {
		t.Fatalf("second dial served by %q, want good", id)
	}
	if got := bad.dialCount.Load(); got != 1 {
		t.Errorf("bad dialCount after second dial = %d, want 1 (cooldown should have skipped it)", got)
	}
	if got := good.dialCount.Load(); got != 2 {
		t.Errorf("good dialCount after second dial = %d, want 2", got)
	}
}

// TestCascadeShortCircuitsAfterFullFailure confirms that when every upstream
// fails on the same request, the next request for that host returns
// ErrCascadeCooling without touching the pool again. After the cascade
// expires, requests resume.
func TestCascadeShortCircuitsAfterFullFailure(t *testing.T) {
	t.Parallel()
	bad1 := alwaysRefused("bad1")
	bad2 := alwaysRefused("bad2")
	bad3 := alwaysRefused("bad3")
	clock := newManualClock(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	sb := buildScoreboard(t, []*fakeUpstream{bad1, bad2, bad3}, scoreboard.Config{
		CascadeTTL: 30 * time.Second,
	}, clock, 1)
	defer sb.Stop()

	// First dial walks all three, fails, trips cascade.
	if _, err := dialOnce(t, sb, "example.com:443"); err == nil {
		t.Fatal("first dial returned nil err; want failure")
	}
	expectedDials := int64(1)
	for _, u := range []*fakeUpstream{bad1, bad2, bad3} {
		if got := u.dialCount.Load(); got != expectedDials {
			t.Errorf("%s dialCount after first dial = %d, want %d", u.ID(), got, expectedDials)
		}
	}

	// Second dial within the cascade TTL must NOT touch any upstream.
	_, err := dialOnce(t, sb, "example.com:443")
	if err == nil {
		t.Fatal("second dial during cascade returned nil err")
	}
	var cascadeErr *scoreboard.CascadeError
	if !errors.As(err, &cascadeErr) {
		t.Fatalf("second dial err = %v, want *CascadeError", err)
	}
	if cascadeErr.Host != "example.com" {
		t.Errorf("CascadeError.Host = %q, want example.com", cascadeErr.Host)
	}
	if !errors.Is(err, scoreboard.ErrCascadeCooling) {
		t.Error("errors.Is(err, ErrCascadeCooling) = false, want true")
	}
	for _, u := range []*fakeUpstream{bad1, bad2, bad3} {
		if got := u.dialCount.Load(); got != expectedDials {
			t.Errorf("%s dialCount during cascade = %d, want %d (no new dials)", u.ID(), got, expectedDials)
		}
	}

	// Advance past cascade TTL: a fresh dial walks the pool again.
	clock.Add(31 * time.Second)
	_, err = dialOnce(t, sb, "example.com:443")
	if err == nil {
		t.Fatal("post-cascade dial succeeded; pool is all-bad")
	}
	if errors.Is(err, scoreboard.ErrCascadeCooling) {
		t.Error("post-cascade dial returned cascade error; cascade should have expired")
	}
}

// TestCascadeClearsOnSuccess confirms a successful request ends the
// cascade-cooling early so the host re-enters normal behavior immediately.
func TestCascadeClearsOnSuccess(t *testing.T) {
	t.Parallel()
	bad := alwaysRefused("bad")
	sb := buildScoreboard(t, []*fakeUpstream{bad}, scoreboard.Config{
		CascadeTTL: time.Hour, // long, so the test must clear it actively
	}, nil, 1)
	defer sb.Stop()

	_, err := dialOnce(t, sb, "example.com:443")
	if err == nil {
		t.Fatal("expected first dial to fail")
	}
	// Manually record a success: the cascade must clear.
	sb.RecordSuccess("example.com", "bad", 5*time.Millisecond)
	if !sb.CascadeUntil("example.com").IsZero() {
		t.Error("CascadeUntil non-zero after RecordSuccess; should be cleared")
	}
}

// TestRecoveryProbe confirms the probe-chance path picks a non-top eligible
// candidate when the random draw wins. With ProbeChance=1, the next Pick
// after a winner is established must promote the penalized candidate.
//
// The test asserts the single-step probe behavior rather than a long
// sequence: as soon as the probed candidate succeeds, its score climbs and
// the "non-top" identity flips, so a multi-iteration assertion of
// "always probed" would be wrong by design.
func TestRecoveryProbe(t *testing.T) {
	t.Parallel()
	winner := alwaysOK("winner")
	probed := alwaysOK("probed")
	clock := newManualClock(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	sb := buildScoreboard(t, []*fakeUpstream{winner, probed}, scoreboard.Config{
		ProbeChance: 1.0, // always probe
	}, clock, 42)
	defer sb.Stop()

	// Push winner above probed by recording a success on it. Without this
	// step, scores tie and probed (priority 20) is the only non-top
	// candidate by tiebreak alone.
	sb.RecordSuccess("example.com", "winner", 5*time.Millisecond)
	snapBefore := snapshotByID(sb, "example.com")
	if got := snapBefore["winner"].Score; got <= 0 {
		t.Fatalf("winner pre-probe score = %v, want > 0", got)
	}

	// Single dial: with ProbeChance=1 the probe roll always wins, so the
	// non-top eligible candidate (probed) gets the dial even though winner
	// has the higher score.
	id, err := dialOnce(t, sb, "example.com:443")
	if err != nil {
		t.Fatalf("probe dial: %v", err)
	}
	if id != "probed" {
		t.Fatalf("probe dial served by %q, want probed (winner outscores it; the probe roll should pick the non-top)", id)
	}

	// probed's score should now exceed its starting zero, courtesy of the
	// successful probe dial. This is the recovery path the proposal calls
	// out: a probed upstream that succeeds climbs back into rotation.
	snap := snapshotByID(sb, "example.com")
	if e, ok := snap["probed"]; !ok {
		t.Fatal("probed has no scoreboard entry")
	} else if e.Score <= 0 {
		t.Errorf("probed score = %v, want > 0 after a probe success", e.Score)
	}
}

// TestTimeDecayDriftsScoresTowardZero confirms the decay goroutine reduces
// positive scores toward zero over time. Uses a 5ms decay interval and
// polls Snapshot to observe drift, capping the wait at 2s so the test
// stays fast even on a busy CI runner.
func TestTimeDecayDriftsScoresTowardZero(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sb := buildScoreboard(t, []*fakeUpstream{alwaysOK("w")}, scoreboard.Config{
		DecayInterval: 5 * time.Millisecond,
		DecayStep:     1.0,
		ScoreCap:      5,
	}, nil, 1)
	defer sb.Stop()
	sb.Start(ctx)
	for i := 0; i < 5; i++ {
		sb.RecordSuccess("example.com", "w", 0)
	}
	if got := snapshotByID(sb, "example.com")["w"].Score; got != 5 {
		t.Fatalf("initial score = %v, want 5", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := snapshotByID(sb, "example.com")["w"].Score; got <= 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("score did not decay to zero within 2s; final = %v",
		snapshotByID(sb, "example.com")["w"].Score)
}

// TestPerHostIndependence confirms a penalty against host A does not affect
// host B's view of the same upstream.
func TestPerHostIndependence(t *testing.T) {
	t.Parallel()
	up1 := alwaysOK("u1")
	up2 := alwaysOK("u2")
	sb := buildScoreboard(t, []*fakeUpstream{up1, up2}, scoreboard.Config{}, nil, 1)
	defer sb.Stop()

	// Penalize u1 hard for hostA only.
	sb.RecordFailure("hostA", "u1", failure.KindRefused, 0)
	sb.RecordFailure("hostA", "u1", failure.KindRefused, 0)
	// Wait past debounce window and apply a third penalty so the score
	// drops below zero conclusively. Use the underlying clock... actually
	// without an injected clock, just rely on real time.
	time.Sleep(150 * time.Millisecond)
	sb.RecordFailure("hostA", "u1", failure.KindRefused, 0)

	snapA := snapshotByID(sb, "hostA")
	if e, ok := snapA["u1"]; !ok {
		t.Fatal("hostA has no entry for u1")
	} else if e.Score >= 0 {
		t.Errorf("hostA u1 score = %v, want < 0 after penalties", e.Score)
	}
	// hostB has no entry for u1, so its view is unpenalized.
	snapB := snapshotByID(sb, "hostB")
	if _, ok := snapB["u1"]; ok {
		t.Error("hostB should have no entry for u1 yet (no failures recorded)")
	}
}

// TestCooldownExpiry confirms a cooled upstream re-enters rotation once the
// cooldown clock advances past its expiry, with score still intact.
func TestCooldownExpiry(t *testing.T) {
	t.Parallel()
	up := alwaysOK("u1")
	clock := newManualClock(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	sb := buildScoreboard(t, []*fakeUpstream{up}, scoreboard.Config{
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 1, Cooldown: 10 * time.Second},
		},
	}, clock, 1)
	defer sb.Stop()

	// Record a failure: cooldown of 10s lands.
	sb.RecordFailure("example.com", "u1", failure.KindRefused, 0)
	snap := snapshotByID(sb, "example.com")
	if e, ok := snap["u1"]; !ok {
		t.Fatal("no entry for u1 after RecordFailure")
	} else if !e.CooldownUntil.After(clock.Now()) {
		t.Fatalf("CooldownUntil = %v, want after now (%v)", e.CooldownUntil, clock.Now())
	}

	// Advance past cooldown. u1 should serve the next dial.
	clock.Add(11 * time.Second)
	id, err := dialOnce(t, sb, "example.com:443")
	if err != nil {
		t.Fatalf("dial after cooldown expiry: %v", err)
	}
	if id != "u1" {
		t.Fatalf("post-cooldown dial served by %q, want u1", id)
	}
}

// TestFailureDebounce confirms 10 concurrent identical RecordFailure calls
// produce exactly one penalty event, not 10. Score after the burst should
// be -policy.Penalty * 1, not -policy.Penalty * 10. globalFailureCount
// likewise records the single applied event.
func TestFailureDebounce(t *testing.T) {
	t.Parallel()
	up := alwaysOK("u1")
	sb := buildScoreboard(t, []*fakeUpstream{up}, scoreboard.Config{
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 3, Cooldown: 30 * time.Second},
		},
		DebounceWindow: 100 * time.Millisecond,
	}, nil, 1)
	defer sb.Stop()

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			sb.RecordFailure("example.com", "u1", failure.KindRefused, 0)
		}()
	}
	close(start)
	wg.Wait()

	snap := snapshotByID(sb, "example.com")
	e, ok := snap["u1"]
	if !ok {
		t.Fatal("no entry for u1")
	}
	if e.Score != -3 {
		t.Errorf("score = %v, want -3 (one penalty event of -3, not 10)", e.Score)
	}
	if e.GlobalFailure != 1 {
		t.Errorf("globalFailure = %d, want 1", e.GlobalFailure)
	}
}

// TestDeterministicSeed confirms two scoreboards built with identical state
// and identical seeds produce identical Pick sequences. Verifies the
// injectable random source is the only source of probe-roll randomness.
func TestDeterministicSeed(t *testing.T) {
	t.Parallel()
	pickSequence := func(seed int64) []string {
		ups := []*fakeUpstream{alwaysOK("a"), alwaysOK("b"), alwaysOK("c")}
		sb := buildScoreboard(t, ups, scoreboard.Config{
			ProbeChance: 0.5, // mid-range so seed selection actually matters
		}, nil, seed)
		defer sb.Stop()
		// Make all three eligible by recording one success on each so they
		// have non-zero entries; tied scores fall through to base priority,
		// so probe rolls are the only source of variance.
		for _, id := range []string{"a", "b", "c"} {
			sb.RecordSuccess("example.com", id, 0)
		}
		out := make([]string, 50)
		for i := range out {
			id, err := dialOnce(t, sb, "example.com:443")
			if err != nil {
				t.Fatalf("dial %d: %v", i, err)
			}
			out[i] = id
		}
		return out
	}
	a := pickSequence(123)
	b := pickSequence(123)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sequences diverged at index %d: %q vs %q", i, a[i], b[i])
		}
	}
	c := pickSequence(124)
	allEqual := true
	for i := range a {
		if a[i] != c[i] {
			allEqual = false
			break
		}
	}
	if allEqual {
		t.Error("seeds 123 and 124 produced identical sequences; the seed is not actually wired")
	}
}

// snapshotByID indexes the scoreboard's snapshot of host by upstream id.
// Tests use it to assert per-entry properties without iterating the slice
// at the call site.
func snapshotByID(sb *scoreboard.Scoreboard, host string) map[string]scoreboard.EntrySnapshot {
	out := make(map[string]scoreboard.EntrySnapshot)
	for _, e := range sb.Snapshot() {
		if e.Host == host {
			out[e.UpstreamID] = e
		}
	}
	return out
}

// TestDialForSkipsScoringOnCallerCancellation confirms the dial path does
// not penalize an upstream when the caller's context cut the dial short.
// Both Canceled (client hung up) and DeadlineExceeded (caller's request
// deadline) must skip RecordFailure: the failure reflects the caller
// bailing, not the upstream being unable to reach the host.
func TestDialForSkipsScoringOnCallerCancellation(t *testing.T) {
	t.Parallel()

	t.Run("context.Canceled", func(t *testing.T) {
		t.Parallel()
		up := alwaysOK("u1") // dial body never runs because ctx is already done
		sb := buildScoreboard(t, []*fakeUpstream{up}, scoreboard.Config{}, nil, 1)
		defer sb.Stop()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		conn, _, err := sb.DialFor(ctx, "tcp", "example.com:443")
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil {
			t.Fatal("DialFor with cancelled ctx returned nil err")
		}
		// Pre-cancellation case: Pick is never called, so no entry exists.
		// What matters is no penalty was recorded.
		snap := snapshotByID(sb, "example.com")
		if e, ok := snap["u1"]; ok && e.Score < 0 {
			t.Errorf("u1 score = %v, want >= 0 (no penalty for caller cancellation)", e.Score)
		}
	})

	t.Run("context.DeadlineExceeded mid-dial", func(t *testing.T) {
		t.Parallel()
		// Dial blocks until ctx fires, then returns ctx.Err(). Mirrors
		// what the real *net.Dialer does when ctx.Deadline elapses.
		up := newFakeUpstream("u1", func() (net.Conn, error) {
			panic("unreachable") // ctx-aware Dial returns before behavior runs
		})
		// Patch the fake to honor ctx.
		up.behavior = func() (net.Conn, error) {
			// Sleep long enough for the deadline to fire.
			time.Sleep(50 * time.Millisecond)
			return nil, context.DeadlineExceeded
		}
		sb := buildScoreboard(t, []*fakeUpstream{up}, scoreboard.Config{}, nil, 1)
		defer sb.Stop()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		conn, _, err := sb.DialFor(ctx, "tcp", "example.com:443")
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil {
			t.Fatal("DialFor with deadline-exceeded ctx returned nil err")
		}
		snap := snapshotByID(sb, "example.com")
		if e, ok := snap["u1"]; ok && e.Score < 0 {
			t.Errorf("u1 score = %v, want >= 0 (no penalty for caller deadline)", e.Score)
		}
		if e, ok := snap["u1"]; ok && e.GlobalFailure > 0 {
			t.Errorf("u1 globalFailure = %d, want 0", e.GlobalFailure)
		}
	})
}

// TestSnapshotIsStable is a small sanity check that Snapshot returns a
// stable list (not a map iteration order test, just that the call works
// after a few records).
func TestSnapshotIsStable(t *testing.T) {
	t.Parallel()
	up := alwaysOK("u1")
	sb := buildScoreboard(t, []*fakeUpstream{up}, scoreboard.Config{}, nil, 1)
	defer sb.Stop()
	sb.RecordSuccess("hostA", "u1", 0)
	sb.RecordSuccess("hostB", "u1", 0)
	snaps := sb.Snapshot()
	if len(snaps) != 2 {
		t.Fatalf("len(snapshot) = %d, want 2", len(snaps))
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Host < snaps[j].Host })
	if snaps[0].Host != "hostA" || snaps[1].Host != "hostB" {
		t.Errorf("hosts = %s, %s; want hostA, hostB", snaps[0].Host, snaps[1].Host)
	}
}
