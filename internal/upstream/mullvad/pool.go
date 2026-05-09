package mullvad

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

// ExpanderConfig is the inputs the [[upstream_pool]] expander needs to turn
// the Mullvad relay list into a slice of synthetic config.UpstreamConfig
// entries that the priority pool can consume. The fields are populated
// from the user's TOML by config.UpstreamPoolConfig.
type ExpanderConfig struct {
	IDPrefix        string        // required, prepended to each generated upstream id
	Priority        int           // applied to every expanded upstream
	Countries       []string      // case-insensitive country filter; empty means accept all
	IncludeInactive bool          // false drops relays with active=false
	Refresh         time.Duration // refresh interval for Run; <= 0 means refresh once and exit
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
	priority := e.cfg.Priority
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
		p := priority
		out = append(out, config.UpstreamConfig{
			ID:       e.cfg.IDPrefix + "-" + r.Hostname,
			Kind:     config.KindSOCKS5,
			Addr:     addr,
			Priority: &p,
		})
	}
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
// onSnapshot with the new list. It blocks until ctx is canceled. Errors
// from individual ticks are logged and do not stop the loop, so a
// transient API outage cannot kill the refresher.
//
// The first snapshot is delivered synchronously so the caller can fail
// fast if Mullvad's API is unreachable at startup. Subsequent failures
// are logged and the previous list stays in effect.
func (e *Expander) Run(ctx context.Context, onSnapshot func([]config.UpstreamConfig)) error {
	if onSnapshot == nil {
		return errors.New("mullvad: onSnapshot callback is required")
	}
	snap, err := e.Snapshot(ctx)
	if err != nil {
		return err
	}
	onSnapshot(snap)
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
			onSnapshot(next)
		}
	}
}
