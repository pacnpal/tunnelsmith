// Package providers wires every supported [[upstream_pool]] provider
// into the registry and binds config.SetProviderValidator so a generic
// config.Validate run defers per-block validation to the provider's
// own rules.
//
// Importers do NOT call anything in this package directly. A blank
// import is enough:
//
//	import _ "github.com/pacnpal/tunnelsmith/internal/upstream/providers"
//
// cmd/tunnelsmith uses this. Tests that round-trip config.Parse for a
// real provider import it for the same reason. Tests that need an
// isolated registry build their own (provider.NewRegistry) and call
// config.SetProviderValidator themselves.
//
// To add a provider:
//  1. Implement provider.Provider in internal/upstream/<name>/.
//  2. Have that package's init() call provider.MustRegister.
//  3. Add a blank import below so the binary picks up the registration.
//
// See docs/providers.md for the full adapter-author walkthrough.
package providers

import (
	"fmt"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"

	// Each provider's init() calls provider.MustRegister, so this blank
	// import is what wires the binary to the provider. New providers
	// add a line here and nothing else in the cmd/ path.
	_ "github.com/pacnpal/tunnelsmith/internal/upstream/mullvad"
	_ "github.com/pacnpal/tunnelsmith/internal/upstream/webshare"
)

func init() {
	config.SetProviderValidator(func(cfg config.UpstreamPoolConfig) error {
		p, ok := provider.Default().Lookup(string(cfg.Provider))
		if !ok {
			return fmt.Errorf("provider %q is not supported (registered providers: %v)",
				cfg.Provider, provider.Default().Names())
		}
		return p.ValidateConfig(cfg)
	})
}
