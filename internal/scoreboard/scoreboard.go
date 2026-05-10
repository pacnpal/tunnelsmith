// Package scoreboard owns the per-(host, upstream) scoring layer Tunnelsmith
// uses to pick exits. The scoreboard wraps an upstream.Pool: the pool keeps
// the static priority-ordered list, the scoreboard layers per-host memory on
// top via score, cooldowns, time decay, cascade-failure handling, and
// recovery probing.
//
// One Scoreboard serves the whole proxy. Pick is the per-request selection
// hook; DialFor is the listener-side entry point that drives the whole
// "pick, dial, record outcome, advance on failure" loop. Both are safe for
// concurrent use; locking is internal.
package scoreboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// MetricsSink names the methods the scoreboard calls when it wants to
// record an observation. metrics.Registry implements this interface; pass
// nil to disable metric emission. Every call site checks for nil before
// dispatching, so the scoreboard is usable without a registry attached.
type MetricsSink interface {
	ObserveDial(upstreamID, outcome string, latency time.Duration)
	ObserveCascadeTrip()
	ObserveProbePick()
}

// Policy is the per-Kind tuple of penalty (a positive number subtracted from
// the relevant entry's score) and cooldown (how long the affected
// (host, upstream) pair sits out of the rotation).
type Policy struct {
	Penalty  float64
	Cooldown time.Duration
}

// Config is the runtime tuning the scoreboard reads at construction. Build
// it from a config.ScoringConfig via FromConfig, or hand-roll it in tests.
type Config struct {
	// KindPolicy maps each failure.Kind the listener can report to its
	// penalty and cooldown. Missing kinds are treated as zero policy
	// (no penalty, no cooldown), which is the right thing for kinds that
	// are declared but not yet wired through the listener path.
	KindPolicy map[failure.Kind]Policy

	// SuccessWeight is added to score on each successful dial.
	SuccessWeight float64

	// ScoreCap clamps |score| so a long-running winner cannot accumulate
	// so much score that one bad minute cannot dethrone it.
	ScoreCap float64

	// ProbeChance is the probability per Pick that the scoreboard tries a
	// non-top eligible candidate, giving recovered upstreams a chance to
	// climb back up. 0 disables probing.
	ProbeChance float64

	// DecayInterval is how often the decay goroutine ticks. DecayStep is
	// the absolute amount each entry's score moves toward zero per tick.
	DecayInterval time.Duration
	DecayStep     float64

	// CascadeTTL is the negative TTL applied to a host when every upstream
	// fails for it. Subsequent Picks within the TTL return ErrCascadeCooling
	// without burning through the pool again.
	CascadeTTL time.Duration

	// DebounceWindow collapses identical (host, upstream, kind) failure
	// events arriving within the window into a single penalty event. Stops
	// concurrent client requests from over-penalizing one rate-limit event
	// into N penalties.
	DebounceWindow time.Duration

	// PruneAfter governs the persistence-tick prune pass. An entry whose
	// score has decayed to zero and whose lastSeen is older than
	// PruneAfter is dropped during the next snapshot. <= 0 disables
	// entry pruning; cascade and debounce eviction always run.
	PruneAfter time.Duration
}

// FromConfig builds a scoreboard Config from the parsed [failure.scoring]
// section plus the [[failure.status]] entries. Phase 4 fires refused and
// timeout from the dial path; Phase 5 wires the status-rule kinds; Phase 8
// fires KindBodyMatch from the listener's response-body inspector. The
// kind→policy table is complete at construction so a missing kind never
// records a penalty silently.
func FromConfig(s config.ScoringConfig, status []config.StatusRule) Config {
	policy := map[failure.Kind]Policy{
		failure.KindRefused: {
			Penalty:  s.RefusedPenalty,
			Cooldown: s.RefusedCooldown.Duration(),
		},
		failure.KindTimeout: {
			Penalty:  s.TimeoutPenalty,
			Cooldown: s.TimeoutCooldown.Duration(),
		},
		failure.KindBodyMatch: {
			Penalty:  s.BodyMatchPenalty,
			Cooldown: s.BodyMatchCooldown.Duration(),
		},
	}
	for _, sr := range status {
		switch sr.Code {
		case 429:
			policy[failure.KindRateLimit] = Policy{
				Penalty:  float64(sr.Penalty),
				Cooldown: sr.Cooldown.Duration(),
			}
		case 403:
			policy[failure.KindForbidden] = Policy{
				Penalty:  float64(sr.Penalty),
				Cooldown: sr.Cooldown.Duration(),
			}
		case 451:
			policy[failure.KindLegalBlock] = Policy{
				Penalty:  float64(sr.Penalty),
				Cooldown: sr.Cooldown.Duration(),
			}
		}
	}
	return Config{
		KindPolicy:     policy,
		SuccessWeight:  s.SuccessWeight,
		ScoreCap:       s.ScoreCap,
		ProbeChance:    s.ProbeChance,
		DecayInterval:  s.DecayInterval.Duration(),
		DecayStep:      s.DecayStep,
		CascadeTTL:     s.CascadeTTL.Duration(),
		DebounceWindow: s.DebounceWindow.Duration(),
		PruneAfter:     s.PruneAfter.Duration(),
	}
}

// Errors the scoreboard surfaces. Callers can errors.Is them to branch on
// "host is in cascade" vs "we tried every upstream this request".
var (
	// ErrCascadeCooling means the host is in cascade-failure and Pick is
	// short-circuiting. The caller should fail fast; no upstream attempt is
	// going to succeed for this host until the negative TTL expires.
	ErrCascadeCooling = errors.New("scoreboard: host in cascade failure")

	// ErrPoolExhausted means every configured upstream is in the per-call
	// tried set. The caller has run out of options for this request.
	ErrPoolExhausted = errors.New("scoreboard: every upstream tried")
)

// CascadeError carries the host whose cascade tripped, alongside the wrapped
// sentinel. Useful when callers want to emit a log line or response header
// that names which host is in cascade without parsing the error string.
type CascadeError struct {
	Host string
}

func (e *CascadeError) Error() string {
	return fmt.Sprintf("scoreboard: host %q in cascade failure", e.Host)
}

func (e *CascadeError) Unwrap() error { return ErrCascadeCooling }

// Scoreboard is the per-(host, upstream) scoring layer. Construct via New;
// safe for concurrent use.
type Scoreboard struct {
	pool   *upstream.Pool
	cfg    Config
	logger *slog.Logger

	// poolEntries is a snapshot of pool.Entries() taken at construction.
	// Pool entries are immutable after NewPool returns, so caching the
	// copy avoids a per-Pick allocation on the hot path. Pool stays
	// referenced for transparency and for the upstream-id list emitted
	// in startup logs.
	poolEntries  []upstream.PoolEntry
	poolRetryCap int
	poolLen      int

	// rules carries Phase 8's compiled per-host routing rules. nil
	// means "no rules configured"; Pick treats every host as
	// pool-wide. The pointer is swapped atomically by ReplaceRules
	// under mu, so a hot reload cannot tear concurrent reads.
	rules *upstream.RuleSet

	// mu guards entries, cascade, and forces. Pick takes RLock; Record*,
	// the decay loop, and the Phase 9 admin actions take Lock.
	mu      sync.RWMutex
	entries map[string]map[string]*entry // host -> upstreamID -> entry
	cascade map[string]time.Time         // host -> cascade-cooling expiry
	forces  map[string]ForceEntry        // host -> active Force pin (Phase 9)

	debounceMu sync.Mutex
	debounce   map[debounceKey]time.Time

	randMu sync.Mutex
	rand   *rand.Rand

	clock func() time.Time

	// decayCancel + decayDone form the lifecycle for the decay goroutine.
	// Start sets them; Stop cancels and waits.
	decayMu     sync.Mutex
	decayCancel context.CancelFunc
	decayDone   chan struct{}

	// metrics is the optional sink for Prometheus emission. Nil means
	// metrics are disabled; every call site uses recordMetric helpers
	// that no-op on nil.
	metrics MetricsSink
}

// entry is per-(host, upstream) state. Field comments name the invariants;
// all reads/writes happen under Scoreboard.mu.
type entry struct {
	score              float64
	cooldownUntil      time.Time
	lastSeen           time.Time
	globalSuccessCount uint64
	globalFailureCount uint64
}

type debounceKey struct {
	host       string
	upstreamID string
	kind       failure.Kind
}

// EntrySnapshot is a read-only view of one entry. Returned by Snapshot for
// metrics and the future web UI.
type EntrySnapshot struct {
	Host          string
	UpstreamID    string
	Score         float64
	CooldownUntil time.Time
	LastSeen      time.Time
	GlobalSuccess uint64
	GlobalFailure uint64
}

// Option customizes a Scoreboard at construction. Logger, clock, and random
// source are all swappable so tests can inject deterministic versions.
type Option func(*Scoreboard)

// WithLogger sets the structured logger used for state-change log lines.
func WithLogger(l *slog.Logger) Option {
	return func(s *Scoreboard) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithClock injects a custom now() function. Tests use this with a manual
// time source so decay and cooldown behavior is observable without sleeping.
func WithClock(clock func() time.Time) Option {
	return func(s *Scoreboard) {
		if clock != nil {
			s.clock = clock
		}
	}
}

// WithRand injects a pre-seeded random source. Tests use a fixed seed so
// probe-roll outcomes are deterministic.
func WithRand(r *rand.Rand) Option {
	return func(s *Scoreboard) {
		if r != nil {
			s.rand = r
		}
	}
}

// WithMetrics attaches a metrics sink. Pass nil to disable metric emission;
// the scoreboard never dereferences a nil sink.
func WithMetrics(m MetricsSink) Option {
	return func(s *Scoreboard) {
		s.metrics = m
	}
}

// WithRules attaches Phase 8's per-host routing rules. Pass nil (or no
// option at all) to disable rule-aware routing; Pick will treat every
// host as pool-wide. The hot-reload path uses ReplaceRules to swap an
// already-running scoreboard's rule set without rebuilding it.
func WithRules(rs *upstream.RuleSet) Option {
	return func(s *Scoreboard) {
		s.rules = rs
	}
}

// New builds a Scoreboard wrapping pool. Pool must be non-nil. Pass options
// to override the default logger, clock, or random source.
func New(pool *upstream.Pool, cfg Config, opts ...Option) (*Scoreboard, error) {
	if pool == nil {
		return nil, errors.New("scoreboard: pool is nil")
	}
	s := &Scoreboard{
		pool:         pool,
		poolEntries:  pool.Entries(),
		poolRetryCap: pool.RetryCap(),
		poolLen:      pool.Len(),
		cfg:          cfg,
		logger:       slog.Default(),
		entries:      make(map[string]map[string]*entry),
		cascade:      make(map[string]time.Time),
		debounce:     make(map[debounceKey]time.Time),
		clock:        time.Now,
		rand:         rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// configSnapshot returns a value copy of s.cfg taken under the read lock.
// Reload swaps the whole struct, never mutates a field in place, so a
// readers' value copy stays consistent with whatever cfg the writer
// installed at snapshot time. Hot paths that read more than one field
// take one snapshot at the start so multiple reads cannot tear
// mid-write.
func (s *Scoreboard) configSnapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// poolSnapshot returns the cached pool view (entries slice header, retry
// cap, pool length) taken under the read lock. ReplacePool installs
// fresh slices, so a reader's pointer to the old backing array stays
// stable for the lifetime of its dial.
func (s *Scoreboard) poolSnapshot() (entries []upstream.PoolEntry, retryCap, poolLen int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.poolEntries, s.poolRetryCap, s.poolLen
}

// Now returns the scoreboard's current time per its injected clock.
// The Phase 9 web UI handlers use this to keep a manual-clock test
// deterministic across both the scoreboard and the UI layer; if the
// production scoreboard never had WithClock called, this returns the
// real wall clock.
func (s *Scoreboard) Now() time.Time {
	return s.clock()
}

// PoolIDs returns the upstream ids from the live pool snapshot. Used
// by the SIGHUP hot-reload path to validate rule.Prefer entries
// against the running pool before installing a new rule set.
func (s *Scoreboard) PoolIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.poolEntries))
	for i, e := range s.poolEntries {
		out[i] = e.Up.ID()
	}
	return out
}

// HasUpstream reports whether id exists in the live pool snapshot.
// Used by control-plane report validation on the per-request path to
// avoid allocating a full id list for each membership check.
func (s *Scoreboard) HasUpstream(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.poolEntries {
		if e.Up.ID() == id {
			return true
		}
	}
	return false
}

// ReplaceRules swaps the per-host rule set in place. Used by the SIGHUP
// hot-reload path: the request path's reads of s.rules happen under
// the same RLock that ReplaceRules takes for its write, so a Pick in
// flight either sees the old rule set fully (single coherent
// snapshot) or the new one. Pass nil to clear all rules.
func (s *Scoreboard) ReplaceRules(rs *upstream.RuleSet) {
	s.mu.Lock()
	s.rules = rs
	s.mu.Unlock()
}

// ReplacePool swaps the wrapped pool for a new one. Used by the SIGHUP
// hot-reload path: the per-(host, upstream) entry table survives the
// swap, so a host whose previous winner is still in the new pool keeps
// the cached winner. Entries keyed off ids that no longer exist in the
// new pool stay in the map but are unreachable through Pick (their id
// is not a candidate). The next Prune pass evicts them once their score
// decays below the threshold.
//
// Returns an error if newPool is nil; callers pass the old pool back if
// they want to keep things unchanged.
func (s *Scoreboard) ReplacePool(newPool *upstream.Pool) error {
	if newPool == nil {
		return errors.New("scoreboard: ReplacePool called with nil pool")
	}
	entries := newPool.Entries()
	retryCap := newPool.RetryCap()
	poolLen := newPool.Len()
	knownIDs := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		knownIDs[e.Up.ID()] = struct{}{}
	}

	var evicted []evictedForce
	s.mu.Lock()
	s.pool = newPool
	s.poolEntries = entries
	s.poolRetryCap = retryCap
	s.poolLen = poolLen
	// Issue #19: evict any active Force pin whose upstream id is no
	// longer in the new pool. pickForced already falls through to
	// normal scoring for stale pins, so routing was correct without
	// this; the eviction stops ForceSnapshot from continuing to render
	// the dead pin in the UI table after a hot reload drops the
	// upstream the pin referenced.
	for host, f := range s.forces {
		if _, ok := knownIDs[f.UpstreamID]; !ok {
			delete(s.forces, host)
			evicted = append(evicted, evictedForce{Host: host, UpstreamID: f.UpstreamID})
		}
	}
	s.mu.Unlock()

	for _, ev := range evicted {
		s.logger.Info("scoreboard force evicted",
			"host", ev.Host,
			"upstream_id", ev.UpstreamID,
			"reason", "upstream_removed_from_pool",
		)
	}
	return nil
}

// evictedForce is a small carrier the ReplacePool path uses to defer
// logging until after the lock is released; doing the slog calls under
// s.mu would hold the write lock for as long as slog's handler takes.
type evictedForce struct {
	Host       string
	UpstreamID string
}

// Reload updates the runtime tuning knobs in place. The new Config can
// change penalty weights, cooldowns, probe chance, debounce window,
// cascade TTL, and prune-after; DecayInterval is intentionally left
// alone because the decay goroutine reads its ticker once at start.
// Listener bindings, the upstream pool, and the random source are all
// changed through their own paths.
//
// Reload holds the write lock for the duration of the swap to avoid
// torn reads from concurrent Pick / Record* callers.
func (s *Scoreboard) Reload(newCfg Config) {
	s.mu.Lock()
	// Preserve DecayInterval so a SIGHUP cannot accidentally tear down
	// the running decay goroutine. Operators who want a different
	// interval restart the binary; the build plan documents this.
	preservedDecay := s.cfg.DecayInterval
	s.cfg = newCfg
	s.cfg.DecayInterval = preservedDecay
	s.mu.Unlock()
	s.logger.Info("scoreboard reloaded",
		"probe_chance", newCfg.ProbeChance,
		"cascade_ttl_ms", newCfg.CascadeTTL.Milliseconds(),
		"debounce_window_ms", newCfg.DebounceWindow.Milliseconds(),
		"prune_after_ms", newCfg.PruneAfter.Milliseconds(),
	)
}

// Start launches the time-decay goroutine. Stop or a cancelled ctx ends it.
// Calling Start twice without an intervening Stop is a no-op past the first
// call; the existing loop keeps running.
func (s *Scoreboard) Start(ctx context.Context) {
	s.decayMu.Lock()
	defer s.decayMu.Unlock()
	if s.decayCancel != nil {
		return
	}
	// Snapshot DecayInterval under s.mu so a SIGHUP-driven Reload that
	// runs concurrently with Start cannot race the field read. Reload
	// preserves the value, but the byte-level swap of s.cfg is still
	// non-atomic.
	interval := s.configSnapshot().DecayInterval
	if interval <= 0 {
		// No decay configured; nothing to start. Keep Stop a no-op.
		return
	}
	dctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.decayCancel = cancel
	s.decayDone = done
	// Pass done as an argument so the goroutine never has to read
	// s.decayDone after Stop nils it. Without this, Stop and the goroutine
	// race on the struct field and Stop can hang on the receive.
	go s.decayLoop(dctx, done, interval)
}

// Stop cancels the decay goroutine and waits for it to exit. Safe to call
// multiple times; subsequent calls are no-ops.
func (s *Scoreboard) Stop() {
	s.decayMu.Lock()
	cancel := s.decayCancel
	done := s.decayDone
	s.decayCancel = nil
	s.decayDone = nil
	s.decayMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

func (s *Scoreboard) decayLoop(ctx context.Context, done chan struct{}, interval time.Duration) {
	defer close(done)
	defer func() {
		// Clear the lifecycle fields when the goroutine exits. Stop may
		// have already cleared them (in which case this is a no-op);
		// when the parent ctx is cancelled externally without Stop, this
		// makes a future Start callable instead of a permanent no-op.
		s.decayMu.Lock()
		s.decayCancel = nil
		s.decayDone = nil
		s.decayMu.Unlock()
	}()
	// interval is captured at Start time and frozen for the lifetime of
	// the goroutine; Reload deliberately preserves DecayInterval so that
	// changing it requires a process restart (documented in
	// docs/configuration.md).
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.decayTick()
		}
	}
}

// decayTick drifts every entry's score toward zero by DecayStep, clamped at
// zero. Holds mu for the duration; decay is cheap (one float subtraction
// per entry) and runs every DecayInterval, not per-request, so the lock
// pressure is fine for v1. DecayStep is read under the same lock so a
// concurrent Reload cannot race with the field read.
func (s *Scoreboard) decayTick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	step := s.cfg.DecayStep
	if step <= 0 {
		return
	}
	for _, perUpstream := range s.entries {
		for _, e := range perUpstream {
			switch {
			case e.score > 0:
				e.score -= step
				if e.score < 0 {
					e.score = 0
				}
			case e.score < 0:
				e.score += step
				if e.score > 0 {
					e.score = 0
				}
			}
		}
	}
}

// Pick returns the best non-cooled non-tried upstream for host. With
// probability cfg.ProbeChance, picks a non-top eligible candidate so a
// previously-penalized upstream gets occasional re-evaluation. Returns
// ErrCascadeCooling if the host is in cascade or ErrPoolExhausted if every
// candidate is in tried. If every untried candidate is on cooldown, picks
// the one whose cooldown expires soonest (cooldown is advisory rather than
// hard exclusion when there is nothing else to try).
//
// Phase 8: Pick consults the configured RuleSet. A rule whose host_glob
// matches host applies in two ways:
//   - Force=true narrows candidates to the rule's Prefer ids before
//     scoring. Every non-Prefer upstream is filtered out, even from
//     the cooldown-fallback path. ErrPoolExhausted fires if every
//     forced candidate is in tried.
//   - Force=false adds a "preferred ranking" that wins over score:
//     preferred upstreams sort to the top in the rule's declaration
//     order; everything else falls in by the existing (score desc,
//     base priority asc) tiebreak.
//
// Phase 9: a live Force pin (set via Scoreboard.Force from the web UI)
// short-circuits Pick the same way the static [[rule]] force=true path
// does, but with one host pinned to one upstream until the pin's
// expiry. The pin is checked before the rule-set scan: when active and
// the pinned upstream is not in tried, Pick returns it ahead of any
// scoring. If the pinned upstream is in tried (in-flight retry already
// burned it), the pin is ignored for this attempt and Pick falls back
// to normal scoring; the operator wanted a hint, not a guaranteed
// failure loop.
//
// tried may be nil; nil and empty are equivalent.
func (s *Scoreboard) Pick(host string, tried map[string]bool) (upstream.Upstream, error) {
	now := s.clock()
	if s.cascadeActive(host, now) {
		return nil, &CascadeError{Host: host}
	}
	if up, ok := s.pickForced(host, tried, now); ok {
		return up, nil
	}
	type ranked struct {
		up         upstream.Upstream
		basePri    int
		score      float64
		cooled     bool
		untilT     time.Time
		untilSet   bool
		preferRank int // 0 = not preferred; 1+ = position in rule.Prefer
	}
	// Take one read lock around every field that hot-reload can swap
	// (poolEntries, cfg.ProbeChance, rules) plus the entries map
	// iteration. A concurrent ReplacePool / Reload / ReplaceRules waits
	// behind this RLock; a concurrent reader runs without contention.
	s.mu.RLock()
	candidates := s.poolEntries
	probeChance := s.cfg.ProbeChance
	rule := s.rules.Match(host)
	if len(candidates) == 0 {
		s.mu.RUnlock()
		return nil, ErrPoolExhausted
	}

	// Read the prefer-rank lookup directly from the compiled rule.
	// NewRuleSet pre-builds it once at compile time; Pick stays
	// allocation-free for the rule path. Position is 1-based so 0
	// stays "not preferred" and the smallest non-zero value wins.
	var preferRank map[string]int
	if rule != nil {
		preferRank = rule.PreferRank
	}

	pool := make([]ranked, 0, len(candidates))
	for _, c := range candidates {
		id := c.Up.ID()
		if tried[id] {
			continue
		}
		// Force=true filters non-prefer ids out before they ever get
		// scored. Cascade fires when every forced candidate is tried.
		if rule != nil && rule.Force {
			if _, ok := preferRank[id]; !ok {
				continue
			}
		}
		var (
			score    float64
			untilT   time.Time
			untilSet bool
			cooled   bool
		)
		if perUp, ok := s.entries[host]; ok {
			if e, ok := perUp[id]; ok {
				score = e.score
				untilT = e.cooldownUntil
				untilSet = !untilT.IsZero()
				cooled = untilSet && untilT.After(now)
			}
		}
		pool = append(pool, ranked{
			up:         c.Up,
			basePri:    c.Priority,
			score:      score,
			cooled:     cooled,
			untilT:     untilT,
			untilSet:   untilSet,
			preferRank: preferRank[id],
		})
	}
	s.mu.RUnlock()

	if len(pool) == 0 {
		return nil, ErrPoolExhausted
	}

	eligible := make([]ranked, 0, len(pool))
	cooled := make([]ranked, 0, len(pool))
	for _, r := range pool {
		if r.cooled {
			cooled = append(cooled, r)
		} else {
			eligible = append(eligible, r)
		}
	}

	if len(eligible) == 0 {
		// Every untried upstream is on cooldown. Pick the one closest to
		// warming up rather than 502'ing the request: cooldown is advisory
		// when nothing else is available. Force=true already pruned
		// non-prefer ids out of cooled above, so this still respects the
		// rule.
		sort.Slice(cooled, func(i, j int) bool {
			return cooled[i].untilT.Before(cooled[j].untilT)
		})
		return cooled[0].up, nil
	}

	// Sort by (preferRank asc with 0 last, score desc, base priority asc).
	// Stable so the input order (priority order from the pool) is the
	// tiebreaker for entries with no prior data. preferRank == 0 sorts
	// after every ranked candidate, so non-preferred upstreams keep
	// their existing order behind the prefer list.
	sort.SliceStable(eligible, func(i, j int) bool {
		ri, rj := eligible[i].preferRank, eligible[j].preferRank
		if ri != rj {
			// Both 0 falls through to score/priority below.
			// One zero, one non-zero: the non-zero wins.
			if ri == 0 {
				return false
			}
			if rj == 0 {
				return true
			}
			return ri < rj
		}
		if eligible[i].score != eligible[j].score {
			return eligible[i].score > eligible[j].score
		}
		return eligible[i].basePri < eligible[j].basePri
	})

	// Probe roll: with prob ProbeChance, pick a non-top eligible candidate
	// uniformly at random so a previously-penalized upstream gets a chance
	// to recover. Skip when there is only one eligible. probeChance was
	// captured under the RLock above so it cannot tear with a concurrent
	// Reload.
	if len(eligible) > 1 && probeChance > 0 && s.probeRoll(probeChance) {
		idx := s.probePick(len(eligible) - 1)
		if s.metrics != nil {
			s.metrics.ObserveProbePick()
		}
		return eligible[1+idx].up, nil
	}

	return eligible[0].up, nil
}

// RecordSuccess applies success bookkeeping for (host, upstreamID): score
// climbs by SuccessWeight (capped at ScoreCap), cooldown clears, lastSeen
// updates, the global counter increments, and any cascade for this host
// clears (a single success ends the negative TTL early).
func (s *Scoreboard) RecordSuccess(host, upstreamID string, latency time.Duration) {
	now := s.clock()
	s.mu.Lock()
	e := s.getOrCreateLocked(host, upstreamID)
	e.score += s.cfg.SuccessWeight
	if e.score > s.cfg.ScoreCap {
		e.score = s.cfg.ScoreCap
	}
	e.cooldownUntil = time.Time{}
	e.lastSeen = now
	e.globalSuccessCount++
	delete(s.cascade, host)
	score := e.score
	s.mu.Unlock()
	s.logger.Debug("scoreboard success",
		"host", host,
		"upstream_id", upstreamID,
		"score", score,
		"latency_ms", latency.Milliseconds(),
	)
}

// RecordFailure applies a penalty + cooldown for (host, upstreamID, kind).
// cooldownOverride, when non-nil, is authoritative: it replaces any
// prior cooldownUntil rather than only extending it. A non-nil zero
// pointer represents "Retry-After: 0", meaning the destination says
// retry immediately, and clears any existing cooldownUntil. A nil
// pointer falls back to the kind's default cooldown from KindPolicy
// and only extends cooldownUntil (the same monotonic behavior earlier
// failures used).
//
// Identical (host, upstream, kind) triples arriving within DebounceWindow
// collapse into one penalty event; later calls are dropped silently.
// globalFailureCount counts penalty events, not raw observations, so it
// stays consistent with what the score actually changed by.
func (s *Scoreboard) RecordFailure(host, upstreamID string, kind failure.Kind, cooldownOverride *time.Duration) {
	if !kind.Valid() {
		return
	}
	now := s.clock()
	// Snapshot the cfg fields the rest of this method reads so a
	// concurrent Reload cannot tear KindPolicy / DebounceWindow / ScoreCap
	// across the call.
	cfg := s.configSnapshot()
	if !s.acceptForDebounce(host, upstreamID, kind, now, cfg.DebounceWindow) {
		return
	}
	policy := cfg.KindPolicy[kind]
	overridden := cooldownOverride != nil
	cooldown := policy.Cooldown
	if overridden {
		cooldown = *cooldownOverride
	}
	s.mu.Lock()
	e := s.getOrCreateLocked(host, upstreamID)
	e.score -= policy.Penalty
	if e.score < -cfg.ScoreCap {
		e.score = -cfg.ScoreCap
	}
	switch {
	case overridden && cooldown > 0:
		// Destination's explicit Retry-After wins over any prior
		// cooldown, even if the prior was longer.
		e.cooldownUntil = now.Add(cooldown)
	case overridden:
		// Retry-After: 0 means retry immediately; clear any prior
		// cooldown so this upstream is eligible again.
		e.cooldownUntil = time.Time{}
	case cooldown > 0:
		// No explicit override, fall back to extend-only behavior so
		// later default-cooldown events cannot shorten an active
		// cooldown.
		until := now.Add(cooldown)
		if until.After(e.cooldownUntil) {
			e.cooldownUntil = until
		}
	}
	e.lastSeen = now
	e.globalFailureCount++
	score := e.score
	s.mu.Unlock()
	s.logger.Debug("scoreboard failure",
		"host", host,
		"upstream_id", upstreamID,
		"kind", kind.String(),
		"penalty", policy.Penalty,
		"cooldown_ms", cooldown.Milliseconds(),
		"score", score,
	)
}

// acceptForDebounce returns true if a (host, upstream, kind) penalty should
// be applied now. False means the previous penalty was within window
// and this call should be a no-op. window is passed by the caller so a
// concurrent Reload cannot tear the s.cfg.DebounceWindow read.
func (s *Scoreboard) acceptForDebounce(host, upstreamID string, kind failure.Kind, now time.Time, window time.Duration) bool {
	if window <= 0 {
		return true
	}
	key := debounceKey{host: host, upstreamID: upstreamID, kind: kind}
	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()
	if last, ok := s.debounce[key]; ok && now.Sub(last) < window {
		return false
	}
	s.debounce[key] = now
	return true
}

// getOrCreateLocked returns the entry for (host, upstreamID), creating it on
// first touch. Caller must hold s.mu for write.
func (s *Scoreboard) getOrCreateLocked(host, upstreamID string) *entry {
	perUp, ok := s.entries[host]
	if !ok {
		perUp = make(map[string]*entry)
		s.entries[host] = perUp
	}
	e, ok := perUp[upstreamID]
	if !ok {
		e = &entry{}
		perUp[upstreamID] = e
	}
	return e
}

// DialFor is the listener-side entry point. It walks Pick → Dial → record
// in a loop bounded by the pool's retry cap, returning the first successful
// (conn, upstream id). On full failure it sets cascade for host and returns
// an aggregated error covering every per-attempt failure.
//
// Cancellation handling matches Pool.DialFor: if ctx is cancelled before any
// dial, return the cancellation directly without setting cascade; if it
// cancels mid-loop, return what we have without setting cascade either.
// Cascade is for "every upstream tried, all failed", not "the caller bailed".
func (s *Scoreboard) DialFor(ctx context.Context, network, addr string) (net.Conn, string, error) {
	host := hostOnly(addr)
	if s.cascadeActive(host, s.clock()) {
		return nil, "", &CascadeError{Host: host}
	}
	// Snapshot pool metadata under the read lock so a concurrent
	// ReplacePool cannot tear retryCap or candidateCount.
	_, retryCap, candidateCount := s.poolSnapshot()
	limit := retryCap
	if limit > candidateCount {
		limit = candidateCount
	}

	tried := make(map[string]bool, limit)
	var (
		attemptErrs []error
		attempts    int
	)
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			if attempts == 0 {
				return nil, "", fmt.Errorf("scoreboard: context canceled before any upstream was tried: %w", err)
			}
			attemptErrs = append(attemptErrs, fmt.Errorf("context canceled after %d attempt(s): %w", attempts, err))
			return nil, "", errors.Join(attemptErrs...)
		}
		up, err := s.Pick(host, tried)
		if err != nil {
			if errors.Is(err, ErrCascadeCooling) {
				return nil, "", err
			}
			break
		}
		start := s.clock()
		conn, dialErr := up.Dial(ctx, network, addr)
		latency := s.clock().Sub(start)
		attempts++
		if dialErr == nil {
			s.RecordSuccess(host, up.ID(), latency)
			s.observeDialSuccess(up.ID(), latency)
			s.logger.Info("upstream dial",
				"upstream_id", up.ID(),
				"host", host,
				"outcome", "success",
				"latency_ms", latency.Milliseconds(),
				"attempt", attempts,
			)
			return conn, up.ID(), nil
		}
		// Skip recording when the caller's context cut the dial short.
		// Either Canceled (client hung up) or DeadlineExceeded (the
		// caller's request deadline elapsed) reflects "bailed", not
		// "upstream could not reach the host", so penalizing the upstream
		// would be wrong. Checking ctx.Err() catches both shapes; the
		// parent context's state is the authoritative signal regardless
		// of how the dial error wraps it. Append an explicit "context
		// canceled" marker so the aggregated error makes the cause
		// readable instead of looking like a pure upstream failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			attemptErrs = append(attemptErrs, fmt.Errorf("%s: %w", up.ID(), dialErr))
			attemptErrs = append(attemptErrs, fmt.Errorf("context canceled after %d attempt(s): %w", attempts, ctxErr))
			return nil, "", errors.Join(attemptErrs...)
		}
		kind := failure.ClassifyDialError(dialErr)
		if kind != "" {
			s.RecordFailure(host, up.ID(), kind, nil)
		}
		s.observeDialFailure(up.ID(), kind, latency)
		s.logger.Warn("upstream dial",
			"upstream_id", up.ID(),
			"host", host,
			"outcome", "failure",
			"latency_ms", latency.Milliseconds(),
			"attempt", attempts,
			"kind", string(kind),
			"err", dialErr,
		)
		tried[up.ID()] = true
		attemptErrs = append(attemptErrs, fmt.Errorf("%s: %w", up.ID(), dialErr))
	}

	// Out of retries. Trip cascade and surface the aggregated error.
	s.TripCascade(host)
	return nil, "", fmt.Errorf(
		"scoreboard: all upstreams failed after %d attempt(s) (cap=%d, pool=%d): %w",
		attempts, retryCap, candidateCount, errors.Join(attemptErrs...),
	)
}

// Snapshot returns a copy of every (host, upstream) entry. Order is
// insertion-stable per host but not stable across hosts.
func (s *Scoreboard) Snapshot() []EntrySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]EntrySnapshot, 0)
	for host, perUp := range s.entries {
		for id, e := range perUp {
			out = append(out, EntrySnapshot{
				Host:          host,
				UpstreamID:    id,
				Score:         e.score,
				CooldownUntil: e.cooldownUntil,
				LastSeen:      e.lastSeen,
				GlobalSuccess: e.globalSuccessCount,
				GlobalFailure: e.globalFailureCount,
			})
		}
	}
	return out
}

// observeDialSuccess records a successful dial against the metrics sink.
// Split from the failure path so a post-dial failure that
// ClassifyDialError cannot tag (kind == "") is reported as "other"
// instead of being silently miscounted as a success.
func (s *Scoreboard) observeDialSuccess(upstreamID string, latency time.Duration) {
	if s.metrics == nil {
		return
	}
	s.metrics.ObserveDial(upstreamID, "success", latency)
}

// observeDialFailure records a failed dial against the metrics sink.
// Known kinds (KindRefused, KindTimeout) map to their named outcomes;
// any other value, including the empty Kind ClassifyDialError returns
// for unrecognized errors, falls into "other".
func (s *Scoreboard) observeDialFailure(upstreamID string, kind failure.Kind, latency time.Duration) {
	if s.metrics == nil {
		return
	}
	var outcome string
	switch kind {
	case failure.KindRefused:
		outcome = "refused"
	case failure.KindTimeout:
		outcome = "timeout"
	default:
		outcome = "other"
	}
	s.metrics.ObserveDial(upstreamID, outcome, latency)
}

// hostOnly returns the host portion of a host:port pair. Mirrors the helper
// in upstream/pool.go but kept package-local to avoid an import cycle.
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
