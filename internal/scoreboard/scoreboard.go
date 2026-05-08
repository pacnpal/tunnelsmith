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
}

// FromConfig builds a scoreboard Config from the parsed [failure.scoring]
// section plus the [[failure.status]] entries. Phase 4 only fires the
// refused and timeout kinds from the dial path; the status-rule kinds and
// body-match kind are populated here for forward-compat so the kind→policy
// table is complete before Phase 5 wires them through the listener.
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

	// mu guards entries and cascade. Pick takes RLock; Record* and the
	// decay loop take Lock.
	mu      sync.RWMutex
	entries map[string]map[string]*entry // host -> upstreamID -> entry
	cascade map[string]time.Time         // host -> cascade-cooling expiry

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

// Start launches the time-decay goroutine. Stop or a cancelled ctx ends it.
// Calling Start twice without an intervening Stop is a no-op past the first
// call; the existing loop keeps running.
func (s *Scoreboard) Start(ctx context.Context) {
	s.decayMu.Lock()
	defer s.decayMu.Unlock()
	if s.decayCancel != nil {
		return
	}
	if s.cfg.DecayInterval <= 0 {
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
	go s.decayLoop(dctx, done)
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

func (s *Scoreboard) decayLoop(ctx context.Context, done chan struct{}) {
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
	t := time.NewTicker(s.cfg.DecayInterval)
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
// pressure is fine for v1.
func (s *Scoreboard) decayTick() {
	step := s.cfg.DecayStep
	if step <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
// tried may be nil; nil and empty are equivalent.
func (s *Scoreboard) Pick(host string, tried map[string]bool) (upstream.Upstream, error) {
	now := s.clock()
	if s.cascadeActive(host, now) {
		return nil, &CascadeError{Host: host}
	}
	candidates := s.poolEntries
	if len(candidates) == 0 {
		return nil, ErrPoolExhausted
	}
	type ranked struct {
		up       upstream.Upstream
		basePri  int
		score    float64
		cooled   bool
		untilT   time.Time
		untilSet bool
	}
	pool := make([]ranked, 0, len(candidates))
	s.mu.RLock()
	for _, c := range candidates {
		id := c.Up.ID()
		if tried[id] {
			continue
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
			up:       c.Up,
			basePri:  c.Priority,
			score:    score,
			cooled:   cooled,
			untilT:   untilT,
			untilSet: untilSet,
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
		// when nothing else is available.
		sort.Slice(cooled, func(i, j int) bool {
			return cooled[i].untilT.Before(cooled[j].untilT)
		})
		return cooled[0].up, nil
	}

	// Sort by score desc, base priority asc. Stable so the input order
	// (priority order from the pool) is the tiebreaker for entries with
	// no prior data.
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].score != eligible[j].score {
			return eligible[i].score > eligible[j].score
		}
		return eligible[i].basePri < eligible[j].basePri
	})

	// Probe roll: with prob ProbeChance, pick a non-top eligible candidate
	// uniformly at random so a previously-penalized upstream gets a chance
	// to recover. Skip when there is only one eligible.
	if len(eligible) > 1 && s.cfg.ProbeChance > 0 && s.probeRoll() {
		idx := s.probePick(len(eligible) - 1)
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
	if !s.acceptForDebounce(host, upstreamID, kind, now) {
		return
	}
	policy := s.cfg.KindPolicy[kind]
	overridden := cooldownOverride != nil
	cooldown := policy.Cooldown
	if overridden {
		cooldown = *cooldownOverride
	}
	s.mu.Lock()
	e := s.getOrCreateLocked(host, upstreamID)
	e.score -= policy.Penalty
	if e.score < -s.cfg.ScoreCap {
		e.score = -s.cfg.ScoreCap
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
// be applied now. False means the previous penalty was within DebounceWindow
// and this call should be a no-op.
func (s *Scoreboard) acceptForDebounce(host, upstreamID string, kind failure.Kind, now time.Time) bool {
	if s.cfg.DebounceWindow <= 0 {
		return true
	}
	key := debounceKey{host: host, upstreamID: upstreamID, kind: kind}
	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()
	if last, ok := s.debounce[key]; ok && now.Sub(last) < s.cfg.DebounceWindow {
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
	retryCap := s.poolRetryCap
	candidateCount := s.poolLen
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

// hostOnly returns the host portion of a host:port pair. Mirrors the helper
// in upstream/pool.go but kept package-local to avoid an import cycle.
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
