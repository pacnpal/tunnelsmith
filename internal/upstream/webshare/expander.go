package webshare

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

// ExpanderConfig holds the inputs the Expander needs. Populated from
// config.UpstreamPoolConfig by Provider.BuildExpander.
type ExpanderConfig struct {
	IDPrefix     string
	Priority     int
	Mode         string   // "direct" or "backbone"; default "direct"
	Kind         string   // "http" or "socks5"; default "http"
	PlanID       string   // optional
	CountryCodes []string // ISO 3166-1 alpha-2; empty = no filter
	Refresh      time.Duration

	// ProxyUsername / ProxyPassword override the per-proxy CONNECT
	// credentials returned by Webshare's /proxy/list/ endpoint. Both
	// must be set together. When empty, the expander threads through
	// the per-proxy values from the API response. The provider
	// resolves env-var indirection (proxy_username_env, etc.) before
	// populating these fields, so the expander only deals with
	// literal values.
	ProxyUsername string
	ProxyPassword string
}

// Expander pulls Webshare's proxy list and turns each entry into a
// config.UpstreamConfig. Construct with NewExpander and call Snapshot
// for one-shot expansion, or call RunRefresh from a goroutine to keep
// the expansion fresh on a ticker.
type Expander struct {
	cfg    ExpanderConfig
	client *Client
	log    *slog.Logger
}

// NewExpander returns an Expander backed by the supplied client.
func NewExpander(cfg ExpanderConfig, client *Client, logger *slog.Logger) (*Expander, error) {
	if cfg.IDPrefix == "" {
		return nil, errors.New("webshare: id_prefix is required")
	}
	if client == nil {
		return nil, errors.New("webshare: client is required")
	}
	mode := cfg.Mode
	if mode == "" {
		mode = "direct"
	}
	if mode != "direct" && mode != "backbone" {
		return nil, fmt.Errorf("webshare: mode %q is not one of direct, backbone", cfg.Mode)
	}
	kind := cfg.Kind
	if kind == "" {
		kind = string(config.KindHTTP)
	}
	if kind != string(config.KindHTTP) && kind != string(config.KindSOCKS5) {
		return nil, fmt.Errorf("webshare: kind %q is not one of http, socks5", cfg.Kind)
	}
	cfg.Mode = mode
	cfg.Kind = kind
	if logger == nil {
		logger = slog.Default()
	}
	return &Expander{cfg: cfg, client: client, log: logger}, nil
}

// Snapshot fetches the proxy list and transforms it. The slice is
// sorted by upstream id for stability across refresh ticks.
func (e *Expander) Snapshot(ctx context.Context) ([]config.UpstreamConfig, error) {
	proxies, err := e.client.ListProxies(ctx, ListProxiesOptions{
		Mode:         e.cfg.Mode,
		PlanID:       e.cfg.PlanID,
		CountryCodes: e.cfg.CountryCodes,
	})
	if err != nil {
		return nil, fmt.Errorf("webshare expander: list: %w", err)
	}
	return e.transform(proxies), nil
}

func (e *Expander) transform(proxies []Proxy) []config.UpstreamConfig {
	kind := config.UpstreamKind(e.cfg.Kind)
	out := make([]config.UpstreamConfig, 0, len(proxies))
	priority := e.cfg.Priority
	priorityPtr := &priority
	for _, p := range proxies {
		// Skip proxies the API has marked invalid. Webshare runs a
		// health check every 30s and flips valid=false on miss, so an
		// expired or temporarily-broken proxy stays out of the pool
		// until the next refresh tick observes it as healthy again.
		if !p.Valid {
			continue
		}
		if p.ProxyAddress == "" || p.Port < 1 || p.Port > 65535 {
			// Backbone mode returns a constant address; direct returns
			// per-proxy IPs. Either way a missing or out-of-range field
			// is a server glitch we'd rather skip than turn into a bad
			// upstream that would fail every dial.
			e.log.Warn("webshare expander: skip proxy with invalid address/port",
				slog.String("id", p.ID),
				slog.String("address", p.ProxyAddress),
				slog.Int("port", p.Port),
			)
			continue
		}
		username := p.Username
		password := p.Password
		// When the operator sets proxy_username / proxy_password (or
		// the env-var variants) on the [[upstream_pool]] block, those
		// values win over whatever the vendor returned. This is the
		// escape hatch for "Webshare's list shows stale credentials and
		// every CONNECT comes back 407" without forcing a full
		// list-refresh round-trip.
		if e.cfg.ProxyUsername != "" {
			username = e.cfg.ProxyUsername
		}
		if e.cfg.ProxyPassword != "" {
			password = e.cfg.ProxyPassword
		}
		out = append(out, config.UpstreamConfig{
			ID:       e.cfg.IDPrefix + "-" + p.ID,
			Kind:     kind,
			Addr:     p.HostPort(),
			Priority: priorityPtr,
			Username: username,
			Password: password,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Compare(out[i].ID, out[j].ID) < 0
	})
	return out
}

// RunRefresh drives the periodic refresh ticker. Matches the shape of
// mullvad.Expander.RunRefresh so the cmd/tunnelsmith pool composer can
// drive every provider through the same loop. prev seeds the first
// diff; onChange fires on every successful fetch.
func (e *Expander) RunRefresh(ctx context.Context, prev []config.UpstreamConfig, onChange func(prev, next []config.UpstreamConfig)) error {
	if onChange == nil {
		return errors.New("webshare: onChange callback is required")
	}
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
				e.log.Warn("webshare expander: refresh failed; keeping previous snapshot",
					slog.Any("err", err),
				)
				continue
			}
			onChange(prev, next)
			prev = next
		}
	}
}
