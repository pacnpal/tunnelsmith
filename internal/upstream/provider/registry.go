package provider

import (
	"fmt"
	"sort"
	"sync"
)

// Registry maps a provider name to its Provider implementation.
// Providers register themselves in package init() so cmd/tunnelsmith
// pulls in the side-effect import and gets the entry for free.
//
// The default registry is package-global because Provider registration
// is a build-time concern (one provider per import path) and the
// alternative — threading a registry through every config call —
// adds nothing but ceremony for a list with a handful of entries.
// Tests that need isolation construct their own Registry value.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry returns an empty Registry. Tests use this to avoid the
// global; production code uses Default().
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Register adds p under p.Name(). Returns an error if a provider with the
// same name is already registered; in init() functions, panic on the
// returned error so the duplicate is caught at startup.
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return fmt.Errorf("provider: cannot register nil provider")
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("provider: cannot register provider with empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[name]; ok {
		return fmt.Errorf("provider: %q is already registered", name)
	}
	r.providers[name] = p
	return nil
}

// Lookup returns the provider registered under name. The bool is false
// when no provider matches.
func (r *Registry) Lookup(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Names returns every registered provider name, sorted lexicographically.
// Used by the control listener for GET /v1/providers and by
// config.Validate's error messages.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for name := range r.providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// defaultRegistry is the package-global Registry every provider's init()
// writes into.
var defaultRegistry = NewRegistry()

// Default returns the package-global Registry. Provider init() functions
// register into this; cmd/tunnelsmith reads from it; config.Validate
// calls ValidateConfig through it.
func Default() *Registry { return defaultRegistry }

// MustRegister registers p into the default registry and panics on error.
// Intended for use in package init().
func MustRegister(p Provider) {
	if err := defaultRegistry.Register(p); err != nil {
		panic(err)
	}
}
