package main

import (
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/metrics"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// quietTestLogger discards log output so tests do not spam stderr.
func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// directUpstream returns a UpstreamConfig that builds without network IO.
// Kind direct is the cheapest valid upstream — needed for tests that
// build a real Pool without actually dialing anywhere.
func directUpstream(id string, priority int) config.UpstreamConfig {
	p := priority
	return config.UpstreamConfig{ID: id, Kind: config.KindDirect, Priority: &p}
}

func buildScoreboard(t *testing.T, ucs ...config.UpstreamConfig) (*scoreboard.Scoreboard, *upstream.Pool) {
	t.Helper()
	entries := make([]upstream.PoolEntry, 0, len(ucs))
	for _, uc := range ucs {
		up, err := upstream.New(uc, time.Second)
		if err != nil {
			t.Fatalf("build upstream %q: %v", uc.ID, err)
		}
		entries = append(entries, upstream.PoolEntry{Up: up, Priority: uc.PriorityValue()})
	}
	pool, err := upstream.NewPool(entries, 5, quietTestLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	cfg := scoreboard.Config{
		ConnectionRefused: true,
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused:    {Penalty: 3, Cooldown: 30 * time.Second},
			failure.KindTimeout:    {Penalty: 2, Cooldown: 15 * time.Second},
			failure.KindRateLimit:  {Penalty: 4, Cooldown: 120 * time.Second},
			failure.KindForbidden:  {Penalty: 6, Cooldown: 30 * time.Minute},
			failure.KindLegalBlock: {Penalty: 8, Cooldown: 6 * time.Hour},
			failure.KindBodyMatch:  {Penalty: 5, Cooldown: 60 * time.Second},
		},
		SuccessWeight:  1,
		ScoreCap:       10,
		ProbeChance:    0,
		DecayInterval:  5 * time.Minute,
		DecayStep:      0.5,
		CascadeTTL:     0,
		DebounceWindow: 100 * time.Millisecond,
	}
	sb, err := scoreboard.New(pool, cfg, scoreboard.WithLogger(quietTestLogger()))
	if err != nil {
		t.Fatalf("scoreboard.New: %v", err)
	}
	t.Cleanup(sb.Stop)
	return sb, pool
}

// composerForTest builds a poolComposer with a real scoreboard + a real
// (but unused) HTTPServer so CloseTransportsExcept does not panic on a
// nil receiver. The composer's blocks slice is seeded from poolBlocks
// the same way newPoolComposer does at runtime.
func composerForTest(t *testing.T, static []config.UpstreamConfig, blocks []*poolBlock, initialPool []config.UpstreamConfig) (*poolComposer, *scoreboard.Scoreboard, *metrics.Registry) {
	t.Helper()
	sb, _ := buildScoreboard(t, initialPool...)
	httpSrv, err := listener.NewHTTP("127.0.0.1:0", sb, failure.NewStatusDetector(nil), 5, quietTestLogger())
	if err != nil {
		t.Fatalf("build http listener: %v", err)
	}
	t.Cleanup(func() {
		// Shutdown is a no-op when Serve never ran; the HTTPServer
		// holds no goroutines we need to drain. CloseTransportsExcept
		// is what the composer calls during a swap, so the listener
		// just needs to exist for the cache map to be addressable.
		_ = httpSrv
	})
	registry := metrics.New()
	c := newPoolComposer(
		static,
		blocks,
		sb,
		httpSrv,
		nil, // no gauge refresher in tests
		registry,
		quietTestLogger(),
		5,           // retryCap
		time.Second, // dialTimeout
	)
	return c, sb, registry
}

// poolHotSwapCount returns the running value of the success-result
// pool_hotswap_total counter for this registry.
func poolHotSwapCount(t *testing.T, reg *metrics.Registry, result string) float64 {
	t.Helper()
	mfs, err := reg.PrometheusRegistry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "tunnelsmith_pool_hotswap_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func sortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// TestPoolComposerSwapsRunningPool — Update with a different id set
// rebuilds the pool; sb.PoolIDs reflects the new union of static + new
// expansion and the success counter ticks.
func TestPoolComposerSwapsRunningPool(t *testing.T) {
	t.Parallel()
	staticUC := directUpstream("static-direct", 100)
	startBlockUCs := []config.UpstreamConfig{directUpstream("mvd-001", 200)}
	pb := &poolBlock{idPrefix: "mvd", initial: startBlockUCs, logger: quietTestLogger()}
	initialPool := append([]config.UpstreamConfig{staticUC}, startBlockUCs...)
	c, sb, reg := composerForTest(t, []config.UpstreamConfig{staticUC}, []*poolBlock{pb}, initialPool)

	// Sanity: scoreboard sees both ids initially.
	if got, want := sortedIDs(sb.PoolIDs()), []string{"mvd-001", "static-direct"}; !equalStrings(got, want) {
		t.Fatalf("initial PoolIDs = %v, want %v", got, want)
	}

	// Hot-swap: drop mvd-001, add mvd-002 and mvd-003.
	next := []config.UpstreamConfig{
		directUpstream("mvd-002", 200),
		directUpstream("mvd-003", 200),
	}
	applied, err := c.Update("mvd", next)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !applied {
		t.Fatalf("Update reported applied=false on a real change")
	}
	if got, want := sortedIDs(sb.PoolIDs()), []string{"mvd-002", "mvd-003", "static-direct"}; !equalStrings(got, want) {
		t.Fatalf("after Update PoolIDs = %v, want %v", got, want)
	}
	if got := poolHotSwapCount(t, reg, "success"); got != 1 {
		t.Fatalf("pool_hotswap_total{result=success} = %v, want 1", got)
	}
}

// TestPoolComposerPreservesStaticEntries — the static slice survives
// every refresh-tick swap regardless of how the block expansion churns.
func TestPoolComposerPreservesStaticEntries(t *testing.T) {
	t.Parallel()
	statics := []config.UpstreamConfig{
		directUpstream("static-A", 50),
		directUpstream("static-B", 60),
	}
	pb := &poolBlock{idPrefix: "mvd", initial: []config.UpstreamConfig{directUpstream("mvd-001", 200)}, logger: quietTestLogger()}
	initialPool := append([]config.UpstreamConfig{}, statics...)
	initialPool = append(initialPool, pb.initial...)
	c, sb, _ := composerForTest(t, statics, []*poolBlock{pb}, initialPool)

	// Empty the block entirely.
	if _, err := c.Update("mvd", nil); err != nil {
		t.Fatalf("Update with empty next: %v", err)
	}
	if got, want := sortedIDs(sb.PoolIDs()), []string{"static-A", "static-B"}; !equalStrings(got, want) {
		t.Fatalf("after empty Update PoolIDs = %v, want statics only %v", got, want)
	}

	// Block comes back with new ids.
	if _, err := c.Update("mvd", []config.UpstreamConfig{directUpstream("mvd-099", 200)}); err != nil {
		t.Fatalf("Update with one entry: %v", err)
	}
	if got, want := sortedIDs(sb.PoolIDs()), []string{"mvd-099", "static-A", "static-B"}; !equalStrings(got, want) {
		t.Fatalf("after refill Update PoolIDs = %v, want %v", got, want)
	}
}

// TestPoolComposerEvictsForcePinForRemovedID — a force pin against an
// upstream id that disappears in the next expansion is dropped during
// the swap (Scoreboard.ReplacePool's existing behavior surfaced through
// the composer's path).
func TestPoolComposerEvictsForcePinForRemovedID(t *testing.T) {
	t.Parallel()
	statics := []config.UpstreamConfig{directUpstream("static-direct", 100)}
	pb := &poolBlock{idPrefix: "mvd", initial: []config.UpstreamConfig{directUpstream("mvd-001", 200)}, logger: quietTestLogger()}
	initialPool := append([]config.UpstreamConfig{}, statics...)
	initialPool = append(initialPool, pb.initial...)
	c, sb, _ := composerForTest(t, statics, []*poolBlock{pb}, initialPool)

	// Pin a host to mvd-001, then drop mvd-001 from the expansion.
	if err := sb.Force("example.com", "mvd-001", time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("Force: %v", err)
	}
	if got := sb.ForceSnapshot(); len(got) != 1 || got[0].UpstreamID != "mvd-001" {
		t.Fatalf("pre-swap ForceSnapshot = %+v, want one pin to mvd-001", got)
	}
	if _, err := c.Update("mvd", []config.UpstreamConfig{directUpstream("mvd-002", 200)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := sb.ForceSnapshot(); len(got) != 0 {
		t.Fatalf("post-swap ForceSnapshot = %+v, want empty (pin to removed mvd-001 should be evicted)", got)
	}
}

// TestPoolComposerErrorLeavesPoolUntouched — a malformed UpstreamConfig
// in the new expansion makes the swap fail; the scoreboard's pool keeps
// the previous set and the error counter ticks.
func TestPoolComposerErrorLeavesPoolUntouched(t *testing.T) {
	t.Parallel()
	statics := []config.UpstreamConfig{directUpstream("static-direct", 100)}
	pb := &poolBlock{idPrefix: "mvd", initial: []config.UpstreamConfig{directUpstream("mvd-001", 200)}, logger: quietTestLogger()}
	initialPool := append([]config.UpstreamConfig{}, statics...)
	initialPool = append(initialPool, pb.initial...)
	c, sb, reg := composerForTest(t, statics, []*poolBlock{pb}, initialPool)

	beforeIDs := sortedIDs(sb.PoolIDs())

	// upstream.New rejects an empty Kind ("kind is required"), which
	// triggers a build error inside the composer's swap path without
	// touching the network.
	bad := config.UpstreamConfig{ID: "broken", Kind: ""}
	prio := 200
	bad.Priority = &prio
	if applied, err := c.Update("mvd", []config.UpstreamConfig{bad}); err == nil {
		t.Fatalf("Update with malformed entry: want error, got nil (applied=%v)", applied)
	}

	if got := sortedIDs(sb.PoolIDs()); !equalStrings(got, beforeIDs) {
		t.Fatalf("PoolIDs changed after failed swap: got %v, want %v", got, beforeIDs)
	}
	if got := poolHotSwapCount(t, reg, "error"); got != 1 {
		t.Fatalf("pool_hotswap_total{result=error} = %v, want 1", got)
	}
	if got := poolHotSwapCount(t, reg, "success"); got != 0 {
		t.Fatalf("pool_hotswap_total{result=success} = %v, want 0", got)
	}
}

// TestPoolComposerErrorDoesNotPoisonCacheForOtherBlocks — a failed
// Update on one block must leave that block's cached expansion at the
// previously-installed snapshot. Otherwise a subsequent Update on a
// DIFFERENT block would build its merged view from the failed
// expansion (the running pool never saw it) and install upstreams
// that were never validated. Pins the
// commit-cache-only-on-success contract.
func TestPoolComposerErrorDoesNotPoisonCacheForOtherBlocks(t *testing.T) {
	t.Parallel()
	statics := []config.UpstreamConfig{directUpstream("static-direct", 100)}
	pbA := &poolBlock{idPrefix: "mvd-a", initial: []config.UpstreamConfig{directUpstream("a-001", 200)}, logger: quietTestLogger()}
	pbB := &poolBlock{idPrefix: "mvd-b", initial: []config.UpstreamConfig{directUpstream("b-001", 200)}, logger: quietTestLogger()}
	initialPool := append([]config.UpstreamConfig{}, statics...)
	initialPool = append(initialPool, pbA.initial...)
	initialPool = append(initialPool, pbB.initial...)
	c, sb, _ := composerForTest(t, statics, []*poolBlock{pbA, pbB}, initialPool)

	// First, confirm a healthy update on B installs cleanly.
	if _, err := c.Update("mvd-b", []config.UpstreamConfig{directUpstream("b-002", 200)}); err != nil {
		t.Fatalf("Update b: %v", err)
	}

	// Failed update on A: malformed entry trips upstream.New.
	prio := 200
	bad := config.UpstreamConfig{ID: "broken", Kind: ""}
	bad.Priority = &prio
	if _, err := c.Update("mvd-a", []config.UpstreamConfig{bad}); err == nil {
		t.Fatalf("Update a (malformed): want error, got nil")
	}

	// Now update B again with a fresh expansion. The merged view must
	// include A's *previous* expansion (a-001), not the failed
	// "broken" entry. If the cache had been poisoned, the next pool
	// would either include "broken" (and fail to build) or be missing
	// "a-001".
	if _, err := c.Update("mvd-b", []config.UpstreamConfig{directUpstream("b-003", 200)}); err != nil {
		t.Fatalf("Update b again: %v", err)
	}
	want := []string{"a-001", "b-003", "static-direct"}
	if got := sortedIDs(sb.PoolIDs()); !equalStrings(got, want) {
		t.Fatalf("after recovery PoolIDs = %v, want %v (failed Update on a must not have committed its cached expansion)", got, want)
	}
}

// TestPoolComposerRetriesAfterTransientError — pins the swap-retry
// contract: when an Update for a snapshot fails, the cache stays at
// the prior good snapshot, so a subsequent Update with the same
// snapshot re-attempts the swap. Without this, a one-time failure
// would go silent forever if Mullvad's relay list happens to be
// stable post-failure (the expander's prev/next diff would be empty
// on later ticks). The success counter ticks once after recovery.
func TestPoolComposerRetriesAfterTransientError(t *testing.T) {
	t.Parallel()
	statics := []config.UpstreamConfig{directUpstream("static-direct", 100)}
	pb := &poolBlock{idPrefix: "mvd", initial: []config.UpstreamConfig{directUpstream("mvd-001", 200)}, logger: quietTestLogger()}
	initialPool := append([]config.UpstreamConfig{}, statics...)
	initialPool = append(initialPool, pb.initial...)
	c, sb, reg := composerForTest(t, statics, []*poolBlock{pb}, initialPool)

	// Tick 1: malformed entry — Update fails, cache stays at mvd-001.
	prio := 200
	bad := config.UpstreamConfig{ID: "broken", Kind: ""}
	bad.Priority = &prio
	if _, err := c.Update("mvd", []config.UpstreamConfig{bad}); err == nil {
		t.Fatalf("Update with malformed entry: want error, got nil")
	}
	if got := poolHotSwapCount(t, reg, "error"); got != 1 {
		t.Fatalf("error counter after first failure = %v, want 1", got)
	}

	// Tick 2: same call (stable Mullvad snapshot post-failure) still
	// errors. Without the cache-on-success fix the composer would
	// have advanced its cache to "broken" and a second call would
	// behave differently. The test asserts both error-counter ticks
	// land and the running pool stays at mvd-001.
	if _, err := c.Update("mvd", []config.UpstreamConfig{bad}); err == nil {
		t.Fatalf("Update again with malformed entry: want error, got nil")
	}
	if got := poolHotSwapCount(t, reg, "error"); got != 2 {
		t.Fatalf("error counter after retry = %v, want 2", got)
	}
	if got, want := sortedIDs(sb.PoolIDs()), []string{"mvd-001", "static-direct"}; !equalStrings(got, want) {
		t.Fatalf("PoolIDs after two failed Updates = %v, want %v", got, want)
	}

	// Tick 3: snapshot recovers. Update succeeds, success counter
	// ticks, running pool catches up.
	if applied, err := c.Update("mvd", []config.UpstreamConfig{directUpstream("mvd-002", 200)}); err != nil || !applied {
		t.Fatalf("recovery Update: applied=%v err=%v, want applied=true err=nil", applied, err)
	}
	if got := poolHotSwapCount(t, reg, "success"); got != 1 {
		t.Fatalf("success counter after recovery = %v, want 1", got)
	}

	// Tick 4: stable snapshot equal to last-applied — Update is a
	// silent no-op. applied=false, no metric tick, no swap.
	if applied, err := c.Update("mvd", []config.UpstreamConfig{directUpstream("mvd-002", 200)}); err != nil || applied {
		t.Fatalf("no-op Update: applied=%v err=%v, want applied=false err=nil", applied, err)
	}
	if got := poolHotSwapCount(t, reg, "success"); got != 1 {
		t.Fatalf("success counter after no-op = %v, want still 1", got)
	}
}

// TestPoolComposerUnknownIDPrefixReturnsError — Update against an
// id_prefix that was not registered at startup is rejected (caller bug)
// and counted as an error.
func TestPoolComposerUnknownIDPrefixReturnsError(t *testing.T) {
	t.Parallel()
	statics := []config.UpstreamConfig{directUpstream("static-direct", 100)}
	pb := &poolBlock{idPrefix: "mvd", initial: []config.UpstreamConfig{directUpstream("mvd-001", 200)}, logger: quietTestLogger()}
	initialPool := append([]config.UpstreamConfig{}, statics...)
	initialPool = append(initialPool, pb.initial...)
	c, sb, reg := composerForTest(t, statics, []*poolBlock{pb}, initialPool)
	beforeIDs := sortedIDs(sb.PoolIDs())

	_, err := c.Update("ghost", []config.UpstreamConfig{directUpstream("ghost-001", 200)})
	if err == nil {
		t.Fatal("Update with unknown id_prefix: want error, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error message %q should name the unknown id_prefix", err.Error())
	}
	if got := sortedIDs(sb.PoolIDs()); !equalStrings(got, beforeIDs) {
		t.Errorf("PoolIDs changed after rejected Update: got %v, want %v", got, beforeIDs)
	}
	if got := poolHotSwapCount(t, reg, "error"); got != 1 {
		t.Errorf("pool_hotswap_total{result=error} = %v, want 1", got)
	}
}

// TestPoolComposerRejectsDuplicateIDs locks in the post-swap uniqueness
// invariant. If a refresh tick ever produces an id that collides with a
// static [[upstream]] (or another block's expansion), the scoreboard
// would silently key multiple upstreams under one id and scramble
// routing/scoring. Update must reject the merged view, leave the
// running pool untouched, and tick result=error.
func TestPoolComposerRejectsDuplicateIDs(t *testing.T) {
	t.Parallel()
	staticUC := directUpstream("collide-id", 100)
	pb := &poolBlock{idPrefix: "mvd", initial: []config.UpstreamConfig{directUpstream("mvd-001", 200)}, logger: quietTestLogger()}
	initialPool := append([]config.UpstreamConfig{staticUC}, pb.initial...)
	c, sb, reg := composerForTest(t, []config.UpstreamConfig{staticUC}, []*poolBlock{pb}, initialPool)

	beforeIDs := sortedIDs(sb.PoolIDs())
	if !equalStrings(beforeIDs, []string{"collide-id", "mvd-001"}) {
		t.Fatalf("initial PoolIDs = %v, want [collide-id mvd-001]", beforeIDs)
	}

	// Update introduces an id that already exists in static.
	colliding := []config.UpstreamConfig{directUpstream("collide-id", 200)}
	applied, err := c.Update("mvd", colliding)
	if err == nil {
		t.Fatal("Update with colliding id: want error, got nil")
	}
	if applied {
		t.Fatalf("Update with colliding id reported applied=true; want false")
	}
	if !strings.Contains(err.Error(), "collide-id") {
		t.Errorf("error message %q should name the duplicate id", err.Error())
	}
	if got := sortedIDs(sb.PoolIDs()); !equalStrings(got, beforeIDs) {
		t.Errorf("PoolIDs changed after rejected Update: got %v, want %v", got, beforeIDs)
	}
	if got := poolHotSwapCount(t, reg, "error"); got != 1 {
		t.Errorf("pool_hotswap_total{result=error} = %v, want 1", got)
	}
	if got := poolHotSwapCount(t, reg, "success"); got != 0 {
		t.Errorf("pool_hotswap_total{result=success} = %v, want 0", got)
	}
}

// TestPoolComposerEmptyExpansionStableNoSwap regresses the nil-vs-empty
// slice bug where the cache seed and post-swap cache commit collapsed
// an empty-but-non-nil snapshot (`[]config.UpstreamConfig{}`) into nil.
// `reflect.DeepEqual(nil, []T{}) == false`, so before the fix every
// refresh tick on a stably-empty Mullvad block triggered a redundant
// pool rebuild and an unexpected `pool_hotswap_total` increment.
func TestPoolComposerEmptyExpansionStableNoSwap(t *testing.T) {
	t.Parallel()
	staticUC := directUpstream("static-direct", 100)
	emptyExpansion := []config.UpstreamConfig{} // non-nil, len 0 — what mullvad.Expander.Snapshot returns
	pb := &poolBlock{idPrefix: "mvd", initial: emptyExpansion, logger: quietTestLogger()}
	initialPool := []config.UpstreamConfig{staticUC}
	c, sb, reg := composerForTest(t, []config.UpstreamConfig{staticUC}, []*poolBlock{pb}, initialPool)

	beforeIDs := sortedIDs(sb.PoolIDs())
	if !equalStrings(beforeIDs, []string{"static-direct"}) {
		t.Fatalf("initial PoolIDs = %v, want [static-direct]", beforeIDs)
	}

	// Same empty-but-non-nil snapshot on the next tick — Update must see
	// it as equal to the cached seed and short-circuit. Try a few times
	// to lock in that no slow drift accumulates.
	for i := 0; i < 3; i++ {
		applied, err := c.Update("mvd", []config.UpstreamConfig{})
		if err != nil {
			t.Fatalf("tick %d Update: %v", i, err)
		}
		if applied {
			t.Fatalf("tick %d Update reported applied=true on a stably-empty snapshot", i)
		}
	}
	if got := poolHotSwapCount(t, reg, "success"); got != 0 {
		t.Fatalf("pool_hotswap_total{result=success} = %v, want 0 (no swap should fire)", got)
	}
	if got := poolHotSwapCount(t, reg, "error"); got != 0 {
		t.Fatalf("pool_hotswap_total{result=error} = %v, want 0", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
