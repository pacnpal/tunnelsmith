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
//
// When PlanID is set, Snapshot also probes /api/v3/proxy/list/status
// to retrieve the canonical (live) username/password for the plan. If
// the status creds differ from what /proxy/list/ returned, the
// canonical pair wins for every expanded upstream and the drift is
// logged once. This is the structural fix for the "rotated password,
// 407 storm" failure mode: Webshare's /proxy/list/ can serve stale
// per-proxy creds for a window after the operator updates them in
// the dashboard, and the v3 status endpoint always returns the
// post-rotation values. The operator override (proxy_username /
// proxy_password) still wins over the canonical pair so the manual
// escape hatch remains available for sub-user credentials or pinned
// values.
//
// When PlanID is empty the status probe is skipped (Webshare requires
// plan_id on /proxy/list/status), and per-proxy creds from the list
// are used as before.
//
// A failed status probe is non-fatal: the list-derived creds are used
// instead and a warning is logged. Auth-class failures from the list
// fetch propagate through ListProxies and never reach this branch.
func (e *Expander) Snapshot(ctx context.Context) ([]config.UpstreamConfig, error) {
	proxies, err := e.client.ListProxies(ctx, ListProxiesOptions{
		Mode:         e.cfg.Mode,
		PlanID:       e.cfg.PlanID,
		CountryCodes: e.cfg.CountryCodes,
	})
	if err != nil {
		return nil, fmt.Errorf("webshare expander: list: %w", err)
	}
	canonicalUser, canonicalPass := e.fetchCanonicalCreds(ctx, proxies)
	return e.transform(proxies, canonicalUser, canonicalPass), nil
}

// Heal forces a server-side proxy_list/refresh and re-snapshots. Use
// when accumulated failure signals (407 storm, mass connect-refused
// from rotated IPs) suggest the cached list is stale. The refresh
// call is best-effort: a quota-exhausted refresh still benefits from
// the immediate re-snapshot, which picks up canonical credentials from
// the v3 status endpoint even if the list itself isn't re-rotated.
//
// Heal is exported so future scoreboard/control wiring can drive it
// without poking package internals. The control listener already
// exposes a refresh-only path via provider.API; Heal is the richer
// "refresh + apply" form for in-binary callers.
func (e *Expander) Heal(ctx context.Context) ([]config.UpstreamConfig, error) {
	if err := e.client.RefreshProxyList(ctx, e.cfg.PlanID); err != nil {
		// A failed refresh shouldn't abort the heal: the very next
		// Snapshot still consults /proxy/list/status for canonical
		// creds, which is the higher-value half of the fix. Log
		// loudly so the operator sees the partial result.
		e.log.Warn("webshare expander: heal refresh failed; continuing to re-snapshot",
			slog.Any("err", err))
	}
	return e.Snapshot(ctx)
}

// fetchCanonicalCreds probes /api/v3/proxy/list/status for the live
// account credentials. Returns ("", "") when:
//
//   - PlanID is empty (status endpoint requires plan_id)
//   - the status probe fails for any reason (probe is best-effort)
//   - the status response carries empty creds (defensive: never apply
//     blanks to an upstream)
//
// When a non-empty pair is returned, the caller treats it as the
// authoritative source and overrides per-proxy values from
// /proxy/list/. proxies is supplied only so this function can log
// the first-observed drift, not to influence the return value.
func (e *Expander) fetchCanonicalCreds(ctx context.Context, proxies []Proxy) (string, string) {
	if e.cfg.PlanID == "" {
		return "", ""
	}
	status, err := e.client.FetchProxyListStatus(ctx, e.cfg.PlanID)
	if err != nil {
		// ListProxies already propagated auth failures upstream, so
		// here we treat any error as transient: log and fall through
		// to per-proxy creds. Don't include the planID in the log
		// (no secret, but cuts down on log volume).
		e.log.Warn("webshare expander: proxy list status probe failed; using per-proxy creds",
			slog.Any("err", err))
		return "", ""
	}
	if status.Username == "" || status.Password == "" {
		// Empty creds in a 200 response is undefined territory; refuse
		// to apply them so an unrelated API regression cannot blank
		// out every upstream's Proxy-Authorization header.
		e.log.Warn("webshare expander: proxy list status returned empty credentials; using per-proxy creds")
		return "", ""
	}
	if status.State != "" && status.State != "completed" {
		// state ∈ {validating, processing, completed, failed}; only
		// "completed" guarantees the IP pool reflects the latest
		// refresh. We still apply the canonical creds (those are
		// always current) and log so the operator can correlate
		// transient list churn.
		e.log.Info("webshare expander: proxy list status not yet completed",
			slog.String("state", status.State))
	}
	for _, p := range proxies {
		if p.Username != status.Username || p.Password != status.Password {
			// Logged at warn level because this is the canary for the
			// stale-creds → 407 storm: every drift event tells the
			// operator the manual override could be retired in favor
			// of the auto-heal path. Don't log the credential values
			// themselves.
			e.log.Warn("webshare expander: credential drift detected; applying canonical creds from /proxy/list/status",
				slog.String("first_drift_id", p.ID))
			break
		}
	}
	return status.Username, status.Password
}

func (e *Expander) transform(proxies []Proxy, canonicalUser, canonicalPass string) []config.UpstreamConfig {
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
		// Precedence (most specific wins):
		//   1. Operator override (proxy_username / proxy_password)
		//   2. Canonical creds from /proxy/list/status (the auto-heal)
		//   3. Per-proxy creds from /proxy/list/
		// Each tier exists for a different failure mode and the
		// ordering keeps explicit operator intent on top.
		username := p.Username
		password := p.Password
		if canonicalUser != "" && canonicalPass != "" {
			username = canonicalUser
			password = canonicalPass
		}
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
//
// Self-healing twist: after consecutive Snapshot failures the next
// attempt is delayed using exponential backoff (base, 2×, 4×, …)
// capped at backoffCap. This stops a hard outage at Webshare's API
// from hammering them at the configured tick rate while the running
// pool happily keeps serving the last known list. On the first
// successful Snapshot the interval snaps back to the configured base.
func (e *Expander) RunRefresh(ctx context.Context, prev []config.UpstreamConfig, onChange func(prev, next []config.UpstreamConfig)) error {
	if onChange == nil {
		return errors.New("webshare: onChange callback is required")
	}
	base := e.cfg.Refresh
	if base <= 0 {
		return nil
	}
	maxInterval := backoffCap(base)
	interval := base
	timer := time.NewTimer(interval)
	defer timer.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			next, err := e.Snapshot(ctx)
			if err != nil {
				consecutiveFailures++
				interval = backoffInterval(consecutiveFailures, base, maxInterval)
				e.log.Warn("webshare expander: refresh failed; keeping previous snapshot",
					slog.Any("err", err),
					slog.Int("consecutive_failures", consecutiveFailures),
					slog.Duration("next_attempt_in", interval),
				)
				timer.Reset(interval)
				continue
			}
			if consecutiveFailures > 0 {
				e.log.Info("webshare expander: refresh recovered",
					slog.Int("after_failures", consecutiveFailures))
			}
			consecutiveFailures = 0
			interval = base
			timer.Reset(interval)
			onChange(prev, next)
			prev = next
		}
	}
}

// backoffCap returns the maximum delay between refresh attempts after
// consecutive failures. Picks the smaller of 8× the configured base or
// 30 minutes, whichever is shorter. The 30-minute floor matters when
// operators configure a daily refresh (24h base): without it,
// 8× would push the cap out to over a week, which is longer than any
// operator-tolerable outage. The 8× ceiling matters when the base is
// short (e.g. 30s): without it, the cap would clamp early and lose
// the backoff shape entirely.
func backoffCap(base time.Duration) time.Duration {
	const hardCap = 30 * time.Minute
	limit := 8 * base
	if limit > hardCap {
		return hardCap
	}
	return limit
}

// backoffInterval returns the next sleep duration after a run of
// consecutive failures. consecutive starts at 1 for the first failure
// (giving 2^0 × base = base; the second failure gives 2×, etc.).
// Capped at maxInterval. Defends against overflow by clamping the
// shift count.
func backoffInterval(consecutive int, base, maxInterval time.Duration) time.Duration {
	if consecutive <= 0 {
		return base
	}
	shift := consecutive - 1
	const maxShift = 30 // 2^30 × base would overflow well past any cap
	if shift > maxShift {
		shift = maxShift
	}
	mult := time.Duration(1) << shift
	next := base * mult
	if next <= 0 || next > maxInterval {
		return maxInterval
	}
	return next
}
