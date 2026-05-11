package provider_test

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"
)

type fakeProvider struct {
	name string
}

func (f *fakeProvider) Name() string                                     { return f.name }
func (f *fakeProvider) ValidateConfig(_ config.UpstreamPoolConfig) error { return nil }
func (f *fakeProvider) BuildExpander(_ config.UpstreamPoolConfig, _ *slog.Logger) (provider.Expander, error) {
	return nil, nil
}
func (f *fakeProvider) BuildAPI(_ config.UpstreamPoolConfig, _ *slog.Logger) (provider.API, error) {
	return nil, provider.ErrAPINotSupported
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := provider.NewRegistry()
	p := &fakeProvider{name: "fake"}
	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("fake")
	if !ok {
		t.Fatal("Lookup: expected provider to be present")
	}
	if got != p {
		t.Fatalf("Lookup: got %v want %v", got, p)
	}
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	r := provider.NewRegistry()
	if err := r.Register(&fakeProvider{name: "fake"}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(&fakeProvider{name: "fake"}); err == nil {
		t.Fatal("second Register: expected duplicate error, got nil")
	}
}

func TestRegistryRejectsNilAndEmpty(t *testing.T) {
	r := provider.NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("Register(nil): expected error, got nil")
	}
	if err := r.Register(&fakeProvider{name: ""}); err == nil {
		t.Fatal("Register with empty name: expected error, got nil")
	}
}

func TestRegistryNamesAreSorted(t *testing.T) {
	r := provider.NewRegistry()
	for _, name := range []string{"zeta", "alpha", "mu"} {
		if err := r.Register(&fakeProvider{name: name}); err != nil {
			t.Fatalf("Register %q: %v", name, err)
		}
	}
	got := r.Names()
	want := []string{"alpha", "mu", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names: got %v want %v", got, want)
	}
}

func TestRegistryLookupMissReturnsFalse(t *testing.T) {
	r := provider.NewRegistry()
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("Lookup(missing): expected ok=false")
	}
}

func TestErrAPINotSupportedSurfaces(t *testing.T) {
	r := provider.NewRegistry()
	if err := r.Register(&fakeProvider{name: "noapi"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, _ := r.Lookup("noapi")
	_, err := p.BuildAPI(config.UpstreamPoolConfig{}, slog.Default())
	if !errors.Is(err, provider.ErrAPINotSupported) {
		t.Fatalf("expected ErrAPINotSupported, got %v", err)
	}
	// Sanity check on the Expander side: zero-value Provider returns
	// (nil, nil) so callers must check for nil explicitly. This guards
	// against callers assuming an Expander is always returned.
	exp, err := p.BuildExpander(config.UpstreamPoolConfig{}, slog.Default())
	if err != nil {
		t.Fatalf("BuildExpander: %v", err)
	}
	if exp != nil {
		t.Fatalf("BuildExpander: expected nil expander from fake, got %v", exp)
	}
	_ = context.Background()
}
