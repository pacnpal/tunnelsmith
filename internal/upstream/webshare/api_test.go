package webshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// newFakeServer returns an httptest.Server that pretends to be Webshare.
// It only knows the endpoints the Client touches; each handler asserts
// the Authorization header so a regression that drops the token would
// fail loudly.
//
// Dispatch is longest-prefix-wins so a test that registers both
// "/proxy/list/" and "/proxy/list/refresh/" gets deterministic
// behaviour (Go's map iteration is randomised; a plain for-range over
// the map would flake).
func newFakeServer(t *testing.T, handlers map[string]http.HandlerFunc) (*httptest.Server, *[]string) {
	t.Helper()
	prefixes := make([]string, 0, len(handlers))
	for p := range handlers {
		prefixes = append(prefixes, p)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		return len(prefixes[i]) > len(prefixes[j])
	})
	var receivedTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTokens = append(receivedTokens, r.Header.Get("Authorization"))
		for _, prefix := range prefixes {
			if strings.HasPrefix(r.URL.Path, prefix) {
				handlers[prefix](w, r)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &receivedTokens
}

func TestProfileSendsTokenHeader(t *testing.T) {
	srv, received := newFakeServer(t, map[string]http.HandlerFunc{
		"/profile/": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id": 1, "email": "u@example.com"}`))
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "abc123"
	c.HTTPClient = srv.Client()

	p, err := c.Profile(context.Background())
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if p.Email != "u@example.com" || p.ID != 1 {
		t.Fatalf("profile mismatch: %+v", p)
	}
	if len(*received) == 0 || (*received)[0] != "Token abc123" {
		t.Fatalf("Authorization header = %v, want %q", *received, "Token abc123")
	}
}

func TestProfileSurfacesUnauthorized(t *testing.T) {
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/profile/": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "bad"
	c.HTTPClient = srv.Client()

	_, err := c.Profile(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestListProxiesFollowsPagination(t *testing.T) {
	page1 := mustEncode(t, paginatedResponse(
		[]Proxy{
			{ID: "d-1", Username: "u", Password: "p", ProxyAddress: "1.1.1.1", Port: 8001, Valid: true, CountryCode: "US"},
			{ID: "d-2", Username: "u", Password: "p", ProxyAddress: "2.2.2.2", Port: 8002, Valid: true, CountryCode: "US"},
		},
		"http://server/api/v2/proxy/list/?page=2&page_size=2",
	))
	page2 := mustEncode(t, paginatedResponse(
		[]Proxy{
			{ID: "d-3", Username: "u", Password: "p", ProxyAddress: "3.3.3.3", Port: 8003, Valid: false, CountryCode: "US"},
		},
		"",
	))
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "", "1":
				_, _ = w.Write(page1)
			case "2":
				_, _ = w.Write(page2)
			default:
				t.Errorf("unexpected page query: %q", r.URL.Query().Get("page"))
				http.NotFound(w, r)
			}
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	got, err := c.ListProxies(context.Background(), ListProxiesOptions{PageSize: 2})
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 proxies, got %d: %+v", len(got), got)
	}
	if got[0].ID != "d-1" || got[2].ID != "d-3" {
		t.Fatalf("pagination order off: %+v", got)
	}
}

func TestListProxiesSendsModeAndCountryFilter(t *testing.T) {
	var observedQuery string
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, r *http.Request) {
			observedQuery = r.URL.RawQuery
			_, _ = w.Write(mustEncode(t, paginatedResponse([]Proxy{}, "")))
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	_, err := c.ListProxies(context.Background(), ListProxiesOptions{
		Mode:         "direct",
		CountryCodes: []string{"US", "GB"},
		PageSize:     50,
	})
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if !strings.Contains(observedQuery, "mode=direct") {
		t.Fatalf("mode missing from query: %q", observedQuery)
	}
	if !strings.Contains(observedQuery, "country_code__in=US%2CGB") {
		t.Fatalf("country filter missing from query: %q", observedQuery)
	}
	if !strings.Contains(observedQuery, "page_size=50") {
		t.Fatalf("page_size missing from query: %q", observedQuery)
	}
}

// TestListProxiesNormalizesCountryCodes asserts lowercase / mixed-case
// ISO codes are uppercased before being sent to the vendor. Webshare's
// docs imply case-sensitive matching in some plan flavours; the
// expander already validates ASCII letters, so the API layer just
// needs to canonicalise.
func TestListProxiesNormalizesCountryCodes(t *testing.T) {
	var observed string
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, r *http.Request) {
			observed = r.URL.RawQuery
			_, _ = w.Write(mustEncode(t, paginatedResponse([]Proxy{}, "")))
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	if _, err := c.ListProxies(context.Background(), ListProxiesOptions{
		CountryCodes: []string{"us", "Gb", "DE"},
	}); err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if !strings.Contains(observed, "country_code__in=US%2CGB%2CDE") {
		t.Fatalf("country codes not normalised: %q", observed)
	}
}

func TestRefreshProxyListAcceptsNoContent(t *testing.T) {
	called := 0
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/refresh/": func(w http.ResponseWriter, r *http.Request) {
			called++
			if r.Method != http.MethodPost {
				t.Errorf("method = %q, want POST", r.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	if err := c.RefreshProxyList(context.Background(), ""); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if called != 1 {
		t.Fatalf("Refresh called %d times, want 1", called)
	}
}

func TestRefreshProxyListThreadsPlanID(t *testing.T) {
	var observed string
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/refresh/": func(w http.ResponseWriter, r *http.Request) {
			observed = r.URL.RawQuery
			w.WriteHeader(http.StatusNoContent)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	if err := c.RefreshProxyList(context.Background(), "plan-42"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !strings.Contains(observed, "plan_id=plan-42") {
		t.Fatalf("plan_id missing: %q", observed)
	}
}

func TestRefreshProxyListSurfacesForbidden(t *testing.T) {
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/refresh/": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	err := c.RefreshProxyList(context.Background(), "")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestRefreshProxyListSurfacesRateLimited(t *testing.T) {
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/refresh/": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()

	if err := c.RefreshProxyList(context.Background(), ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

// TestListProxiesSkipsCacheOnAuthFailure pins the security stance that
// a 401 / 403 / 429 must propagate to the caller even when a stale
// cache could mask it. Otherwise a revoked token would silently keep
// serving from the disk snapshot.
func TestListProxiesSkipsCacheOnAuthFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"forbidden", http.StatusForbidden, ErrForbidden},
		{"rate limited", http.StatusTooManyRequests, ErrRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
				"/proxy/list/": func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
				},
			})
			// Seed a populated cache; a regression would silently
			// return cached entries here.
			cachePath := filepath.Join(t.TempDir(), "cache.json")
			cached := []Proxy{{ID: "d-99", ProxyAddress: "9.9.9.9", Port: 1, Valid: true}}
			if err := (&Cache{Path: cachePath}).Write(cached); err != nil {
				t.Fatalf("seed cache: %v", err)
			}
			c := NewClient()
			c.BaseURL = srv.URL
			c.APIToken = "tok"
			c.HTTPClient = srv.Client()
			c.Cache = &Cache{Path: cachePath}
			got, err := c.ListProxies(context.Background(), ListProxiesOptions{})
			if err == nil {
				t.Fatalf("expected error, got nil and %+v", got)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want wrapping %v", err, tc.want)
			}
			if got != nil {
				t.Fatalf("expected nil list on auth/rate-limit, got %+v", got)
			}
		})
	}
}

func TestListProxiesFallsBackToCache(t *testing.T) {
	// API returns 500 every time; the cache has a list.
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.json")
	cached := []Proxy{{ID: "d-1", ProxyAddress: "1.1.1.1", Port: 1, Valid: true, Username: "u", Password: "p"}}
	if err := (&Cache{Path: cachePath}).Write(cached); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	c.Cache = &Cache{Path: cachePath}

	got, err := c.ListProxies(context.Background(), ListProxiesOptions{})
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(got) != 1 || got[0].ID != "d-1" {
		t.Fatalf("cache fallback: got %+v", got)
	}
}

func TestListProxiesWritesCacheOnSuccess(t *testing.T) {
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(mustEncode(t, paginatedResponse(
				[]Proxy{{ID: "d-1", ProxyAddress: "1.1.1.1", Port: 1, Valid: true}},
				"",
			)))
		},
	})
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	c.Cache = &Cache{Path: cachePath}

	if _, err := c.ListProxies(context.Background(), ListProxiesOptions{}); err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
}

func TestLoadTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	if err := os.WriteFile(path, []byte("  hello\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("LoadTokenFile: %v", err)
	}
	if got != "hello" {
		t.Fatalf("LoadTokenFile = %q, want %q", got, "hello")
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if _, err := LoadTokenFile(empty); err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
}

func TestLoadTokenFileRejectsMultipleTokens(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"two lines":       "tok1\ntok2\n",
		"space separated": "tok1 tok2",
		"trailing junk":   "tok1\nfooter line\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := LoadTokenFile(path)
			if err == nil {
				t.Fatal("expected error for multi-field file, got nil")
			}
			if !strings.Contains(err.Error(), "exactly one token") {
				t.Fatalf("err = %v, want one mentioning 'exactly one token'", err)
			}
		})
	}
}

func TestLoadTokenFileRejectsRelativePath(t *testing.T) {
	if _, err := LoadTokenFile("relative.txt"); err == nil {
		t.Fatal("expected error for relative path, got nil")
	}
}

func TestDoRejectsEmptyToken(t *testing.T) {
	c := NewClient()
	_, err := c.Profile(context.Background())
	if err == nil {
		t.Fatal("expected error from empty token, got nil")
	}
}

// paginatedResponse builds the wire-shaped pagination envelope used by
// /proxy/list/.
func paginatedResponse(results []Proxy, next string) map[string]any {
	return map[string]any{
		"count":    len(results),
		"next":     next,
		"previous": nil,
		"results":  results,
	}
}

func mustEncode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

// TestListProxiesCapsPages confirms the maxResponsePages guard fires if
// a misbehaving server keeps handing back a "next" cursor forever. We
// patch maxResponsePages-style behaviour by serving an unbounded loop
// and ensure the client gives up.
func TestListProxiesCapsPages(t *testing.T) {
	hits := 0
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, r *http.Request) {
			hits++
			// Encode a payload that keeps pointing at itself; the
			// query string is irrelevant since the client follows
			// next verbatim.
			body := paginatedResponse(
				[]Proxy{{ID: fmt.Sprintf("d-%d", hits), ProxyAddress: "1.1.1.1", Port: hits + 8000, Valid: true}},
				r.URL.String()+"&"+strconv.Itoa(hits),
			)
			_, _ = w.Write(mustEncode(t, body))
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	_, err := c.ListProxies(context.Background(), ListProxiesOptions{})
	if err == nil {
		t.Fatal("expected error when pagination overflows, got nil")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("expected error to mention page limit, got %v", err)
	}
}
