package mullvad

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"
)

// ProviderName is the [[upstream_pool]] provider value this package
// registers under.
const ProviderName = "mullvad"

// Provider adapts the Mullvad expander to the provider abstraction.
// Mullvad has no vendor API to expose so BuildAPI returns
// provider.ErrAPINotSupported.
type Provider struct{}

// NewProvider returns the Mullvad Provider value. Used by init() and by
// tests that build a non-default registry.
func NewProvider() *Provider { return &Provider{} }

// Name reports the registry key.
func (p *Provider) Name() string { return ProviderName }

// ValidateConfig runs the Mullvad-specific block checks. Generic checks
// (id_prefix non-empty, refresh non-negative, cache_path absolute) are
// already enforced by config.Validate; this method covers the rest.
//
// Countries is the headline rule: an empty list would mean "every
// Mullvad relay across every country" which is almost certainly a
// configuration accident (relay count is ~500), so reject it.
func (p *Provider) ValidateConfig(cfg config.UpstreamPoolConfig) error {
	if len(cfg.Countries) == 0 {
		return fmt.Errorf("mullvad: countries must list at least one country")
	}
	for i, c := range cfg.Countries {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("mullvad: countries[%d] is empty or whitespace", i)
		}
	}
	return nil
}

// BuildExpander returns a Mullvad expander wrapped to satisfy
// provider.Expander. The Client + Cache wiring matches what
// cmd/tunnelsmith used to do inline.
func (p *Provider) BuildExpander(cfg config.UpstreamPoolConfig, logger *slog.Logger) (provider.Expander, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// Caller's logger is expected to already carry provider /
	// id_prefix context (cmd/tunnelsmith threads those at the
	// expandPool layer). Add only the component tag here so the
	// id_prefix attribute doesn't double-up in structured logs.
	expLogger := logger.With("component", "mullvad-expander")
	client := NewClient()
	client.Logger = expLogger
	if cfg.CachePath != "" {
		client.Cache = &Cache{Path: cfg.CachePath}
	}
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:        cfg.IDPrefix,
		Priority:        cfg.PriorityValue(),
		Countries:       cfg.Countries,
		IncludeInactive: cfg.IncludeInactive,
		Refresh:         cfg.RefreshDuration(),
	}, client, expLogger)
	if err != nil {
		return nil, err
	}
	return exp, nil
}

// BuildAPI returns ErrAPINotSupported: Mullvad publishes the relay list
// as a public JSON endpoint, but it does not expose anything the operator
// can "control" the way Webshare's POST /api/v2/proxy/list/refresh
// rotates a pool. Surfacing this through the registry lets the control
// listener answer GET /v1/providers with a clear capability flag and
// reject POST /v1/providers/mullvad/refresh with 501 rather than 404.
func (p *Provider) BuildAPI(_ config.UpstreamPoolConfig, _ *slog.Logger) (provider.API, error) {
	return nil, provider.ErrAPINotSupported
}

func init() { provider.MustRegister(NewProvider()) }
