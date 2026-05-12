package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"
)

// Auto-heal trigger defaults. Picked so the typical password-rotation
// pattern (every upstream in the pool starts 407ing within seconds of
// each other) fires the heal quickly, while a single flaky proxy or
// transient blip doesn't churn the pool. Tunable knobs are not exposed
// in TOML yet — operators who hit a case the defaults don't cover can
// ask for config.
const (
	authHealThreshold   = 3                // burst size in window
	authHealWindow      = 30 * time.Second // sliding window
	authHealCooldown    = 60 * time.Second // min gap between heal runs
	authHealCallTimeout = 60 * time.Second // bound the Heal RPC
)

// poolUpdater is the small slice of poolComposer the driver depends on
// — just the hot-swap entry point. Factored as an interface so the
// driver's unit tests can inject a no-op without standing up the full
// composer dependency graph (scoreboard, listener, metrics, gauges).
// The production *poolComposer satisfies this implicitly via its
// existing Update method.
type poolUpdater interface {
	Update(idPrefix string, next []config.UpstreamConfig) (bool, error)
}

// authHealDriver observes KindProxyAuth events for one [[upstream_pool]]
// block and triggers Healer.Heal when a burst suggests credentials have
// rotated. One driver per block: each filters Observe events to its
// own id_prefix so a 407 from pool A never triggers a heal on pool B.
//
// The driver is a thin coordinator: count events in a sliding window,
// gate via cooldown to avoid runaway heals, and on threshold spawn a
// goroutine that calls Healer.Heal and forwards the result to the
// pool composer (same hot-swap path the scheduled refresh tick uses).
// In-flight tracking ensures only one heal runs at a time per pool.
type authHealDriver struct {
	idPrefix string
	healer   provider.Healer
	logger   *slog.Logger

	threshold   int
	window      time.Duration
	cooldown    time.Duration
	callTimeout time.Duration
	clock       func() time.Time

	// composer + parentCtx are wired after construction because
	// newPoolComposer and the errgroup context both materialise later
	// in main() than the scoreboard hook the driver is registered
	// under. wireMu guards both; reads from runHeal take it. parentCtx
	// defaults to context.Background() so a driver that's never wired
	// (test paths, defensive) still produces a valid context, but in
	// production main() always upgrades it to gctx so a SIGTERM cancels
	// in-flight heal RPCs cleanly.
	wireMu    sync.Mutex
	composer  poolUpdater
	parentCtx context.Context

	mu       sync.Mutex
	events   []time.Time
	lastHeal time.Time
	inFlight bool
}

// newAuthHealDriver returns a driver scoped to one pool block. composer
// is set later via setComposer once newPoolComposer has run.
func newAuthHealDriver(idPrefix string, healer provider.Healer, logger *slog.Logger) *authHealDriver {
	if logger == nil {
		logger = slog.Default()
	}
	return &authHealDriver{
		idPrefix:    idPrefix,
		healer:      healer,
		logger:      logger.With("component", "auth-heal-driver", "id_prefix", idPrefix),
		threshold:   authHealThreshold,
		window:      authHealWindow,
		cooldown:    authHealCooldown,
		callTimeout: authHealCallTimeout,
		clock:       time.Now,
		parentCtx:   context.Background(),
	}
}

// setComposer wires the composer the driver will route healed snapshots
// through. Set once at startup after newPoolComposer returns; the
// driver buffers events and defers cooldown advancement when the
// composer is still nil (see bump) so the first burst after startup
// heals correctly.
func (d *authHealDriver) setComposer(c poolUpdater) {
	d.wireMu.Lock()
	d.composer = c
	d.wireMu.Unlock()
}

// setParentContext wires the shutdown-aware parent context the driver
// derives its per-heal timeout from. main() calls this with the
// errgroup context built from signal.NotifyContext(os.Interrupt,
// syscall.SIGTERM), so a SIGTERM (or Ctrl-C) cancels any in-flight
// Healer.Heal RPC. SIGHUP is handled separately as a config-reload
// signal and does NOT cancel gctx, so an in-flight heal continues
// across a reload. Defaults to context.Background() before this
// call fires; only the brief startup window between scoreboard.New
// and connectAuthHealDriversToComposer can observe the default, and
// the driver's composer-nil deferral keeps that window safe.
func (d *authHealDriver) setParentContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	d.wireMu.Lock()
	d.parentCtx = ctx
	d.wireMu.Unlock()
}

// Observe is the scoreboard FailureHook callback. Cheap and non-blocking:
// the bump path takes a short-held lock and may dispatch a goroutine if
// the threshold tripped. Events for kinds other than KindProxyAuth or
// for upstream IDs outside this driver's id_prefix are dropped without
// touching state.
func (d *authHealDriver) Observe(host, upstreamID string, kind failure.Kind) {
	if kind != failure.KindProxyAuth {
		return
	}
	if !strings.HasPrefix(upstreamID, d.idPrefix+"-") {
		return
	}
	d.bump()
}

// bump records one event and, when the threshold has been hit within the
// sliding window AND the cooldown has elapsed AND no heal is in flight
// AND the composer has already been wired, kicks off a goroutine that
// calls Healer.Heal. The events slice gets trimmed to the window on
// every bump so it cannot grow unboundedly.
//
// The composer-nil check is deliberately the last gate before committing
// state changes (advancing lastHeal, clearing events, marking inFlight).
// During the brief startup window between scoreboard.New (which
// registers the failure hook) and connectAuthHealDriversToComposer
// (which wires the composer), a burst of 407s reaching this point
// would otherwise burn the cooldown on a no-op heal and leave the
// driver unable to react once the composer becomes available.
func (d *authHealDriver) bump() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	cutoff := now.Add(-d.window)
	keep := d.events[:0]
	for _, t := range d.events {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now)
	// Bound the events slice at threshold. Only the count within the
	// window matters for the firing decision (>= threshold triggers);
	// keeping more than threshold timestamps wastes memory and CPU
	// during a sustained 407 storm in cooldown, where bumps keep
	// arriving but cannot fire a heal. Capping at threshold turns the
	// per-Observe scan from O(events-in-window) into O(threshold).
	if len(keep) > d.threshold {
		keep = keep[len(keep)-d.threshold:]
	}
	d.events = keep
	if len(d.events) < d.threshold {
		return
	}
	if d.inFlight {
		return
	}
	if !d.lastHeal.IsZero() && now.Sub(d.lastHeal) < d.cooldown {
		return
	}
	// Composer-ready check: if the composer isn't wired yet, leave the
	// events buffered (don't clear) and skip cooldown advancement so a
	// subsequent bump after the composer lands fires immediately. The
	// 407 storm at startup is exactly the scenario this branch
	// protects against.
	d.wireMu.Lock()
	composerReady := d.composer != nil
	d.wireMu.Unlock()
	if !composerReady {
		return
	}
	// inFlight blocks concurrent dispatches while the heal goroutine
	// runs; lastHeal is set at runHeal's completion (in its defer
	// handler) so the cooldown gate measures the gap from when one
	// heal *finishes* to when the next *starts*, matching the
	// "let one heal complete and propagate" intent. Setting lastHeal
	// at dispatch instead would let a heal that took most of
	// callTimeout re-arm immediately on completion.
	d.inFlight = true
	d.events = nil
	go d.runHeal()
}

// runHeal invokes Healer.Heal under a bounded context derived from the
// driver's parent context, then forwards the resulting snapshot through
// the pool composer. Failures log a warn line but never panic; the
// inFlight flag is cleared in any case so a subsequent burst can
// re-arm. The success log fires only AFTER composer.Update lands so a
// later reader of the journal cannot mistake a fetched-but-not-applied
// heal for a successful pool rotation. Logs include the upstream count
// so an operator watching the journal can correlate the heal with the
// follow-on pool size.
func (d *authHealDriver) runHeal() {
	defer func() {
		// lastHeal is set HERE (completion time) so the cooldown
		// measures the gap from one heal's completion to the next
		// heal's dispatch — not from dispatch to dispatch. Without
		// this, a Heal RPC that ate most of callTimeout would let
		// the next heal fire immediately on completion because
		// now-sub-lastHeal already exceeds cooldown.
		d.mu.Lock()
		d.inFlight = false
		d.lastHeal = d.clock()
		d.mu.Unlock()
	}()
	d.wireMu.Lock()
	parent := d.parentCtx
	composer := d.composer
	d.wireMu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, d.callTimeout)
	defer cancel()
	next, err := d.healer.Heal(ctx)
	if err != nil {
		d.logger.Warn("auth heal failed", "err", err)
		return
	}
	d.logger.Debug("auth heal: snapshot fetched", "upstreams", len(next))
	if composer == nil {
		// Should not happen in production (main wires the composer
		// before any traffic flows), but defensive: log and bail
		// rather than nil-deref. Logged at warn because if we ever
		// reach here it means the bump composer-ready gate slipped.
		d.logger.Warn("auth heal: composer not wired; skipping pool hot-swap")
		return
	}
	applied, err := composer.Update(d.idPrefix, next)
	if err != nil {
		d.logger.Warn("auth heal: pool hot-swap failed", "err", err)
		return
	}
	if applied {
		d.logger.Info("auth heal: pool hot-swap applied after KindProxyAuth burst",
			"upstreams", len(next))
	} else {
		d.logger.Debug("auth heal: pool unchanged after heal", "upstreams", len(next))
	}
}

// fanoutFailureHook builds a scoreboard.FailureHook that broadcasts
// each event to every driver. Drivers self-filter by id_prefix so a
// single hook routes correctly across multiple pool blocks. Returns
// nil when there are no drivers, which lets the caller skip
// registering a hook entirely (no overhead for non-webshare setups).
func fanoutFailureHook(drivers []*authHealDriver) func(host, upstreamID string, kind failure.Kind) {
	if len(drivers) == 0 {
		return nil
	}
	return func(host, upstreamID string, kind failure.Kind) {
		for _, d := range drivers {
			d.Observe(host, upstreamID, kind)
		}
	}
}

// buildAuthHealDrivers walks the pool blocks and constructs one driver
// per block whose Expander implements provider.Healer. Returns nil
// when no block opts in; callers should treat that as "no auto-heal
// configured" and skip the hook registration.
//
// The function imports nothing from concrete provider packages: type
// assertion against provider.Healer is the only mechanism for
// detecting capability, which keeps cmd/tunnelsmith honest about the
// interface contract.
func buildAuthHealDrivers(blocks []*poolBlock, logger *slog.Logger) []*authHealDriver {
	var drivers []*authHealDriver
	for _, b := range blocks {
		healer, ok := b.exp.(provider.Healer)
		if !ok {
			continue
		}
		drivers = append(drivers, newAuthHealDriver(b.idPrefix, healer, logger))
	}
	return drivers
}

// connectAuthHealDriversToComposer wires composer AND the parent
// context into every driver after newPoolComposer has built it.
// Composer may not exist at scoreboard-construction time, so drivers
// buffer events until this call lands; bump() defers cooldown
// advancement when composer is nil so the first burst after startup
// heals correctly. parentCtx is main's errgroup context, which is
// cancelled on SIGTERM / os.Interrupt (not SIGHUP — that's a
// separate reload signal). Cancelling propagates into the per-heal
// timeout context so an in-flight Healer.Heal RPC is cancelled
// instead of dangling past shutdown.
func connectAuthHealDriversToComposer(drivers []*authHealDriver, parentCtx context.Context, composer poolUpdater) {
	for _, d := range drivers {
		// Order matters: parentCtx FIRST, composer LAST. bump() only
		// dispatches a heal once it observes composer != nil. If
		// composer landed before parentCtx, a heal dispatched in
		// the tiny window between the two assignments would inherit
		// the default context.Background() parent and never honor
		// a SIGTERM. Setting parentCtx first means composer-nil is
		// still the gate, and by the time composer flips non-nil
		// the parent is already shutdown-aware.
		d.setParentContext(parentCtx)
		d.setComposer(composer)
	}
}

// validateNoPoolPrefixCollision refuses to start the binary when any
// upstream id (static or generated by ANY pool, with or without a
// Healer) would be routed into the wrong auto-heal driver by the
// strings.HasPrefix dispatch in Observe.
//
// Three collision shapes are caught:
//
//  1. Static [[upstream]] id starts with an auto-heal driver's
//     id_prefix + "-". Example: a static upstream named "ws-fixed"
//     plus a [[upstream_pool]] with id_prefix="ws". A 407 from
//     "ws-fixed" would fire a Webshare Heal even though the static
//     upstream isn't a Webshare proxy.
//
//  2. Two auto-heal driver prefixes where one's id_prefix is a
//     prefix-with-dash of the other's. id_prefix in config can
//     contain "-" (per the idPrefixRe regex), so blocks like
//     id_prefix="ws" and id_prefix="ws-east" would have the "ws"
//     driver match every event "ws-east-d-…" and trigger heals on
//     the wrong provider.
//
//  3. A non-Healer pool's id_prefix is a sub-prefix-with-dash of a
//     driver's id_prefix (or vice-versa). The non-Healer pool has
//     no driver of its own, but its upstream ids (e.g. mullvad
//     relays "ws-foo-mlv-1" if id_prefix="ws-foo") would still
//     match a "ws-" driver's matcher, firing Webshare heals on
//     mullvad failures. allPoolPrefixes carries every
//     [[upstream_pool]].id_prefix (Healer or not) so this case is
//     caught at startup.
//
// Failing at startup is preferable to silent mis-routing: the
// operator either renames a static upstream or changes a pool's
// id_prefix and learns which fix they wanted.
func validateNoPoolPrefixCollision(static []config.UpstreamConfig, drivers []*authHealDriver, allPoolPrefixes []string) error {
	for _, d := range drivers {
		prefix := d.idPrefix + "-"
		for _, u := range static {
			if strings.HasPrefix(u.ID, prefix) {
				return fmt.Errorf(
					"auth-heal driver: static [[upstream]] id %q collides with [[upstream_pool]] id_prefix %q (a 407 from the static upstream would trigger a vendor Heal on the pool); rename the static upstream or change the pool's id_prefix",
					u.ID, d.idPrefix,
				)
			}
		}
	}
	// Driver vs every-pool-prefix (including non-Healer pools): a
	// driver's matcher "D-" must not be a prefix of ANY pool's
	// matcher "P-" for P != D. Catches both driver-vs-driver and
	// driver-vs-non-Healer cases in one pass.
	for _, d := range drivers {
		dMatcher := d.idPrefix + "-"
		for _, p := range allPoolPrefixes {
			if p == d.idPrefix {
				continue // same pool; the driver matching its own pool is the whole point
			}
			pMatcher := p + "-"
			if strings.HasPrefix(pMatcher, dMatcher) {
				return fmt.Errorf(
					"auth-heal driver: [[upstream_pool]] id_prefix %q is a sub-prefix of id_prefix %q (events for %q would also fire heals on the %q pool); rename one of the pools so neither id_prefix starts with the other followed by '-'",
					d.idPrefix, p, p, d.idPrefix,
				)
			}
		}
	}
	return nil
}
