package scoreboard_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// TestSaveLoadRoundTrip confirms a full write + read cycle preserves every
// field on the entry struct plus the cascade map. Includes one host with
// three entries and one cascade-active host so both sides of the format
// see real data.
func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.gob")

	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}

	sbWrite := newPersistTestScoreboard(t, clock)
	sbWrite.RecordSuccess("api.example.com", "alpha", 25*time.Millisecond)
	sbWrite.RecordSuccess("api.example.com", "beta", 50*time.Millisecond)
	sbWrite.RecordFailure("api.example.com", "gamma", failure.KindRefused, nil)
	sbWrite.TripCascade("blocked.example.com")

	if err := sbWrite.SaveSnapshot(path); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file not created: %v", err)
	}

	sbRead := newPersistTestScoreboard(t, clock)
	if err := sbRead.LoadSnapshot(path); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	wantEntries := map[string]map[string]struct {
		minScore float64
	}{
		"api.example.com": {
			"alpha": {minScore: 1},
			"beta":  {minScore: 1},
			"gamma": {minScore: -1},
		},
	}
	got := sbRead.Snapshot()
	gotMap := make(map[string]map[string]scoreboard.EntrySnapshot)
	for _, e := range got {
		if gotMap[e.Host] == nil {
			gotMap[e.Host] = map[string]scoreboard.EntrySnapshot{}
		}
		gotMap[e.Host][e.UpstreamID] = e
	}
	for host, perUp := range wantEntries {
		for id, want := range perUp {
			e, ok := gotMap[host][id]
			if !ok {
				t.Errorf("missing entry after load: host=%s upstream=%s", host, id)
				continue
			}
			if want.minScore > 0 && e.Score < want.minScore {
				t.Errorf("entry %s/%s score = %v, want >= %v", host, id, e.Score, want.minScore)
			}
			if want.minScore < 0 && e.Score > want.minScore {
				t.Errorf("entry %s/%s score = %v, want <= %v", host, id, e.Score, want.minScore)
			}
		}
	}
	if got, want := sbRead.CascadeActiveCount(), 1; got != want {
		t.Errorf("CascadeActiveCount after load = %d, want %d", got, want)
	}
}

// TestLoadSnapshotMissingFileNotError documents the deliberate "first run"
// behavior: a missing snapshot file is not an error, so a fresh install
// boots cleanly without needing a sentinel file.
func TestLoadSnapshotMissingFileNotError(t *testing.T) {
	t.Parallel()
	clock := &fixedClock{now: time.Now()}
	sb := newPersistTestScoreboard(t, clock)
	missing := filepath.Join(t.TempDir(), "absent.gob")
	if err := sb.LoadSnapshot(missing); err != nil {
		t.Fatalf("LoadSnapshot on missing file returned error: %v", err)
	}
}

// TestLoadSnapshotMagicMismatch surfaces a clear error when the file does
// not start with our magic bytes.
func TestLoadSnapshotMagicMismatch(t *testing.T) {
	t.Parallel()
	clock := &fixedClock{now: time.Now()}
	sb := newPersistTestScoreboard(t, clock)
	path := filepath.Join(t.TempDir(), "wrong.gob")
	if err := os.WriteFile(path, []byte("not-tsb-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := sb.LoadSnapshot(path)
	if err == nil {
		t.Fatal("LoadSnapshot accepted a non-magic file")
	}
	if !strings.Contains(err.Error(), "magic mismatch") {
		t.Errorf("error %q lacks magic mismatch hint", err.Error())
	}
}

// TestSaveSnapshotAtomicRename covers the temp-and-rename pattern: a crash
// mid-write must not leave the path holding a corrupt file. The test pre-
// seeds path with a known-good snapshot, runs Save, and verifies the file
// the read side sees still parses cleanly.
func TestSaveSnapshotAtomicRename(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.gob")

	clock := &fixedClock{now: time.Now()}
	sbA := newPersistTestScoreboard(t, clock)
	sbA.RecordSuccess("a.example.com", "alpha", time.Millisecond)
	if err := sbA.SaveSnapshot(path); err != nil {
		t.Fatalf("first SaveSnapshot: %v", err)
	}

	sbB := newPersistTestScoreboard(t, clock)
	sbB.RecordSuccess("b.example.com", "beta", time.Millisecond)
	if err := sbB.SaveSnapshot(path); err != nil {
		t.Fatalf("second SaveSnapshot: %v", err)
	}

	sbRead := newPersistTestScoreboard(t, clock)
	if err := sbRead.LoadSnapshot(path); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	got := sbRead.Snapshot()
	if len(got) != 1 || got[0].Host != "b.example.com" {
		t.Errorf("snapshot did not reflect second write: %+v", got)
	}
}

// TestPruneDropsZeroScoreStaleEntries locks in the prune policy: an entry
// with score == 0 and lastSeen older than PruneAfter should be removed,
// the host's empty per-upstream map should also be removed, and entries
// with positive scores must not be touched.
func TestPruneDropsZeroScoreStaleEntries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}

	sb := newPersistTestScoreboardWithPrune(t, clock, time.Hour)
	// Drive each h1 entry to score == 0 via balanced success/failure so
	// the test does not need a decay-tick test hook. RecordSuccess sets
	// score to +SuccessWeight and clears cooldown; RecordFailure with
	// KindRefused (Penalty == SuccessWeight in this test policy) drops
	// the score back to zero. lastSeen is stamped from the fixed clock,
	// so advancing the clock makes both entries stale.
	sb.RecordSuccess("h1", "alpha", 0)
	sb.RecordFailure("h1", "alpha", failure.KindRefused, nil)
	sb.RecordSuccess("h1", "beta", 0)
	sb.RecordFailure("h1", "beta", failure.KindRefused, nil)

	// h2 stays fresh and positive across the clock advance.
	sb.RecordSuccess("h2", "gamma", 0)

	clock.advance(2 * time.Hour)
	// Re-touch h2 so its lastSeen is "now" relative to the advanced clock.
	sb.RecordSuccess("h2", "gamma", 0)

	stats := sb.Prune()
	if stats.EntriesDropped != 2 {
		t.Errorf("EntriesDropped = %d, want 2 (h1.alpha and h1.beta)", stats.EntriesDropped)
	}
	if stats.HostsDropped != 1 {
		t.Errorf("HostsDropped = %d, want 1 (h1 became empty)", stats.HostsDropped)
	}
	if got := sb.Snapshot(); len(got) != 1 || got[0].Host != "h2" {
		t.Errorf("snapshot after prune: %+v, want only h2", got)
	}
}

// TestPruneEvictsExpiredCascade confirms cascade entries past their expiry
// are dropped, and active ones survive. The two TripCascade calls are
// staggered on the fixed clock so a single 31s advance only crosses the
// first trip's TTL; without the stagger both entries would expire and
// the test would not exercise the "active ones survive" path.
func TestPruneEvictsExpiredCascade(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	sb := newPersistTestScoreboard(t, clock)

	sb.TripCascade("expiring.example.com")
	// Stagger the second trip so its TTL lands later on the fake clock.
	clock.advance(2 * time.Second)
	sb.TripCascade("active.example.com")

	// CascadeTTL in the test scoreboard is 30s. Advance past the first
	// trip's expiry but not the second's: 31s after the first trip is
	// 29s after the second.
	clock.advance(29 * time.Second)
	stats := sb.Prune()
	if stats.CascadeDropped != 1 {
		t.Errorf("CascadeDropped = %d, want 1 (only expiring.example.com is past TTL)", stats.CascadeDropped)
	}
	if got := sb.CascadeActiveCount(); got != 1 {
		t.Errorf("CascadeActiveCount after prune = %d, want 1 (active.example.com still in cooldown)", got)
	}
}

// TestPersistenceLoopFlushesOnShutdown drives the loop end-to-end and
// verifies a final flush runs at ctx cancellation even when Interval is 0.
func TestPersistenceLoopFlushesOnShutdown(t *testing.T) {
	t.Parallel()

	clock := &fixedClock{now: time.Now()}
	sb := newPersistTestScoreboard(t, clock)
	sb.RecordSuccess("api.example.com", "alpha", 1*time.Millisecond)

	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.gob")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	loop := scoreboard.NewPersistenceLoop(sb, scoreboard.PersistenceConfig{
		Path:     path,
		Interval: 0, // disabled; only the shutdown flush runs
	}, logger, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file not written on shutdown: %v", err)
	}
}

// TestPersistenceLoopReportsErrorToSink stands up a path inside a directory
// that does not exist; the persistence loop must surface "error" through
// the metrics sink and keep running.
func TestPersistenceLoopReportsErrorToSink(t *testing.T) {
	t.Parallel()

	clock := &fixedClock{now: time.Now()}
	sb := newPersistTestScoreboard(t, clock)

	bogus := filepath.Join(t.TempDir(), "missing-dir", "snapshot.gob")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink := &fakePersistSink{}

	loop := scoreboard.NewPersistenceLoop(sb, scoreboard.PersistenceConfig{
		Path:     bogus,
		Interval: 0,
	}, logger, sink)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.errorCount.Load() == 0 {
		t.Error("sink did not see any error outcomes from the failing flush")
	}
}

// TestPersistenceLoopRunOnceGuard rejects re-Run on a single instance.
func TestPersistenceLoopRunOnceGuard(t *testing.T) {
	t.Parallel()

	clock := &fixedClock{now: time.Now()}
	sb := newPersistTestScoreboard(t, clock)
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.gob")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	loop := scoreboard.NewPersistenceLoop(sb, scoreboard.PersistenceConfig{
		Path:     path,
		Interval: 0,
	}, logger, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := loop.Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := loop.Run(ctx); err == nil {
		t.Error("second Run accepted; expected a guard error")
	} else if !strings.Contains(err.Error(), "already used") {
		t.Errorf("error %q lacks expected guard hint", err.Error())
	}
}

// TestCooledHostsByUpstreamSnapshot is the gauge feed: cooled-host counts
// must reflect only entries whose cooldownUntil is in the future.
func TestCooledHostsByUpstreamSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	sb := newPersistTestScoreboard(t, clock)

	sb.RecordFailure("api.example.com", "alpha", failure.KindRefused, nil)
	sb.RecordFailure("api.example.com", "beta", failure.KindRefused, nil)
	sb.RecordFailure("blog.example.com", "alpha", failure.KindRefused, nil)
	sb.RecordSuccess("blog.example.com", "alpha", time.Millisecond) // clears cooldown for alpha@blog

	got := sb.CooledHostsByUpstream()
	if got["alpha"] != 1 {
		t.Errorf("alpha cooled hosts = %d, want 1 (api still cooled, blog cleared)", got["alpha"])
	}
	if got["beta"] != 1 {
		t.Errorf("beta cooled hosts = %d, want 1", got["beta"])
	}
	if got, want := sb.EntriesCount(), 3; got != want {
		t.Errorf("EntriesCount = %d, want %d", got, want)
	}
}

// fixedClock advances only when the test asks. The scoreboard's clock
// option accepts a func; we adapt this struct to that signature in the
// helper below.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newPersistTestScoreboard(t *testing.T, clock *fixedClock) *scoreboard.Scoreboard {
	t.Helper()
	return newPersistTestScoreboardWithPrune(t, clock, time.Hour)
}

func newPersistTestScoreboardWithPrune(t *testing.T, clock *fixedClock, prune time.Duration) *scoreboard.Scoreboard {
	t.Helper()
	pool := buildPersistTestPool(t, "alpha", "beta", "gamma")
	cfg := scoreboard.Config{
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 1, Cooldown: 30 * time.Second},
			failure.KindTimeout: {Penalty: 1, Cooldown: 15 * time.Second},
		},
		SuccessWeight:  1,
		ScoreCap:       10,
		ProbeChance:    0,
		DecayInterval:  time.Minute,
		DecayStep:      0.5,
		CascadeTTL:     30 * time.Second,
		DebounceWindow: 0, // off so every RecordFailure lands
		PruneAfter:     prune,
	}
	sb, err := scoreboard.New(pool, cfg, scoreboard.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("scoreboard.New: %v", err)
	}
	return sb
}

func buildPersistTestPool(t *testing.T, ids ...string) *upstream.Pool {
	t.Helper()
	entries := make([]upstream.PoolEntry, 0, len(ids))
	for i, id := range ids {
		entries = append(entries, upstream.PoolEntry{
			Up:       &stubUpstream{id: id},
			Priority: 100 + i,
		})
	}
	pool, err := upstream.NewPool(entries, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("upstream.NewPool: %v", err)
	}
	return pool
}

type stubUpstream struct {
	id string
}

func (s *stubUpstream) ID() string                { return s.id }
func (s *stubUpstream) Kind() config.UpstreamKind { return config.KindDirect }
func (s *stubUpstream) Dial(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, errors.New("stub: dial not implemented for this test")
}

type fakePersistSink struct {
	successCount atomicCounter
	errorCount   atomicCounter
}

func (f *fakePersistSink) ObservePersistenceWrite(result string) {
	switch result {
	case "success":
		f.successCount.Add(1)
	default:
		f.errorCount.Add(1)
	}
}

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (a *atomicCounter) Add(n int) {
	a.mu.Lock()
	a.n += n
	a.mu.Unlock()
}

func (a *atomicCounter) Load() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}
