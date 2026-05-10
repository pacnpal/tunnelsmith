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
	if err := c.Update("mvd", next); err != nil {
		t.Fatalf("Update: %v", err)
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
	if err := c.Update("mvd", nil); err != nil {
		t.Fatalf("Update with empty next: %v", err)
	}
	if got, want := sortedIDs(sb.PoolIDs()), []string{"static-A", "static-B"}; !equalStrings(got, want) {
		t.Fatalf("after empty Update PoolIDs = %v, want statics only %v", got, want)
	}

	// Block comes back with new ids.
	if err := c.Update("mvd", []config.UpstreamConfig{directUpstream("mvd-099", 200)}); err != nil {
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
	if err := c.Update("mvd", []config.UpstreamConfig{directUpstream("mvd-002", 200)}); err != nil {
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
	if err := c.Update("mvd", []config.UpstreamConfig{bad}); err == nil {
		t.Fatalf("Update with malformed entry: want error, got nil")
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

	err := c.Update("ghost", []config.UpstreamConfig{directUpstream("ghost-001", 200)})
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
