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

	// composer is wired after construction because newPoolComposer runs
	// later in main() than the scoreboard hook the driver is registered
	// under. composerMu guards the swap; reads from runHeal take it.
	composerMu sync.Mutex
	composer   poolUpdater

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
	}
}

// setComposer wires the composer the driver will route healed snapshots
// through. Set once at startup after newPoolComposer returns; the
// driver buffers events and defers cooldown advancement when the
// composer is still nil (see bump) so the first burst after startup
// heals correctly.
func (d *authHealDriver) setComposer(c poolUpdater) {
	d.composerMu.Lock()
	d.composer = c
	d.composerMu.Unlock()
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
	d.composerMu.Lock()
	composerReady := d.composer != nil
	d.composerMu.Unlock()
	if !composerReady {
		return
	}
	d.lastHeal = now
	d.inFlight = true
	d.events = nil
	go d.runHeal()
}

// runHeal invokes Healer.Heal under a bounded context and forwards the
// resulting snapshot through the pool composer. Failures log a warn
// line but never panic; the inFlight flag is cleared in any case so a
// subsequent burst can re-arm. Logs include the upstream count so an
// operator watching the journal can correlate the heal with the
// follow-on pool size.
func (d *authHealDriver) runHeal() {
	defer func() {
		d.mu.Lock()
		d.inFlight = false
		d.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), d.callTimeout)
	defer cancel()
	next, err := d.healer.Heal(ctx)
	if err != nil {
		d.logger.Warn("auth heal failed", "err", err)
		return
	}
	d.logger.Info("auth heal: pool refreshed after KindProxyAuth burst",
		"upstreams", len(next))
	d.composerMu.Lock()
	composer := d.composer
	d.composerMu.Unlock()
	if composer == nil {
		// Should not happen in production (main wires the composer
		// before any traffic flows), but defensive: log and bail
		// rather than nil-deref.
		d.logger.Warn("auth heal: composer not wired; skipping pool hot-swap")
		return
	}
	applied, err := composer.Update(d.idPrefix, next)
	if err != nil {
		d.logger.Warn("auth heal: pool hot-swap failed", "err", err)
		return
	}
	if applied {
		d.logger.Info("auth heal: pool hot-swap applied", "upstreams", len(next))
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

// connectAuthHealDriversToComposer wires composer into every driver
// after newPoolComposer has built it. Composer may not exist at
// scoreboard-construction time, so drivers buffer events until this
// call lands; bump() defers cooldown advancement when composer is
// nil so the first burst after startup heals correctly.
func connectAuthHealDriversToComposer(drivers []*authHealDriver, composer poolUpdater) {
	for _, d := range drivers {
		d.setComposer(composer)
	}
}

// validateNoStaticPoolPrefixCollision refuses to start the binary when
// a static [[upstream]] id starts with any auto-heal pool's id_prefix
// followed by "-". Without the guard, a static upstream named
// "ws-fixed" (when a [[upstream_pool]] block uses id_prefix="ws")
// would feed 407 events into the wrong driver and fire a Webshare
// Heal in response to failures from an entirely unrelated upstream.
// Failing at startup is preferable to silently routing the events
// incorrectly: the operator either renames the static upstream or
// changes the pool's id_prefix and learns which fix they wanted.
func validateNoStaticPoolPrefixCollision(static []config.UpstreamConfig, drivers []*authHealDriver) error {
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
	return nil
}
