package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"
)

// ProviderAPIBinding ties one [[upstream_pool]] block's id_prefix to
// the vendor API it exposes (when the provider has one). cmd/tunnelsmith
// builds one binding per pool block at startup, wraps the slice in a
// ProviderRegistry, and hands it to control.NewServer through
// ServerOptions.Providers so the control endpoint can dispatch
// POST /v1/providers/{id_prefix}/refresh through the right adapter.
type ProviderAPIBinding struct {
	// IDPrefix is the [[upstream_pool]].id_prefix the operator wrote
	// in TOML. Routed on this value rather than provider name so an
	// operator running two Webshare pool blocks (e.g. different
	// plans) can refresh them independently.
	IDPrefix string
	// Provider is the registry key ("mullvad", "webshare", …). Used
	// in the listing response so the operator sees what flavor a
	// binding is.
	Provider string
	// API is the vendor surface; nil when the provider exposes none.
	// A nil API still surfaces in GET /v1/providers so the operator
	// can confirm the block is wired, but POST .../refresh returns
	// 501 Not Implemented.
	API provider.API
}

// ProviderRegistry holds the bindings the control endpoint dispatches
// against. Bindings are static for the lifetime of the process (pool
// shape is restart-frozen for v1); the registry is goroutine-safe so
// future hot-add of a provider is a single-method change.
type ProviderRegistry struct {
	mu       sync.RWMutex
	bindings []ProviderAPIBinding
}

// NewProviderRegistry returns a registry seeded with the given
// bindings. cmd/tunnelsmith builds one slice and hands it over;
// tests pass a single fake.
func NewProviderRegistry(bindings []ProviderAPIBinding) *ProviderRegistry {
	cp := make([]ProviderAPIBinding, len(bindings))
	copy(cp, bindings)
	return &ProviderRegistry{bindings: cp}
}

// Lookup returns the binding for the given id_prefix.
func (r *ProviderRegistry) Lookup(idPrefix string) (ProviderAPIBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range r.bindings {
		if b.IDPrefix == idPrefix {
			return b, true
		}
	}
	return ProviderAPIBinding{}, false
}

// List returns every registered binding, sorted by id_prefix so the
// HTTP response is stable.
func (r *ProviderRegistry) List() []ProviderAPIBinding {
	r.mu.RLock()
	out := make([]ProviderAPIBinding, len(r.bindings))
	copy(out, r.bindings)
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return strings.Compare(out[i].IDPrefix, out[j].IDPrefix) < 0
	})
	return out
}

// providerListEntry is the JSON shape returned by GET /v1/providers.
type providerListEntry struct {
	IDPrefix string `json:"id_prefix"`
	Provider string `json:"provider"`
	HasAPI   bool   `json:"has_api"`
}

// refreshResponse is the JSON shape returned by
// POST /v1/providers/{id_prefix}/refresh on success.
type refreshResponse struct {
	IDPrefix string `json:"id_prefix"`
	Provider string `json:"provider"`
	Status   string `json:"status"`
}

// refreshErrorResponse is the JSON shape returned on error. Used so an
// operator scripting against the endpoint gets a structured error
// instead of plain text.
type refreshErrorResponse struct {
	IDPrefix string `json:"id_prefix"`
	Provider string `json:"provider"`
	Error    string `json:"error"`
}

// mountProvidersHandlers attaches /v1/providers and
// /v1/providers/{id_prefix}/refresh to mux. Both routes are gated by
// the same bearer-token check that protects /v1/report; an operator
// who already configured [control].auth_tokens does not need extra
// config to lock down the new routes.
//
// Routing uses a prefix match because Go 1.22's pattern-based mux is
// not the convention this repo uses elsewhere (the report handler is
// HandleFunc on an exact path). Sticking with the same style keeps
// the file mentally cohesive with handlers.go.
func mountProvidersHandlers(mux *http.ServeMux, registry *ProviderRegistry, tokens TokenSource, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	mux.HandleFunc("/v1/providers", func(w http.ResponseWriter, r *http.Request) {
		if !runProviderAuth(w, r, tokens) {
			return
		}
		handleProvidersList(w, r, registry)
	})
	mux.HandleFunc("/v1/providers/", func(w http.ResponseWriter, r *http.Request) {
		if !runProviderAuth(w, r, tokens) {
			return
		}
		// Path shape: /v1/providers/{id_prefix}/refresh
		tail := strings.TrimPrefix(r.URL.Path, "/v1/providers/")
		parts := strings.Split(tail, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "expected /v1/providers/{id_prefix}/refresh", http.StatusNotFound)
			return
		}
		idPrefix, action := parts[0], parts[1]
		switch action {
		case "refresh":
			handleProviderRefresh(w, r, registry, idPrefix, logger)
		default:
			http.Error(w, fmt.Sprintf("unknown provider action %q (supported: refresh)", action), http.StatusNotFound)
		}
	})
}

func runProviderAuth(w http.ResponseWriter, r *http.Request, tokens TokenSource) bool {
	if tokens == nil {
		return true
	}
	snap := tokens.Snapshot()
	if !snap.Enabled() {
		return true
	}
	// Provider control routes are operator-only; reuse the same
	// auth machinery as /v1/report. nil metrics sink: provider
	// rejections aren't reports, so they should not pollute
	// tunnelsmith_reports_rejected_total.
	return checkAuth(w, r, snap, nil)
}

func handleProvidersList(w http.ResponseWriter, r *http.Request, registry *ProviderRegistry) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bindings := registry.List()
	out := make([]providerListEntry, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, providerListEntry{
			IDPrefix: b.IDPrefix,
			Provider: b.Provider,
			HasAPI:   b.API != nil,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// providerRefreshTimeout caps how long a refresh call can run. Webshare's
// refresh is fast (server-side rotate trigger) but a hung HTTPS dial
// must not pin a goroutine indefinitely.
const providerRefreshTimeout = 30 * time.Second

func handleProviderRefresh(w http.ResponseWriter, r *http.Request, registry *ProviderRegistry, idPrefix string, logger *slog.Logger) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	binding, ok := registry.Lookup(idPrefix)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown id_prefix %q", idPrefix), http.StatusNotFound)
		return
	}
	if binding.API == nil {
		writeRefreshError(w, http.StatusNotImplemented, binding, "provider has no API surface")
		return
	}
	planID := r.URL.Query().Get("plan_id")
	ctx, cancel := context.WithTimeout(r.Context(), providerRefreshTimeout)
	defer cancel()
	if err := binding.API.RefreshProxyList(ctx, provider.RefreshOptions{PlanID: planID}); err != nil {
		// Map known provider error sentinels to the closest HTTP
		// status so an operator scripting against the route can
		// distinguish "back off and retry" (429) from a timeout
		// (504) from a generic vendor failure (502). The default
		// is 502 because a vendor that returned an error is still
		// effectively a bad gateway from the operator's view.
		status, publicMsg := classifyProviderError(err)
		// The full err chain stays in the operator log; the JSON
		// response body carries only the category string so vendor-
		// side detail (response bodies, internal hostnames, token-
		// derived URLs from any %w wrapping) never escapes the
		// operator's own infrastructure even though the route is
		// bearer-token gated.
		logger.Warn("provider refresh failed",
			"id_prefix", idPrefix,
			"provider", binding.Provider,
			"http_status", status,
			"err", err,
		)
		writeRefreshError(w, status, binding, publicMsg)
		return
	}
	logger.Info("provider refresh triggered",
		"id_prefix", idPrefix,
		"provider", binding.Provider,
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(refreshResponse{
		IDPrefix: idPrefix,
		Provider: binding.Provider,
		Status:   "accepted",
	})
}

// classifyProviderError maps a provider.API.RefreshProxyList error to
// (HTTP status, sanitised public message). The public message is short
// and category-level so vendor-side detail never leaves the binary
// through the HTTP body; full err chains live in the operator log
// instead. The match order matters: more-specific sentinels first,
// generic fall-through at the end.
func classifyProviderError(err error) (int, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "upstream timeout"
	case errors.Is(err, provider.ErrAPIRateLimited):
		return http.StatusTooManyRequests, "vendor rate limited"
	default:
		return http.StatusBadGateway, "refresh failed"
	}
}

func writeRefreshError(w http.ResponseWriter, status int, binding ProviderAPIBinding, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(refreshErrorResponse{
		IDPrefix: binding.IDPrefix,
		Provider: binding.Provider,
		Error:    msg,
	})
}
