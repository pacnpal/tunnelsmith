package webshare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"
)

func TestProviderName(t *testing.T) {
	if name := (&Provider{}).Name(); name != "webshare" {
		t.Fatalf("Name = %q, want %q", name, "webshare")
	}
}

func TestProviderValidateConfig(t *testing.T) {
	p := &Provider{}
	t.Run("inline token ok", func(t *testing.T) {
		err := p.ValidateConfig(config.UpstreamPoolConfig{
			Provider: "webshare", IDPrefix: "ws", APIToken: "tok",
			CountryCodes: []string{"US", "DE"},
		})
		if err != nil {
			t.Fatalf("ValidateConfig: %v", err)
		}
	})
	t.Run("missing token", func(t *testing.T) {
		err := p.ValidateConfig(config.UpstreamPoolConfig{IDPrefix: "ws"})
		if err == nil || !strings.Contains(err.Error(), "api_token") {
			t.Fatalf("ValidateConfig: got %v, want token error", err)
		}
	})
	t.Run("whitespace-only inline token rejected", func(t *testing.T) {
		// "   " passes a naive != "" check; the TrimSpace guard
		// in ValidateConfig is what makes this fail fast at
		// config-load instead of as a 401 from the vendor.
		err := p.ValidateConfig(config.UpstreamPoolConfig{
			IDPrefix: "ws", APIToken: "   ",
		})
		if err == nil || !strings.Contains(err.Error(), "api_token") {
			t.Fatalf("ValidateConfig: got %v, want token error", err)
		}
	})
	t.Run("both token and file", func(t *testing.T) {
		err := p.ValidateConfig(config.UpstreamPoolConfig{
			IDPrefix: "ws", APIToken: "x", APITokenFile: "/abs/y",
		})
		if err == nil || !strings.Contains(err.Error(), "only one") {
			t.Fatalf("ValidateConfig: got %v, want exclusive error", err)
		}
	})
	t.Run("countries field rejected", func(t *testing.T) {
		err := p.ValidateConfig(config.UpstreamPoolConfig{
			IDPrefix: "ws", APIToken: "tok",
			Countries: []string{"USA"},
		})
		if err == nil || !strings.Contains(err.Error(), "country_codes") {
			t.Fatalf("ValidateConfig: got %v, want countries error", err)
		}
	})
	t.Run("invalid country code length", func(t *testing.T) {
		err := p.ValidateConfig(config.UpstreamPoolConfig{
			IDPrefix: "ws", APIToken: "tok",
			CountryCodes: []string{"USA"},
		})
		if err == nil || !strings.Contains(err.Error(), "two-letter") {
			t.Fatalf("ValidateConfig: got %v, want ISO error", err)
		}
	})
	t.Run("invalid country code non-letter", func(t *testing.T) {
		for _, bad := range []string{"1!", "  ", "U ", "U1"} {
			err := p.ValidateConfig(config.UpstreamPoolConfig{
				IDPrefix: "ws", APIToken: "tok",
				CountryCodes: []string{bad},
			})
			if err == nil || !strings.Contains(err.Error(), "ASCII letters only") {
				t.Errorf("ValidateConfig(%q): got %v, want non-letter error", bad, err)
			}
		}
	})
	t.Run("country codes case-insensitive accepted", func(t *testing.T) {
		// Lowercase codes are accepted at validate time; the API
		// call normalises them to uppercase before sending.
		err := p.ValidateConfig(config.UpstreamPoolConfig{
			IDPrefix: "ws", APIToken: "tok",
			CountryCodes: []string{"us", "Gb", "DE"},
		})
		if err != nil {
			t.Fatalf("ValidateConfig: %v", err)
		}
	})
	t.Run("invalid mode", func(t *testing.T) {
		err := p.ValidateConfig(config.UpstreamPoolConfig{
			IDPrefix: "ws", APIToken: "tok", Mode: "weird",
		})
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("ValidateConfig: got %v, want mode error", err)
		}
	})
	t.Run("invalid kind", func(t *testing.T) {
		err := p.ValidateConfig(config.UpstreamPoolConfig{
			IDPrefix: "ws", APIToken: "tok", Kind: "wireguard",
		})
		if err == nil || !strings.Contains(err.Error(), "kind") {
			t.Fatalf("ValidateConfig: got %v, want kind error", err)
		}
	})
}

// TestProviderAPIWrapsRateLimited locks in the contract the control
// listener relies on: a Webshare-side 429 must surface as
// provider.ErrAPIRateLimited at the API boundary so the control
// handler's errors.Is check maps it to HTTP 429 without importing
// this package.
func TestProviderAPIWrapsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	p := &Provider{}
	api, err := p.BuildAPI(config.UpstreamPoolConfig{
		Provider: "webshare", IDPrefix: "ws", APIToken: "tok",
	}, nil)
	if err != nil {
		t.Fatalf("BuildAPI: %v", err)
	}
	a := api.(*apiAdapter)
	a.client.BaseURL = srv.URL
	a.client.HTTPClient = srv.Client()
	gotErr := a.RefreshProxyList(context.Background(), provider.RefreshOptions{})
	if !errors.Is(gotErr, provider.ErrAPIRateLimited) {
		t.Fatalf("RefreshProxyList err = %v, want wrapping provider.ErrAPIRateLimited", gotErr)
	}
	// The wrapped vendor error must still be reachable so logs and
	// JSON envelopes carry the webshare-specific message.
	if !errors.Is(gotErr, ErrRateLimited) {
		t.Fatalf("RefreshProxyList err = %v, want also wrapping webshare.ErrRateLimited", gotErr)
	}
}

func TestProviderBuildAPIRefreshes(t *testing.T) {
	var hit int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	// Override BaseURL by injecting a custom Client. The simplest
	// way is to inline: call buildClient under our test config, then
	// retarget BaseURL.
	p := &Provider{}
	cfg := config.UpstreamPoolConfig{
		Provider: "webshare", IDPrefix: "ws", APIToken: "tok",
	}
	api, err := p.BuildAPI(cfg, nil)
	if err != nil {
		t.Fatalf("BuildAPI: %v", err)
	}
	a, ok := api.(*apiAdapter)
	if !ok {
		t.Fatalf("BuildAPI returned %T, want *apiAdapter", api)
	}
	a.client.BaseURL = srv.URL
	a.client.HTTPClient = srv.Client()
	if err := a.RefreshProxyList(context.Background(), provider.RefreshOptions{}); err != nil {
		t.Fatalf("RefreshProxyList: %v", err)
	}
	if hit != 1 {
		t.Fatalf("expected 1 refresh call, got %d", hit)
	}
}

func TestProviderBuildExpanderLoadsTokenFile(t *testing.T) {
	tokPath := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokPath, []byte("from-file"), 0o600); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token from-file" {
			t.Errorf("Authorization = %q, want %q", got, "Token from-file")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 0, "results": []Proxy{},
		})
	}))
	t.Cleanup(srv.Close)

	p := &Provider{}
	exp, err := p.BuildExpander(config.UpstreamPoolConfig{
		Provider: "webshare", IDPrefix: "ws", APITokenFile: tokPath,
	}, nil)
	if err != nil {
		t.Fatalf("BuildExpander: %v", err)
	}
	// The expander wraps a Client; retarget it so the Snapshot
	// actually hits our httptest server. We do this through the
	// concrete type because that's what BuildExpander returns
	// internally.
	concrete, ok := exp.(*Expander)
	if !ok {
		t.Fatalf("BuildExpander returned %T, want *Expander", exp)
	}
	concrete.client.BaseURL = srv.URL
	concrete.client.HTTPClient = srv.Client()
	if _, err := concrete.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
}

func TestProviderValidateProxyCreds(t *testing.T) {
	p := &Provider{}
	base := config.UpstreamPoolConfig{
		Provider: "webshare", IDPrefix: "ws", APIToken: "tok",
	}
	t.Run("both unset is fine", func(t *testing.T) {
		if err := p.ValidateConfig(base); err != nil {
			t.Fatalf("ValidateConfig: %v", err)
		}
	})
	t.Run("inline pair is fine", func(t *testing.T) {
		cfg := base
		cfg.ProxyUsername = "u"
		cfg.ProxyPassword = "p"
		if err := p.ValidateConfig(cfg); err != nil {
			t.Fatalf("ValidateConfig: %v", err)
		}
	})
	t.Run("env pair is fine", func(t *testing.T) {
		cfg := base
		cfg.ProxyUsernameEnv = "WS_USER"
		cfg.ProxyPasswordEnv = "WS_PASS"
		if err := p.ValidateConfig(cfg); err != nil {
			t.Fatalf("ValidateConfig: %v", err)
		}
	})
	t.Run("mixed inline + env across creds is fine", func(t *testing.T) {
		// One credential inline, the other via env is allowed: each
		// credential picks its own source. The pairing rule only
		// requires that both credentials are set, not that they use
		// the same source.
		cfg := base
		cfg.ProxyUsername = "u"
		cfg.ProxyPasswordEnv = "WS_PASS"
		if err := p.ValidateConfig(cfg); err != nil {
			t.Fatalf("ValidateConfig: %v", err)
		}
	})
	t.Run("inline + env on same credential rejected", func(t *testing.T) {
		cfg := base
		cfg.ProxyUsername = "u"
		cfg.ProxyUsernameEnv = "WS_USER"
		cfg.ProxyPassword = "p"
		err := p.ValidateConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "only one") {
			t.Fatalf("ValidateConfig: got %v, want exclusivity error", err)
		}
	})
	t.Run("half-set pair rejected", func(t *testing.T) {
		cfg := base
		cfg.ProxyUsername = "u" // password missing
		err := p.ValidateConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "must be set together") {
			t.Fatalf("ValidateConfig: got %v, want pairing error", err)
		}
	})
}

func TestProviderBuildExpanderResolvesProxyCredsFromEnv(t *testing.T) {
	t.Setenv("WS_PROXY_USER_TEST", "env-user")
	t.Setenv("WS_PROXY_PASS_TEST", "env-pass")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 1,
			"results": []Proxy{
				{ID: "d-1", Username: "vendor", Password: "vendor", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
			},
		})
	}))
	t.Cleanup(srv.Close)

	p := &Provider{}
	exp, err := p.BuildExpander(config.UpstreamPoolConfig{
		Provider:         "webshare",
		IDPrefix:         "ws",
		APIToken:         "tok",
		ProxyUsernameEnv: "WS_PROXY_USER_TEST",
		ProxyPasswordEnv: "WS_PROXY_PASS_TEST",
	}, nil)
	if err != nil {
		t.Fatalf("BuildExpander: %v", err)
	}
	concrete := exp.(*Expander)
	concrete.client.BaseURL = srv.URL
	concrete.client.HTTPClient = srv.Client()

	got, err := concrete.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 1 || got[0].Username != "env-user" || got[0].Password != "env-pass" {
		t.Fatalf("expected env-resolved creds on the upstream, got %+v", got)
	}
}

func TestProviderBuildExpanderTrimsResolvedProxyCreds(t *testing.T) {
	// Both branches of resolveCred must strip surrounding whitespace
	// before the value reaches the upstream's Proxy-Authorization
	// header. A trailing newline or space in a Docker compose
	// environment: line (or an accidentally-padded TOML literal)
	// would otherwise produce a 407 indistinguishable from a real
	// credential mismatch.
	t.Setenv("WS_PROXY_USER_PADDED", "  env-user  \n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 1,
			"results": []Proxy{
				{ID: "d-1", Username: "vendor", Password: "vendor", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
			},
		})
	}))
	t.Cleanup(srv.Close)

	p := &Provider{}
	exp, err := p.BuildExpander(config.UpstreamPoolConfig{
		Provider:         "webshare",
		IDPrefix:         "ws",
		APIToken:         "tok",
		ProxyUsernameEnv: "WS_PROXY_USER_PADDED",
		ProxyPassword:    "  inline-pass  ",
	}, nil)
	if err != nil {
		t.Fatalf("BuildExpander: %v", err)
	}
	concrete := exp.(*Expander)
	concrete.client.BaseURL = srv.URL
	concrete.client.HTTPClient = srv.Client()

	got, err := concrete.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d upstreams, want 1", len(got))
	}
	if got[0].Username != "env-user" {
		t.Errorf("env-resolved username = %q, want %q (trimmed)", got[0].Username, "env-user")
	}
	if got[0].Password != "inline-pass" {
		t.Errorf("inline password = %q, want %q (trimmed)", got[0].Password, "inline-pass")
	}
}

func TestProviderBuildExpanderRejectsMissingEnv(t *testing.T) {
	// The env-var path must fail closed when the named variable is
	// unset. Otherwise a typo in the env name would silently produce
	// upstreams with empty credentials and surface as a 407 storm at
	// runtime, exactly the symptom this feature is supposed to fix.
	t.Setenv("WS_PROXY_USER_PRESENT", "user")
	// WS_PROXY_PASS_MISSING deliberately not set.

	p := &Provider{}
	_, err := p.BuildExpander(config.UpstreamPoolConfig{
		Provider:         "webshare",
		IDPrefix:         "ws",
		APIToken:         "tok",
		ProxyUsernameEnv: "WS_PROXY_USER_PRESENT",
		ProxyPasswordEnv: "WS_PROXY_PASS_MISSING",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "WS_PROXY_PASS_MISSING") {
		t.Fatalf("BuildExpander: got %v, want unset-env error naming WS_PROXY_PASS_MISSING", err)
	}
}

func TestProviderBuildAPITokenError(t *testing.T) {
	p := &Provider{}
	// Both empty: BuildExpander/BuildAPI must fail before any
	// HTTP call leaves the binary. ErrTokenMissing is the
	// sentinel resolveToken returns so the assertion uses
	// errors.Is rather than a brittle substring match.
	_, err := p.BuildAPI(config.UpstreamPoolConfig{IDPrefix: "ws"}, nil)
	if !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("BuildAPI: got %v, want ErrTokenMissing", err)
	}
}
