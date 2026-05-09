package mullvad

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

// ExpanderConfig is the inputs the [[upstream_pool]] expander needs to turn
// the Mullvad relay list into a slice of synthetic config.UpstreamConfig
// entries that the priority pool can consume. The fields are populated
// from the user's TOML by config.UpstreamPoolConfig.
//
// Countries is non-empty in production: config.Validate rejects an
// [[upstream_pool]] block without at least one country so operators cannot
// accidentally turn on all 50 Mullvad exit countries. Tests that drive
// ExpanderConfig directly may pass an empty list, in which case every
// relay matches.
type ExpanderConfig struct {
	IDPrefix        string        // required, prepended to each generated upstream id
	Priority        int           // applied to every expanded upstream
	Countries       []string      // required (post-Validate); empty accepts all (tests only)
	IncludeInactive bool          // false drops relays with active=false
	Refresh         time.Duration // refresh interval for Run; <= 0 disables polling
}

// Expander pulls Mullvad's relay list and turns it into a deterministic
// slice of UpstreamConfig values. Construct with NewExpander, call Snapshot
// for a one-shot expansion, or call Run from a goroutine to keep the
// expansion fresh on a ticker.
type Expander struct {
	cfg    ExpanderConfig
	client *Client
	log    *slog.Logger
}

// NewExpander returns an Expander backed by the supplied client. The client
// is required so callers can wire test transports or seed the disk cache.
func NewExpander(cfg ExpanderConfig, client *Client, logger *slog.Logger) (*Expander, error) {
	if cfg.IDPrefix == "" {
		return nil, errors.New("mullvad: id_prefix is required")
	}
	if client == nil {
		return nil, errors.New("mullvad: client is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Expander{cfg: cfg, client: client, log: logger}, nil
}

// Snapshot fetches the relay list and returns the filtered, transformed
// list of synthetic upstreams. The slice is sorted by id for stability.
func (e *Expander) Snapshot(ctx context.Context) ([]config.UpstreamConfig, error) {
	relays, err := e.client.Fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("mullvad expander: fetch: %w", err)
	}
	return e.transform(relays), nil
}

func (e *Expander) transform(relays []Relay) []config.UpstreamConfig {
	allowed := buildCountryFilter(e.cfg.Countries)
	out := make([]config.UpstreamConfig, 0, len(relays))
	// Allocate the priority pointer once for the whole snapshot. The value
	// is constant across every expanded upstream in this pool, and
	// UpstreamConfig.Priority is read-only after construction, so sharing
	// is safe. This avoids ~N heap allocations per refresh tick at
	// Mullvad's scale (560+ relays).
	priority := e.cfg.Priority
	priorityPtr := &priority
	for _, r := range relays {
		if !e.cfg.IncludeInactive && !r.Active {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[strings.ToLower(r.Country)]; !ok {
				continue
			}
		}
		addr, err := SOCKS5Address(r.Hostname)
		if err != nil {
			e.log.Warn("mullvad expander: skip relay with unexpected hostname",
				slog.String("hostname", r.Hostname),
				slog.Any("err", err),
			)
			continue
		}
		out = append(out, config.UpstreamConfig{
			ID:       e.cfg.IDPrefix + "-" + r.Hostname,
			Kind:     config.KindSOCKS5,
			Addr:     addr,
			Priority: priorityPtr,
		})
	}
	// Sort by upstream id so the slice's order is independent of the
	// Client implementation's relay ordering. parseResponse already sorts
	// by hostname, but a future Client could return relays in any order
	// and downstream code (and unit tests) want a deterministic answer.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func buildCountryFilter(countries []string) map[string]struct{} {
	if len(countries) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(countries))
	for _, c := range countries {
		out[strings.ToLower(strings.TrimSpace(c))] = struct{}{}
	}
	return out
}

// Run takes a Snapshot at startup and on every refresh interval, calling
// onSnapshot with the new list. The first snapshot is delivered
// synchronously so the caller can fail fast if Mullvad's API is
// unreachable at startup; subsequent failures are logged and the
// previous list stays in effect.
//
// When ExpanderConfig.Refresh is > 0, Run blocks until ctx is canceled,
// looping the ticker. When Refresh is <= 0 (polling disabled), Run
// returns nil right after delivering the initial snapshot. Errors from
// individual ticks are logged and do not stop the loop, so a transient
// API outage cannot kill the refresher.
func (e *Expander) Run(ctx context.Context, onSnapshot func([]config.UpstreamConfig)) error {
	if onSnapshot == nil {
		return errors.New("mullvad: onSnapshot callback is required")
	}
	snap, err := e.Snapshot(ctx)
	if err != nil {
		return err
	}
	onSnapshot(snap)
	return e.runRefreshLoop(ctx, snap, func(_, next []config.UpstreamConfig) {
		onSnapshot(next)
	})
}

// RunRefresh drives the periodic refresh ticker without taking an initial
// snapshot. The caller is expected to have already called Snapshot to seed
// the priority pool and passes that snapshot as prev. On each tick a fresh
// snapshot is fetched and onChange is invoked with (prev, next); prev is
// then updated to next so the next tick compares against the latest. Tick
// fetch failures log a warning, do NOT call onChange, and do not advance
// prev. Returns nil on ctx cancel; returns immediately with nil if
// refresh is <= 0.
func (e *Expander) RunRefresh(ctx context.Context, prev []config.UpstreamConfig, onChange func(prev, next []config.UpstreamConfig)) error {
	if onChange == nil {
		return errors.New("mullvad: onChange callback is required")
	}
	return e.runRefreshLoop(ctx, prev, onChange)
}

// runRefreshLoop is the shared ticker implementation behind Run and
// RunRefresh. The seed prev is what new ticks are compared against; after
// a successful tick prev is rebound to the new snapshot.
func (e *Expander) runRefreshLoop(ctx context.Context, prev []config.UpstreamConfig, onChange func(prev, next []config.UpstreamConfig)) error {
	if e.cfg.Refresh <= 0 {
		return nil
	}
	ticker := time.NewTicker(e.cfg.Refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			next, err := e.Snapshot(ctx)
			if err != nil {
				e.log.Warn("mullvad expander: refresh failed; keeping previous snapshot",
					slog.Any("err", err),
				)
				continue
			}
			onChange(prev, next)
			prev = next
		}
	}
}
