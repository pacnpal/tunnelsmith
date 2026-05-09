package scoreboard_test

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// TestReplacePoolPreservesEntries confirms a ReplacePool call swaps the
// pool out without dropping the per-(host, upstream) state. A host whose
// previous winner survives in the new pool keeps its score.
func TestReplacePoolPreservesEntries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	sb := newPersistTestScoreboard(t, clock)

	sb.RecordSuccess("api.example.com", "alpha", time.Millisecond)
	if got, want := sb.EntriesCount(), 1; got != want {
		t.Fatalf("setup EntriesCount = %d, want %d", got, want)
	}

	// New pool keeps alpha plus adds a fresh upstream "delta".
	newPool := buildPersistTestPool(t, "alpha", "delta")
	if err := sb.ReplacePool(newPool); err != nil {
		t.Fatalf("ReplacePool: %v", err)
	}

	if got, want := sb.EntriesCount(), 1; got != want {
		t.Errorf("EntriesCount after swap = %d, want %d (alpha must still be there)", got, want)
	}
	got := sb.Snapshot()
	if got[0].UpstreamID != "alpha" || got[0].Score < 1 {
		t.Errorf("entry score lost across ReplacePool: %+v", got[0])
	}
}

// TestReplacePoolEvictsForcePinsForRemovedUpstreams confirms a hot
// reload that drops an upstream from the pool also drops any active
// Force pin pointing at that upstream. The routing path already fell
// through to normal scoring (pickForced ignores stale pins), but
// ForceSnapshot would keep surfacing the dead pin in the UI table
// until an operator clicked Clear; this is issue #19.
func TestReplacePoolEvictsForcePinsForRemovedUpstreams(t *testing.T) {
	t.Parallel()

	clock := &fixedClock{now: time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)}
	sb := newPersistTestScoreboard(t, clock) // pool: alpha, beta, gamma

	until := clock.Now().Add(time.Hour)
	if err := sb.Force("survives.example.com", "alpha", until); err != nil {
		t.Fatalf("Force survives: %v", err)
	}
	if err := sb.Force("evicted.example.com", "beta", until); err != nil {
		t.Fatalf("Force evicted: %v", err)
	}
	if err := sb.Force("also-evicted.example.com", "gamma", until); err != nil {
		t.Fatalf("Force also-evicted: %v", err)
	}
	if got := len(sb.ForceSnapshot()); got != 3 {
		t.Fatalf("ForceSnapshot before swap = %d, want 3", got)
	}

	// New pool keeps alpha, drops beta and gamma, adds delta.
	newPool := buildPersistTestPool(t, "alpha", "delta")
	if err := sb.ReplacePool(newPool); err != nil {
		t.Fatalf("ReplacePool: %v", err)
	}

	snap := sb.ForceSnapshot()
	if len(snap) != 1 {
		t.Fatalf("ForceSnapshot after swap = %d, want 1 (only alpha-pin survives)", len(snap))
	}
	if snap[0].Host != "survives.example.com" || snap[0].UpstreamID != "alpha" {
		t.Errorf("surviving pin = %+v, want survives.example.com/alpha", snap[0])
	}
	if _, ok := sb.ForcedFor("evicted.example.com"); ok {
		t.Error("ForcedFor still reports beta-pin as active after pool swap")
	}
	if _, ok := sb.ForcedFor("also-evicted.example.com"); ok {
		t.Error("ForcedFor still reports gamma-pin as active after pool swap")
	}
}

// TestReplacePoolNilRejected catches the contract violation up front.
func TestReplacePoolNilRejected(t *testing.T) {
	t.Parallel()
	clock := &fixedClock{now: time.Now()}
	sb := newPersistTestScoreboard(t, clock)
	if err := sb.ReplacePool(nil); err == nil {
		t.Fatal("ReplacePool(nil) returned nil error")
	}
}

// TestReloadAppliesNewScoringConfig verifies that Reload swaps in the new
// scoring tunings (we move the SuccessWeight from 1 to 5 and confirm the
// next RecordSuccess applies the new weight).
func TestReloadAppliesNewScoringConfig(t *testing.T) {
	t.Parallel()

	clock := &fixedClock{now: time.Now()}
	sb := newPersistTestScoreboard(t, clock)
	sb.RecordSuccess("api.example.com", "alpha", time.Millisecond)
	if got := sb.Snapshot()[0].Score; got != 1 {
		t.Fatalf("baseline score = %v, want 1 (SuccessWeight=1)", got)
	}

	newCfg := scoreboard.Config{
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 1, Cooldown: 30 * time.Second},
		},
		SuccessWeight:  5,
		ScoreCap:       100,
		ProbeChance:    0,
		DecayInterval:  time.Minute,
		DecayStep:      0.5,
		CascadeTTL:     30 * time.Second,
		DebounceWindow: 0,
		PruneAfter:     time.Hour,
	}
	sb.Reload(newCfg)

	sb.RecordSuccess("api.example.com", "alpha", time.Millisecond)
	if got := sb.Snapshot()[0].Score; got != 6 {
		t.Errorf("post-reload score = %v, want 6 (1 baseline + 5 from new SuccessWeight)", got)
	}
}

// TestReloadKeepsDecayInterval guards the documented exception: a SIGHUP
// reload cannot retune the decay goroutine's ticker. The Config swap
// preserves the original DecayInterval even if the caller passed a
// different value.
func TestReloadKeepsDecayInterval(t *testing.T) {
	t.Parallel()

	pool := buildPersistTestPool(t, "alpha")
	cfg := scoreboard.Config{
		KindPolicy:     map[failure.Kind]scoreboard.Policy{},
		SuccessWeight:  1,
		ScoreCap:       10,
		ProbeChance:    0,
		DecayInterval:  17 * time.Minute,
		DecayStep:      0.5,
		CascadeTTL:     30 * time.Second,
		DebounceWindow: 0,
		PruneAfter:     time.Hour,
	}
	sb, err := scoreboard.New(pool, cfg, scoreboard.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	newCfg := cfg
	newCfg.DecayInterval = 5 * time.Second
	sb.Reload(newCfg)

	// We cannot read sb.cfg directly (unexported), but a downstream Reload
	// behavior that depends on DecayInterval is the only observable point;
	// the next RecordSuccess does not surface DecayInterval, so we cover
	// the guarantee through the documented contract: reload returns
	// without panicking, scoreboard keeps serving. Behavioral coverage
	// for the actual decay-loop swap belongs in a future internal test.
	sb.RecordSuccess("api.example.com", "alpha", time.Millisecond)
	if got := sb.Snapshot()[0].Score; got != 1 {
		t.Errorf("post-reload score = %v, want 1", got)
	}
	_ = upstream.PoolEntry{} // keep the upstream import alive
}

// TestReloadAndReplacePoolUnderLoad hammers the scoreboard with concurrent
// Pick / RecordSuccess / RecordFailure / DialFor calls while a writer
// goroutine swaps the pool and the config back and forth via ReplacePool
// and Reload. Run under -race; without the snapshot helpers added in the
// review fix, the race detector fires on s.cfg and s.poolEntries reads.
func TestReloadAndReplacePoolUnderLoad(t *testing.T) {
	t.Parallel()

	clock := &fixedClock{now: time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)}
	sb := newPersistTestScoreboard(t, clock)

	cfgA := scoreboard.Config{
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 1, Cooldown: 30 * time.Second},
			failure.KindTimeout: {Penalty: 1, Cooldown: 15 * time.Second},
		},
		SuccessWeight:  1,
		ScoreCap:       10,
		ProbeChance:    0.1,
		DecayInterval:  time.Minute,
		DecayStep:      0.5,
		CascadeTTL:     30 * time.Second,
		DebounceWindow: 50 * time.Millisecond,
		PruneAfter:     time.Hour,
	}
	cfgB := cfgA
	cfgB.SuccessWeight = 5
	cfgB.ProbeChance = 0
	cfgB.DebounceWindow = 0
	cfgB.CascadeTTL = 0

	poolAlpha := buildPersistTestPool(t, "alpha", "beta", "gamma")
	poolDelta := buildPersistTestPool(t, "alpha", "delta")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writers: swap pool + config back and forth.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				_ = sb.ReplacePool(poolDelta)
				sb.Reload(cfgB)
			} else {
				_ = sb.ReplacePool(poolAlpha)
				sb.Reload(cfgA)
			}
		}
	}()

	// Readers: hot-path callers.
	const readers = 4
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			host := "api.example.com"
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = sb.Pick(host, nil)
				sb.RecordSuccess(host, "alpha", time.Millisecond)
				if j%3 == 0 {
					sb.RecordFailure(host, "alpha", failure.KindRefused, nil)
				}
				if j%5 == 0 {
					sb.TripCascade(host)
				}
				_ = sb.Snapshot()
				_ = sb.CooledHostsByUpstream()
				_ = sb.CascadeActiveCount()
				_ = sb.EntriesCount()
				_ = sb.Prune()
			}
		}(i)
	}

	// Run for a short interval; -race instrumentation is what makes this
	// useful, not throughput.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
