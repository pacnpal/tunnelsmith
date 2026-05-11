package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"
)

// fakeAPI is a provider.API used to assert dispatch through the
// /v1/providers/{id_prefix}/refresh handler without needing a real
// vendor.
type fakeAPI struct {
	calls atomic.Int64
	err   error
	mu    sync.Mutex
	last  provider.RefreshOptions
}

func (f *fakeAPI) RefreshProxyList(_ context.Context, opts provider.RefreshOptions) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.last = opts
	f.mu.Unlock()
	return f.err
}

func newProviderTestServer(t *testing.T, bindings []ProviderAPIBinding, tokens []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mountProvidersHandlers(mux, NewProviderRegistry(bindings), NewTokenSet(tokens), nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProvidersListHappyPath(t *testing.T) {
	bindings := []ProviderAPIBinding{
		{IDPrefix: "ws", Provider: "webshare", API: &fakeAPI{}},
		{IDPrefix: "mvd", Provider: "mullvad", API: nil},
	}
	srv := newProviderTestServer(t, bindings, nil)
	resp, err := http.Get(srv.URL + "/v1/providers")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []providerListEntry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	// Sorted: "mvd" before "ws".
	if got[0].IDPrefix != "mvd" || got[1].IDPrefix != "ws" {
		t.Fatalf("ordering: %+v", got)
	}
	if got[0].HasAPI != false || got[1].HasAPI != true {
		t.Fatalf("has_api flags: %+v", got)
	}
}

func TestProviderRefreshCallsAPI(t *testing.T) {
	api := &fakeAPI{}
	srv := newProviderTestServer(t, []ProviderAPIBinding{
		{IDPrefix: "ws", Provider: "webshare", API: api},
	}, nil)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/providers/ws/refresh?plan_id=plan-42", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202; body=%s", resp.StatusCode, body)
	}
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("api calls = %d, want 1", got)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.last.PlanID != "plan-42" {
		t.Fatalf("plan_id = %q, want plan-42", api.last.PlanID)
	}
}

func TestProviderRefreshReturnsNotImplementedForNilAPI(t *testing.T) {
	srv := newProviderTestServer(t, []ProviderAPIBinding{
		{IDPrefix: "mvd", Provider: "mullvad", API: nil},
	}, nil)
	resp, err := http.Post(srv.URL+"/v1/providers/mvd/refresh", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

func TestProviderRefreshReturnsNotFoundForUnknownPrefix(t *testing.T) {
	srv := newProviderTestServer(t, []ProviderAPIBinding{
		{IDPrefix: "ws", Provider: "webshare", API: &fakeAPI{}},
	}, nil)
	resp, err := http.Post(srv.URL+"/v1/providers/missing/refresh", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestProviderRefreshSurfacesUpstreamErrorAsBadGateway(t *testing.T) {
	api := &fakeAPI{err: errors.New("token rejected")}
	srv := newProviderTestServer(t, []ProviderAPIBinding{
		{IDPrefix: "ws", Provider: "webshare", API: api},
	}, nil)
	resp, err := http.Post(srv.URL+"/v1/providers/ws/refresh", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProviderRefreshAuthGated(t *testing.T) {
	api := &fakeAPI{}
	srv := newProviderTestServer(t, []ProviderAPIBinding{
		{IDPrefix: "ws", Provider: "webshare", API: api},
	}, []string{"secret"})

	// Without the bearer token: 401.
	resp, err := http.Post(srv.URL+"/v1/providers/ws/refresh", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth POST: status = %d, want 401", resp.StatusCode)
	}
	if api.calls.Load() != 0 {
		t.Fatalf("api should not be called on auth failure: %d", api.calls.Load())
	}

	// With the bearer token: 202.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/providers/ws/refresh", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("auth POST: status = %d, want 202", resp.StatusCode)
	}
	if api.calls.Load() != 1 {
		t.Fatalf("api calls = %d, want 1", api.calls.Load())
	}
}

func TestProviderRefreshRejectsGET(t *testing.T) {
	srv := newProviderTestServer(t, []ProviderAPIBinding{
		{IDPrefix: "ws", Provider: "webshare", API: &fakeAPI{}},
	}, nil)
	resp, err := http.Get(srv.URL + "/v1/providers/ws/refresh")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestProvidersListRejectsPOST(t *testing.T) {
	srv := newProviderTestServer(t, []ProviderAPIBinding{}, nil)
	resp, err := http.Post(srv.URL+"/v1/providers", "", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestProviderRefreshMalformedPath(t *testing.T) {
	srv := newProviderTestServer(t, []ProviderAPIBinding{}, nil)
	cases := []string{
		"/v1/providers/",
		"/v1/providers/x",
		"/v1/providers/x/",
		"/v1/providers/x/y/z",
	}
	for _, p := range cases {
		resp, err := http.Post(srv.URL+p, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", p, resp.StatusCode)
		}
	}
}

// silence unused import warning in some configurations
var _ = time.Second
