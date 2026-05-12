package webshare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

func newListServer(t *testing.T, proxies []Proxy) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":    len(proxies),
			"next":     nil,
			"previous": nil,
			"results":  proxies,
		})
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	return c, srv
}

func TestExpanderSnapshotTransforms(t *testing.T) {
	c, _ := newListServer(t, []Proxy{
		{ID: "d-10", Username: "u", Password: "p", ProxyAddress: "1.1.1.1", Port: 8001, Valid: true, CountryCode: "US"},
		{ID: "d-20", Username: "u", Password: "p", ProxyAddress: "2.2.2.2", Port: 8002, Valid: false, CountryCode: "DE"},
		{ID: "d-30", Username: "u", Password: "p", ProxyAddress: "3.3.3.3", Port: 8003, Valid: true, CountryCode: "GB"},
	})
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 200}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Invalid proxy d-20 must be skipped.
	if len(got) != 2 {
		t.Fatalf("expected 2 valid upstreams, got %d: %+v", len(got), got)
	}
	if got[0].ID != "ws-d-10" || got[1].ID != "ws-d-30" {
		t.Fatalf("id transform: %+v", got)
	}
	if got[0].Addr != "1.1.1.1:8001" || got[1].Addr != "3.3.3.3:8003" {
		t.Fatalf("addr transform: %+v", got)
	}
	if got[0].Username != "u" || got[0].Password != "p" {
		t.Fatalf("auth lost: %+v", got[0])
	}
	if got[0].Kind != config.KindHTTP {
		t.Fatalf("default kind not http: %v", got[0].Kind)
	}
	if got[0].Priority == nil || *got[0].Priority != 200 {
		t.Fatalf("priority not threaded: %+v", got[0])
	}
}

func TestExpanderHonorsKindSOCKS5(t *testing.T) {
	c, _ := newListServer(t, []Proxy{
		{ID: "d-1", ProxyAddress: "1.1.1.1", Port: 1080, Valid: true},
	})
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 200, Kind: "socks5"}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 1 || got[0].Kind != config.KindSOCKS5 {
		t.Fatalf("expected socks5, got %+v", got)
	}
}

func TestExpanderRejectsBadMode(t *testing.T) {
	c := NewClient()
	c.APIToken = "x"
	if _, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Mode: "wireguard"}, c, nil); err == nil {
		t.Fatal("expected error for bad mode")
	}
}

func TestExpanderRejectsBadKind(t *testing.T) {
	c := NewClient()
	c.APIToken = "x"
	if _, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Kind: "wireguard"}, c, nil); err == nil {
		t.Fatal("expected error for bad kind")
	}
}

func TestExpanderRejectsEmptyIDPrefix(t *testing.T) {
	c := NewClient()
	c.APIToken = "x"
	if _, err := NewExpander(ExpanderConfig{}, c, nil); err == nil {
		t.Fatal("expected error for empty id_prefix")
	}
}

// TestExpanderProxyCredsOverrideWins exercises the proxy_username /
// proxy_password override path. When the operator pins credentials on
// the [[upstream_pool]] block, every expanded upstream must carry
// those literal values instead of whatever the vendor's proxy-list
// returned. This is the escape hatch for "Webshare's list shows stale
// creds and every CONNECT comes back 407".
func TestExpanderProxyCredsOverrideWins(t *testing.T) {
	c, _ := newListServer(t, []Proxy{
		{ID: "d-1", Username: "old", Password: "old", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
		{ID: "d-2", Username: "old", Password: "old", ProxyAddress: "2.2.2.2", Port: 2, Valid: true},
	})
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:      "ws",
		Priority:      200,
		ProxyUsername: "override-user",
		ProxyPassword: "override-pass",
	}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d upstreams, want 2", len(got))
	}
	for _, u := range got {
		if u.Username != "override-user" || u.Password != "override-pass" {
			t.Errorf("upstream %s creds = %q/%q, want override values", u.ID, u.Username, u.Password)
		}
	}
}

// TestExpanderProxyCredsEmptyKeepsVendorValues confirms the override is
// truly opt-in: with both override fields blank, the expander threads
// the per-proxy username/password from the API response through
// unchanged.
func TestExpanderProxyCredsEmptyKeepsVendorValues(t *testing.T) {
	c, _ := newListServer(t, []Proxy{
		{ID: "d-1", Username: "vendor-u", Password: "vendor-p", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
	})
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 200}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 1 || got[0].Username != "vendor-u" || got[0].Password != "vendor-p" {
		t.Fatalf("expected vendor creds preserved, got %+v", got)
	}
}

// TestExpanderUsesCanonicalCredsFromStatus pins the credential-self-heal
// path: when plan_id is configured, the v3 /proxy/list/status response
// is the source of truth for username/password, and any drift between
// the status creds and the per-proxy creds returned by /proxy/list/
// must resolve in favour of the status creds.
//
// The scenario simulates the production failure mode the comment block
// in Snapshot describes: operator rotates the dashboard password,
// /proxy/list/ still serves the old per-proxy creds for a window, and
// without the heal every CONNECT lands as 407.
func TestExpanderUsesCanonicalCredsFromStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/list/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 2,
				"results": []Proxy{
					{ID: "d-1", Username: "stale", Password: "stale", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
					{ID: "d-2", Username: "stale", Password: "stale", ProxyAddress: "2.2.2.2", Port: 2, Valid: true},
				},
			})
		case "/proxy/list/status":
			if r.URL.Query().Get("plan_id") != "plan-X" {
				t.Errorf("status probe missing plan_id, got %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state":    "completed",
				"username": "fresh",
				"password": "fresh-pass",
			})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1, PlanID: "plan-X"}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 upstreams, got %d: %+v", len(got), got)
	}
	for _, u := range got {
		if u.Username != "fresh" || u.Password != "fresh-pass" {
			t.Errorf("upstream %s = %q/%q, want canonical creds fresh/fresh-pass", u.ID, u.Username, u.Password)
		}
	}
}

// TestExpanderFallsBackToListCredsWhenStatusFails confirms the status
// probe is best-effort: a 500 from /proxy/list/status must not break
// Snapshot. The per-proxy creds from /proxy/list/ stand in.
func TestExpanderFallsBackToListCredsWhenStatusFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/list/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 1,
				"results": []Proxy{
					{ID: "d-1", Username: "ulist", Password: "plist", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
				},
			})
		case "/proxy/list/status":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1, PlanID: "plan-X"}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 1 || got[0].Username != "ulist" || got[0].Password != "plist" {
		t.Fatalf("expected list-derived creds when status fails, got %+v", got)
	}
}

// TestExpanderOperatorOverrideBeatsCanonical confirms the precedence
// chain: even when status returns canonical creds, an explicit operator
// override on the [[upstream_pool]] block still wins. This protects the
// "I know what I'm doing, use these exact creds" escape hatch.
func TestExpanderOperatorOverrideBeatsCanonical(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/list/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 1,
				"results": []Proxy{
					{ID: "d-1", Username: "ulist", Password: "plist", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
				},
			})
		case "/proxy/list/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "completed", "username": "canon", "password": "canonp",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:      "ws",
		Priority:      1,
		PlanID:        "plan-X",
		ProxyUsername: "override-u",
		ProxyPassword: "override-p",
	}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 1 || got[0].Username != "override-u" || got[0].Password != "override-p" {
		t.Fatalf("operator override should win over canonical creds; got %+v", got)
	}
}

// TestExpanderTrimsPlanIDAtConstruction pins the construction-time
// trim: whitespace-only PlanID falls back to "no plan_id" semantics
// (no status probe), and a padded value reaches downstream API calls
// without surrounding whitespace. Both branches of the original
// padded-PlanID bug — silently slipping the gate AND sending dirty
// values to the vendor — are covered.
func TestExpanderTrimsPlanIDAtConstruction(t *testing.T) {
	t.Run("whitespace-only PlanID acts like empty", func(t *testing.T) {
		var statusHits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/proxy/list/":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"count": 1,
					"results": []Proxy{
						{ID: "d-1", Username: "u", Password: "p", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
					},
				})
			case "/proxy/list/status":
				statusHits++
			}
		}))
		t.Cleanup(srv.Close)
		c := NewClient()
		c.BaseURL = srv.URL
		c.APIToken = "tok"
		c.HTTPClient = srv.Client()
		exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1, PlanID: "   "}, c, nil)
		if err != nil {
			t.Fatalf("NewExpander: %v", err)
		}
		if _, err := exp.Snapshot(context.Background()); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if statusHits != 0 {
			t.Fatalf("whitespace PlanID still triggered %d status probes; want 0", statusHits)
		}
	})
	t.Run("padded PlanID reaches API trimmed", func(t *testing.T) {
		var observed string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/proxy/list/":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"count": 1,
					"results": []Proxy{
						{ID: "d-1", Username: "u", Password: "p", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
					},
				})
			case "/proxy/list/status":
				observed = r.URL.Query().Get("plan_id")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"state":    "completed",
					"username": "u",
					"password": "p",
				})
			}
		}))
		t.Cleanup(srv.Close)
		c := NewClient()
		c.BaseURL = srv.URL
		c.APIToken = "tok"
		c.HTTPClient = srv.Client()
		exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1, PlanID: "  plan-42\t"}, c, nil)
		if err != nil {
			t.Fatalf("NewExpander: %v", err)
		}
		if _, err := exp.Snapshot(context.Background()); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if observed != "plan-42" {
			t.Fatalf("status probe received plan_id %q, want %q (trimmed)", observed, "plan-42")
		}
	})
}

// TestExpanderSkipsStatusProbeWithoutPlanID asserts the probe is gated
// on plan_id (Webshare's API requires it). With no PlanID, /proxy/list/status
// must NEVER be called.
func TestExpanderSkipsStatusProbeWithoutPlanID(t *testing.T) {
	var statusHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/list/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 1,
				"results": []Proxy{
					{ID: "d-1", Username: "u", Password: "p", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
				},
			})
		case "/proxy/list/status":
			statusHits++
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	if _, err := exp.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if statusHits != 0 {
		t.Fatalf("status endpoint hit %d times without plan_id; want 0", statusHits)
	}
}

// TestExpanderIgnoresEmptyCanonicalCreds defends against the defensive
// branch in fetchCanonicalCreds: if /proxy/list/status returns 200 with
// empty username/password, the expander must NOT blank out every
// upstream's Proxy-Authorization header. List-derived creds win in
// that pathological case.
func TestExpanderIgnoresEmptyCanonicalCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/list/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 1,
				"results": []Proxy{
					{ID: "d-1", Username: "ulist", Password: "plist", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
				},
			})
		case "/proxy/list/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "completed", "username": "", "password": "",
			})
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1, PlanID: "plan-X"}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 1 || got[0].Username != "ulist" || got[0].Password != "plist" {
		t.Fatalf("empty status creds must be ignored; got %+v", got)
	}
}

// TestHealCallsRefreshAndReSnapshots pins the contract that Heal does
// what its name says: issue a /proxy/list/refresh/ POST first, then a
// fresh Snapshot. Both calls must observe even when the proxy is
// being driven from outside.
func TestHealCallsRefreshAndReSnapshots(t *testing.T) {
	var refreshHits, listHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/list/refresh/":
			refreshHits++
			w.WriteHeader(http.StatusNoContent)
		case "/proxy/list/":
			listHits++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 1,
				"results": []Proxy{
					{ID: "d-1", Username: "u", Password: "p", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
				},
			})
		case "/proxy/list/status":
			// Plan-less heal: status probe not expected. Make any hit fail loudly.
			t.Errorf("unexpected status probe in plan-less heal")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Heal(context.Background())
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if refreshHits != 1 {
		t.Fatalf("refresh hits = %d, want 1", refreshHits)
	}
	if listHits != 1 {
		t.Fatalf("list hits = %d, want 1", listHits)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 upstream, got %d", len(got))
	}
}

// TestHealContinuesPastRefreshFailure pins the partial-success contract
// in Heal: when the refresh endpoint errors (e.g. quota exhausted), the
// expander still re-snapshots, which is the half of the heal that picks
// up canonical creds from the status endpoint. Failing closed here would
// turn a partial-but-useful heal into nothing.
func TestHealContinuesPastRefreshFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/list/refresh/":
			w.WriteHeader(http.StatusForbidden) // ErrForbidden, e.g. no quota
		case "/proxy/list/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 1,
				"results": []Proxy{
					{ID: "d-1", Username: "u", Password: "p", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Heal(context.Background())
	if err != nil {
		t.Fatalf("Heal must succeed past refresh failure: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 upstream after partial heal, got %d", len(got))
	}
}

// TestBackoffIntervalShape pins the backoff math: base on the first
// failure, doubling each subsequent failure, capped at maxInterval.
// Locked here because the runtime test (waiting through three ticks)
// would add seconds to the suite without telling us anything more.
func TestBackoffIntervalShape(t *testing.T) {
	base := 1 * time.Second
	cap := 8 * time.Second
	cases := []struct {
		consecutive int
		want        time.Duration
	}{
		{0, base}, // pre-failure
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},  // hits cap
		{5, 8 * time.Second},  // stays at cap
		{50, 8 * time.Second}, // very high failures still capped
	}
	for _, tc := range cases {
		got := backoffInterval(tc.consecutive, base, cap)
		if got != tc.want {
			t.Errorf("backoffInterval(%d, %v, %v) = %v, want %v", tc.consecutive, base, cap, got, tc.want)
		}
	}
}

// TestBackoffCapPicksSmaller pins the dual-cap rule: 8× base for short
// intervals, hardCap (30m) for long ones. Both regimes matter so the
// test covers each.
func TestBackoffCapPicksSmaller(t *testing.T) {
	cases := []struct {
		base time.Duration
		want time.Duration
	}{
		{1 * time.Second, 8 * time.Second},            // 8× < 30m
		{1 * time.Minute, 8 * time.Minute},            // 8× < 30m
		{10 * time.Minute, 30 * time.Minute},          // 8× = 80m > 30m, clamp
		{1 * time.Hour, 30 * time.Minute},             // 8× = 8h > 30m, clamp
		{24 * time.Hour, 30 * time.Minute},            // 8× = 192h > 30m, clamp
		{time.Duration(1<<60), 30 * time.Minute},      // 8× would overflow int64, clamp
	}
	for _, tc := range cases {
		got := backoffCap(tc.base)
		if got != tc.want {
			t.Errorf("backoffCap(%v) = %v, want %v", tc.base, got, tc.want)
		}
	}
}

// TestRunRefreshSpacesOutOnPersistentFailure exercises RunRefresh end-to-end
// with a server that always returns 5xx. The test only asserts that the
// number of hits over the test window is roughly consistent with the
// backoff curve (i.e. not 1 hit per base tick), not that the timing is
// precise — Go scheduling makes microsecond assertions flaky. The
// load-shedding behaviour is what matters; exact tick spacing is in the
// unit tests above.
func TestRunRefreshSpacesOutOnPersistentFailure(t *testing.T) {
	t.Parallel()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	// Base of 20ms makes the test snappy. Cap is min(8×, 30m) = 160ms.
	// Over 600ms we'd expect 1 (t=20), 2 (t=60), 3 (t=140), 4 (t=300),
	// 5 (t=460)... roughly 5 hits, NOT 30 (which would be 600/20).
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1, Refresh: 20 * time.Millisecond}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = exp.RunRefresh(ctx, nil, func(_, _ []config.UpstreamConfig) {})

	got := atomic.LoadInt64(&hits)
	if got == 0 {
		t.Fatalf("RunRefresh never fired the snapshot")
	}
	if got > 15 {
		// A no-backoff loop would produce ~30 hits in this window;
		// 15 is the conservative ceiling that lets the test be robust
		// to scheduler jitter while still failing on a regression.
		t.Fatalf("RunRefresh hit the server %d times in 600ms; expected backoff to keep it well below 15", got)
	}
}

// TestExpanderSnapshotIsSortedByID locks the determinism contract the
// pool composer's diff relies on. Webshare returns proxies in
// account-creation order; the expander must re-sort so a stable input
// produces a stable slice (and the no-op shortcut in poolComposer.Update
// fires when nothing changed).
func TestExpanderSnapshotIsSortedByID(t *testing.T) {
	c, _ := newListServer(t, []Proxy{
		{ID: "d-30", ProxyAddress: "3.3.3.3", Port: 1, Valid: true},
		{ID: "d-10", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
		{ID: "d-20", ProxyAddress: "2.2.2.2", Port: 1, Valid: true},
	})
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	want := []string{"ws-d-10", "ws-d-20", "ws-d-30"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}
