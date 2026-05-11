// Package provider declares the contracts an [[upstream_pool]] provider
// implements. Concrete providers (mullvad, webshare, …) sit in sibling
// packages and register themselves into the package-global Registry via
// their own init().
//
// The split is three small interfaces so a new provider only writes what
// it actually needs:
//
//   - Provider builds the other two from one UpstreamPoolConfig block. It
//     also owns per-provider config validation, so config.Validate can
//     stay generic and the provider-specific rules (Webshare needs an
//     API token, Mullvad needs at least one country, …) live next to
//     the provider's other code.
//
//   - Expander is the existing Mullvad shape: Snapshot returns the current
//     UpstreamConfig fan-out and RunRefresh drives the periodic refresh
//     ticker the binary's pool composer subscribes to. Every provider has
//     one; this is the path that fills the priority pool.
//
//   - API is the optional control surface. Providers like Webshare expose
//     a vendor REST API the operator can poke (force a list refresh,
//     check profile/subscription). Providers like Mullvad have nothing
//     to expose here and return ErrAPINotSupported. The control listener
//     surfaces this through POST /v1/providers/{name}/refresh.
//
// The Mullvad expander predates this abstraction; mullvad.Provider adapts
// the existing shape so behavior is unchanged. New providers should
// implement these interfaces directly.
package provider

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

// ErrAPINotSupported is returned by Provider.BuildAPI for providers that
// have no vendor API to expose (currently mullvad). The control listener
// translates this into 501 Not Implemented on
// POST /v1/providers/{name}/refresh.
var ErrAPINotSupported = errors.New("provider: vendor API not supported")

// Expander turns a provider's view of the world into the
// config.UpstreamConfig slice the priority pool consumes. The contract
// matches what cmd/tunnelsmith already drives for Mullvad so the pool
// composer didn't need to change shape.
type Expander interface {
	// Snapshot fetches the current set of synthetic upstreams. Called
	// once at startup to seed the pool and again on every refresh tick
	// via RunRefresh. The slice must be deterministic for stable input
	// (sorted by id) so the composer's diff is meaningful.
	Snapshot(ctx context.Context) ([]config.UpstreamConfig, error)

	// RunRefresh drives the periodic refresh ticker. The seed prev is
	// what the first tick compares against (the caller passes the
	// startup Snapshot result). onChange fires on every successful
	// fresh fetch with (prev, next); the implementation rebinds prev
	// to next after each call so the next tick compares against the
	// most-recent observation. Returns nil on ctx cancel or when the
	// configured refresh interval is <= 0.
	RunRefresh(ctx context.Context, prev []config.UpstreamConfig, onChange func(prev, next []config.UpstreamConfig)) error
}

// RefreshOptions selects which provider plan/list to refresh. Webshare's
// API accepts an optional plan_id; PlanID is forwarded verbatim. Empty
// string asks the provider to use its default plan.
type RefreshOptions struct {
	PlanID string
}

// API is the optional vendor-API control surface. Providers that have
// nothing to expose return ErrAPINotSupported from Provider.BuildAPI.
//
// The surface is intentionally small for v1: only RefreshProxyList ships,
// since that is the action with the highest operator value (force a new
// IP pool when an upstream is poisoned). Future endpoints (Profile,
// Subscription, Plan) drop into this interface without breaking the
// control listener — the listener just dispatches by route.
type API interface {
	// RefreshProxyList asks the vendor to rotate / regenerate the proxy
	// list. The provider's Expander picks up the new list on the next
	// refresh tick. Implementations should be idempotent enough that an
	// operator hitting the route twice does not break.
	//
	// Errors are surfaced verbatim; the control handler wraps them
	// into 5xx responses without leaking the API token.
	RefreshProxyList(ctx context.Context, opts RefreshOptions) error
}

// Provider is the registry entry for one [[upstream_pool]] provider. It
// builds the Expander and (optionally) the API from a single
// UpstreamPoolConfig block plus a logger.
//
// ValidateConfig runs at config.Parse time so a misconfigured block
// (e.g. webshare missing api_token) fails fast, before the binary tries
// to talk to the vendor. The generic config.Validate calls
// Registry.Lookup(provider).ValidateConfig for each block.
type Provider interface {
	// Name is the [[upstream_pool]].provider string this provider
	// registers under. Lowercase, no whitespace; matches what an
	// operator writes in TOML.
	Name() string

	// ValidateConfig checks the block's provider-specific fields.
	// The caller has already validated the generic fields
	// (id_prefix, refresh, etc.).
	ValidateConfig(cfg config.UpstreamPoolConfig) error

	// BuildExpander constructs the Expander for this block. The
	// returned value must be ready to Snapshot immediately; the
	// caller will call Snapshot once at startup before wiring
	// RunRefresh.
	BuildExpander(cfg config.UpstreamPoolConfig, logger *slog.Logger) (Expander, error)

	// BuildAPI constructs the vendor API surface for this block.
	// Providers with no vendor API return (nil, ErrAPINotSupported).
	BuildAPI(cfg config.UpstreamPoolConfig, logger *slog.Logger) (API, error)
}
