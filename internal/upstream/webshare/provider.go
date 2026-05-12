package webshare

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"
)

// ErrTokenMissing is returned by buildClient when neither api_token nor
// api_token_file is set. Surfaced as a sentinel so tests and callers
// can match it with errors.Is rather than string-matching the message.
var ErrTokenMissing = errors.New("webshare: no api_token or api_token_file configured")

// ProviderName is the [[upstream_pool]] provider value this package
// registers under.
const ProviderName = "webshare"

// Provider implements provider.Provider for Webshare. It wires
// BuildExpander to webshare.Expander and BuildAPI to a small wrapper
// over Client.RefreshProxyList so the control listener can drive the
// vendor's on-demand refresh.
type Provider struct{}

// NewProvider returns the Webshare provider value.
func NewProvider() *Provider { return &Provider{} }

// Name reports the registry key.
func (p *Provider) Name() string { return ProviderName }

// ValidateConfig enforces Webshare-specific block invariants. Generic
// checks (id_prefix, refresh, cache_path) run before this in
// config.Validate; this method covers the rest.
//
//   - Exactly one of api_token / api_token_file must be set (the
//     binary cannot talk to the vendor without a token).
//   - Mode must be empty (defaults to "direct"), "direct", or
//     "backbone".
//   - Kind must be empty (defaults to "http"), "http", or "socks5".
//   - CountryCodes entries must be two-letter ISO codes. Mullvad's
//     human-readable Countries list is rejected here so the operator
//     fixes the obvious typo at startup instead of getting an empty
//     list at runtime.
func (p *Provider) ValidateConfig(cfg config.UpstreamPoolConfig) error {
	// Trim before the presence check so a whitespace-only api_token
	// (e.g. "   " — easy to land via accidental copy/paste from a
	// dashboard) fails at config-load instead of as a generic 401
	// from the vendor at first request. This matches the
	// LoadTokenFile contract, which already rejects empty-after-trim.
	hasInline := strings.TrimSpace(cfg.APIToken) != ""
	hasFile := strings.TrimSpace(cfg.APITokenFile) != ""
	if !hasInline && !hasFile {
		return errors.New("webshare: one of api_token or api_token_file is required")
	}
	if hasInline && hasFile {
		return errors.New("webshare: set only one of api_token or api_token_file")
	}
	switch cfg.Mode {
	case "", "direct", "backbone":
	default:
		return fmt.Errorf("webshare: mode %q is not one of direct, backbone", cfg.Mode)
	}
	switch cfg.Kind {
	case "", string(config.KindHTTP), string(config.KindSOCKS5):
	default:
		return fmt.Errorf("webshare: kind %q is not one of http, socks5", cfg.Kind)
	}
	if len(cfg.Countries) > 0 {
		// Webshare's vendor API filters by ISO codes ("US", "DE"), not
		// human-readable names. Surface the mismatch at validate time
		// so the operator picks the right field instead of seeing an
		// empty pool at startup.
		return errors.New("webshare: use country_codes (ISO 3166-1 alpha-2) instead of countries")
	}
	for i, code := range cfg.CountryCodes {
		if !isISOAlpha2(code) {
			return fmt.Errorf("webshare: country_codes[%d] %q must be a two-letter ISO 3166-1 alpha-2 code (ASCII letters only)", i, code)
		}
	}
	if err := validateProxyCreds(cfg); err != nil {
		return err
	}
	return nil
}

// validateProxyCreds enforces the inline/env exclusivity rule and the
// pairing rule for proxy_username and proxy_password. Both credentials
// are optional overall: a config with neither field set is fine, and
// keeps the per-proxy values from the vendor's /proxy/list/ response.
// When one credential is set the other must also be set so the dialer
// never sends a half-populated Proxy-Authorization header.
//
// Env-var fields name the variable, not the value: tunnelsmith reads
// the named variable at BuildExpander time. Resolution happens there
// (not here) so that ValidateConfig stays side-effect free; this
// function only checks for misconfiguration shapes.
func validateProxyCreds(cfg config.UpstreamPoolConfig) error {
	uInline := strings.TrimSpace(cfg.ProxyUsername) != ""
	uEnv := strings.TrimSpace(cfg.ProxyUsernameEnv) != ""
	pInline := strings.TrimSpace(cfg.ProxyPassword) != ""
	pEnv := strings.TrimSpace(cfg.ProxyPasswordEnv) != ""
	if uInline && uEnv {
		return errors.New("webshare: set only one of proxy_username or proxy_username_env")
	}
	if pInline && pEnv {
		return errors.New("webshare: set only one of proxy_password or proxy_password_env")
	}
	uSet := uInline || uEnv
	pSet := pInline || pEnv
	if uSet != pSet {
		return errors.New("webshare: proxy_username and proxy_password must be set together (both inline or both via _env)")
	}
	return nil
}

// isISOAlpha2 reports whether s is exactly two ASCII letters. Webshare's
// API expects ISO 3166-1 alpha-2 codes; values like "1!" or " U" or
// "USA" would silently produce zero matches at runtime, so reject them
// at config-load.
func isISOAlpha2(s string) bool {
	if len(s) != 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// BuildExpander loads the API token (from inline or file), builds a
// Client, and wraps it in an Expander.
func (p *Provider) BuildExpander(cfg config.UpstreamPoolConfig, logger *slog.Logger) (provider.Expander, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// Caller's logger already carries provider / id_prefix
	// context (cmd/tunnelsmith threads those at expandPool). Add
	// only the component tag here so id_prefix doesn't double-up.
	logger = logger.With("component", "webshare-expander")
	client, err := buildClient(cfg, logger)
	if err != nil {
		return nil, err
	}
	proxyUser, proxyPass, err := resolveProxyCreds(cfg)
	if err != nil {
		return nil, err
	}
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:      cfg.IDPrefix,
		Priority:      cfg.PriorityValue(),
		Mode:          cfg.Mode,
		Kind:          cfg.Kind,
		PlanID:        cfg.PlanID,
		CountryCodes:  cfg.CountryCodes,
		Refresh:       cfg.RefreshDuration(),
		ProxyUsername: proxyUser,
		ProxyPassword: proxyPass,
	}, client, logger)
	if err != nil {
		return nil, err
	}
	return exp, nil
}

// BuildAPI returns the vendor API surface. The provider.API interface
// only declares RefreshProxyList in v1, so that is the single method
// the control endpoint dispatches against. The webshare.Client also
// exposes Profile and other methods directly for future expansion;
// they are not part of the control surface today.
func (p *Provider) BuildAPI(cfg config.UpstreamPoolConfig, logger *slog.Logger) (provider.API, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// Caller already carries provider / id_prefix context; see the
	// note in BuildExpander above.
	logger = logger.With("component", "webshare-api")
	client, err := buildClient(cfg, logger)
	if err != nil {
		return nil, err
	}
	return &apiAdapter{client: client, planID: cfg.PlanID}, nil
}

// buildClient centralises the token-load + cache-wiring so both
// BuildExpander and BuildAPI share one construction path. Each call
// allocates a fresh *Client: the Client struct is read-only after
// construction, has no shared mutable state, and the cache uses an
// atomic tmp-and-rename write, so two separate clients pointed at
// the same cache path are safe. The cost (one extra token-file read
// at startup) is negligible and avoids the lifecycle questions
// caching would introduce.
func buildClient(cfg config.UpstreamPoolConfig, logger *slog.Logger) (*Client, error) {
	token, err := resolveToken(cfg)
	if err != nil {
		return nil, err
	}
	c := NewClient()
	c.APIToken = token
	c.Logger = logger
	if cfg.CachePath != "" {
		c.Cache = &Cache{Path: cfg.CachePath}
	}
	return c, nil
}

// resolveProxyCreds returns the literal username/password to stamp into
// every expanded upstream, or ("", "", nil) when the operator has not
// asked for an override. The env-var path reads at startup so a
// rotated secret only needs a restart (matches the rest of the
// Webshare token-handling contract). Both credentials must be set
// together; validateProxyCreds enforces that earlier in the load path,
// but this function double-checks the resolved values to catch the
// "env var named but unset" case that ValidateConfig cannot see.
func resolveProxyCreds(cfg config.UpstreamPoolConfig) (string, string, error) {
	user, err := resolveCred(cfg.ProxyUsername, cfg.ProxyUsernameEnv, "proxy_username")
	if err != nil {
		return "", "", err
	}
	pass, err := resolveCred(cfg.ProxyPassword, cfg.ProxyPasswordEnv, "proxy_password")
	if err != nil {
		return "", "", err
	}
	if (user == "") != (pass == "") {
		// One side resolved to empty (env var unset, most likely) while
		// the other carries a value. The same pairing rule from
		// validateProxyCreds applies post-resolution.
		return "", "", fmt.Errorf("webshare: proxy_username and proxy_password must both resolve to non-empty values")
	}
	return user, pass, nil
}

// resolveCred returns the literal value behind an inline-or-env field
// pair. inlineVal wins when set; envVar names an environment variable
// to look up (an empty result is an error so a typo in the env-var name
// fails fast instead of silently producing empty credentials).
func resolveCred(inlineVal, envVar, field string) (string, error) {
	if v := strings.TrimSpace(inlineVal); v != "" {
		return v, nil
	}
	if e := strings.TrimSpace(envVar); e != "" {
		val := os.Getenv(e)
		if strings.TrimSpace(val) == "" {
			return "", fmt.Errorf("webshare: %s_env points at %q but the variable is unset or empty", field, e)
		}
		return val, nil
	}
	return "", nil
}

func resolveToken(cfg config.UpstreamPoolConfig) (string, error) {
	// Trim both fields so a whitespace-padded value never reaches
	// the Authorization header or LoadTokenFile (which would error
	// on a path like "   "). ValidateConfig already rejects
	// empty-after-trim for both, but this is defence in depth for
	// callers that bypass Provider.ValidateConfig (tests, future
	// code paths) and matches the LoadTokenFile contract.
	if t := strings.TrimSpace(cfg.APIToken); t != "" {
		return t, nil
	}
	if path := strings.TrimSpace(cfg.APITokenFile); path != "" {
		return LoadTokenFile(path)
	}
	return "", ErrTokenMissing
}

// apiAdapter is the provider.API implementation. Holds the planID at
// construction time so the route handler does not need to thread it
// through every call.
type apiAdapter struct {
	client *Client
	planID string
}

func (a *apiAdapter) RefreshProxyList(ctx context.Context, opts provider.RefreshOptions) error {
	planID := opts.PlanID
	if planID == "" {
		planID = a.planID
	}
	return wrapVendorErr(a.client.RefreshProxyList(ctx, planID))
}

// wrapVendorErr translates Webshare-specific typed errors into the
// cross-provider sentinels declared in package provider so the control
// listener can dispatch on errors.Is without importing this package
// (which would form an import cycle through internal/upstream/providers).
//
// %w on both sides preserves the wrapped chain: the control handler
// sees provider.ErrAPIRateLimited; an operator reading the JSON
// response body still sees the Webshare-specific message.
func wrapVendorErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrRateLimited):
		return fmt.Errorf("%w: %w", provider.ErrAPIRateLimited, err)
	default:
		return err
	}
}

func init() { provider.MustRegister(NewProvider()) }
