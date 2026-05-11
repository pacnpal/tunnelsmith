package webshare

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"
)

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
	hasInline := cfg.APIToken != ""
	hasFile := cfg.APITokenFile != ""
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
		if len(code) != 2 {
			return fmt.Errorf("webshare: country_codes[%d] %q must be a two-letter ISO 3166-1 alpha-2 code", i, code)
		}
	}
	return nil
}

// BuildExpander loads the API token (from inline or file), builds a
// Client, and wraps it in an Expander.
func (p *Provider) BuildExpander(cfg config.UpstreamPoolConfig, logger *slog.Logger) (provider.Expander, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "webshare-expander", "id_prefix", cfg.IDPrefix)
	client, err := buildClient(cfg, logger)
	if err != nil {
		return nil, err
	}
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:     cfg.IDPrefix,
		Priority:     cfg.PriorityValue(),
		Mode:         cfg.Mode,
		Kind:         cfg.Kind,
		PlanID:       cfg.PlanID,
		CountryCodes: cfg.CountryCodes,
		Refresh:      cfg.RefreshDuration(),
	}, client, logger)
	if err != nil {
		return nil, err
	}
	return exp, nil
}

// BuildAPI returns the vendor API surface. Webshare's surface is one
// call (RefreshProxyList) plus Profile, which the control endpoint
// uses as a "ping" to validate credentials.
func (p *Provider) BuildAPI(cfg config.UpstreamPoolConfig, logger *slog.Logger) (provider.API, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "webshare-api", "id_prefix", cfg.IDPrefix)
	client, err := buildClient(cfg, logger)
	if err != nil {
		return nil, err
	}
	return &apiAdapter{client: client, planID: cfg.PlanID}, nil
}

// buildClient is shared by BuildExpander and BuildAPI so the same
// token-load + cache-wiring lives in one place. The Client is
// goroutine-safe — same instance is fine across both callers.
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

func resolveToken(cfg config.UpstreamPoolConfig) (string, error) {
	switch {
	case cfg.APIToken != "":
		return cfg.APIToken, nil
	case cfg.APITokenFile != "":
		return LoadTokenFile(cfg.APITokenFile)
	default:
		return "", errors.New("webshare: no api_token or api_token_file configured")
	}
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
	return a.client.RefreshProxyList(ctx, planID)
}

func init() { provider.MustRegister(NewProvider()) }
