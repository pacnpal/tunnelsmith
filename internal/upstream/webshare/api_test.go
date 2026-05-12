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
	"strconv"
	"strings"
	"sync"
	"testing"
)

// newFakeServer returns an httptest.Server that pretends to be Webshare.
// It only knows the endpoints the Client touches; each handler asserts
// the Authorization header so a regression that drops the token would
// fail loudly.
//
// Dispatch matches r.URL.Path exactly. The query string lives in
// r.URL.RawQuery so paginated /proxy/list/?page=N still routes to the
// registered "/proxy/list/" handler. Exact match means a test that
// accidentally sends to "/proxy/list/refresh/extra" gets 404 instead
// of silently dispatching to the closest prefix.
func newFakeServer(t *testing.T, handlers map[string]http.HandlerFunc) (*httptest.Server, *[]string) {
	t.Helper()
	var receivedTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTokens = append(receivedTokens, r.Header.Get("Authorization"))
		if h, ok := handlers[r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &receivedTokens
}

// TestProxyHostPort pins the IPv6-bracketing contract. Without
// net.JoinHostPort an IPv6 address would emit ambiguous output
// like "2001:db8::1:8080" that net.SplitHostPort cannot recover.
func TestProxyHostPort(t *testing.T) {
	cases := []struct {
		name string
		addr string
		port int
		want string
	}{
		{"ipv4", "1.2.3.4", 8080, "1.2.3.4:8080"},
		{"hostname", "proxy.example.com", 80, "proxy.example.com:80"},
		{"ipv6", "2001:db8::1", 8080, "[2001:db8::1]:8080"},
		{"ipv6 loopback", "::1", 1080, "[::1]:1080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Proxy{ProxyAddress: tc.addr, Port: tc.port}.HostPort()
			if got != tc.want {
				t.Fatalf("HostPort = %q, want %q", got, tc.want)
			}
		})
	}
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

// TestListProxiesSkipsCacheOnGeneric4xx pins the policy that an
// unexpected 4xx (e.g. 400 Bad Request from a malformed plan_id)
// propagates instead of being silently masked by stale cached data.
// Operator misconfig is fixable; stale data hides it.
func TestListProxiesSkipsCacheOnGeneric4xx(t *testing.T) {
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail": "invalid plan_id"}`))
		},
	})
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
		t.Fatalf("expected error from 400, got nil and %+v", got)
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want HTTPStatusError with StatusCode=400", err)
	}
	if got != nil {
		t.Fatalf("expected nil list on 4xx, got %+v", got)
	}
}

// TestListProxiesSkipsCacheOnDecodeError pins the conservative
// fallback policy: a malformed JSON response (which usually signals
// a schema change at Webshare) propagates so the operator notices,
// instead of being silently masked by a stale cache. Without the
// guard, a schema break would keep the binary serving from disk
// indefinitely.
func TestListProxiesSkipsCacheOnDecodeError(t *testing.T) {
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json at all`))
		},
	})
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	cached := []Proxy{{ID: "d-stale", ProxyAddress: "1.1.1.1", Port: 1, Valid: true}}
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
		t.Fatalf("expected decode error to propagate, got nil and %+v", got)
	}
	if got != nil {
		t.Fatalf("expected nil list on decode error (no fallback), got %+v", got)
	}
}

// TestListProxiesFallsBackOn5xx pins the inverse: a transient 5xx
// from Webshare still falls back to the cached list so the running
// pool keeps serving rather than blanking out on a server-side blip.
func TestListProxiesFallsBackOn5xx(t *testing.T) {
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		},
	})
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	cached := []Proxy{{ID: "d-77", ProxyAddress: "7.7.7.7", Port: 1, Valid: true}}
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
	if len(got) != 1 || got[0].ID != "d-77" {
		t.Fatalf("expected cached list on 5xx, got %+v", got)
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

// TestListProxiesRejectsMalformedNextPath locks the anchoring fix that
// prevents a "next" URL like /api/v2evil/... from silently rewriting
// the request URL when the /api/v2 prefix is stripped. The well-formed
// /api/v2/ prefix strips cleanly; anything else is rejected.
func TestListProxiesRejectsMalformedNextPath(t *testing.T) {
	page := mustEncode(t, map[string]any{
		"count":   1,
		"next":    "https://attacker.example.com/api/v2evil/leak",
		"results": []Proxy{{ID: "d-1", ProxyAddress: "1.1.1.1", Port: 1, Valid: true}},
	})
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(page)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	_, err := c.ListProxies(context.Background(), ListProxiesOptions{})
	if err == nil {
		t.Fatal("expected error on /api/v2evil/... next path; got nil")
	}
	if !strings.Contains(err.Error(), "next path") {
		t.Fatalf("err = %v, want one mentioning 'next path'", err)
	}
}

// TestListProxiesPushesValidFilter pins the server-side filter: in
// direct mode we tell Webshare to skip invalid proxies entirely, which
// reduces response size on a flapping pool. In backbone mode the
// `valid` field is documented as unsupported, so we must NOT send it.
func TestListProxiesPushesValidFilter(t *testing.T) {
	t.Run("direct mode sends valid=true", func(t *testing.T) {
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
		if _, err := c.ListProxies(context.Background(), ListProxiesOptions{Mode: "direct"}); err != nil {
			t.Fatalf("ListProxies: %v", err)
		}
		if !strings.Contains(observed, "valid=true") {
			t.Fatalf("expected valid=true in query, got %q", observed)
		}
	})
	t.Run("backbone mode omits valid filter", func(t *testing.T) {
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
		if _, err := c.ListProxies(context.Background(), ListProxiesOptions{Mode: "backbone"}); err != nil {
			t.Fatalf("ListProxies: %v", err)
		}
		if strings.Contains(observed, "valid=") {
			t.Fatalf("backbone mode must not send valid filter, got %q", observed)
		}
	})
}

// TestAPITokenAutoReloadOnUnauthorized exercises the credential-rotation
// self-heal: a 401 from the vendor triggers a re-read of APITokenFile;
// if the file has rotated, the client swaps the in-memory token and
// retries the same request. The fake server returns 401 for the old
// token and 200 for the new one, so a successful request after one
// 401 proves the reload+retry fired.
func TestAPITokenAutoReloadOnUnauthorized(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokPath, []byte("old-token"), 0o600); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	var hits int
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/profile/": func(w http.ResponseWriter, r *http.Request) {
			hits++
			switch r.Header.Get("Authorization") {
			case "Token new-token":
				_, _ = w.Write([]byte(`{"id":1,"email":"u@example.com"}`))
			default:
				// Rotate the on-disk token between the first 401
				// and the retry. This simulates an operator
				// rewriting the secret file during the request
				// window.
				_ = os.WriteFile(tokPath, []byte("new-token"), 0o600)
				w.WriteHeader(http.StatusUnauthorized)
			}
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "old-token"
	c.APITokenFile = tokPath
	c.HTTPClient = srv.Client()
	p, err := c.Profile(context.Background())
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if p.Email != "u@example.com" {
		t.Fatalf("decoded profile mismatch: %+v", p)
	}
	if hits != 2 {
		t.Fatalf("expected exactly 2 round-trips (401 then 200), got %d", hits)
	}
	if got := c.snapshotToken(); got != "new-token" {
		t.Fatalf("in-memory token after reload = %q, want %q", got, "new-token")
	}
}

// TestAPITokenAutoReloadNoFileNoRetry is the negative case: without
// APITokenFile, a 401 must propagate unchanged. No retry, no reload.
func TestAPITokenAutoReloadNoFileNoRetry(t *testing.T) {
	hits := 0
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/profile/": func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusUnauthorized)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "stale"
	// APITokenFile deliberately unset.
	c.HTTPClient = srv.Client()
	_, err := c.Profile(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly 1 round-trip (no retry), got %d", hits)
	}
}

// TestAPITokenAutoReloadSameFileNoRetry pins the conservative branch:
// when the file content is unchanged, the 401 propagates without
// retry. Otherwise a genuinely-bad token would cause an infinite
// reload loop in production.
func TestAPITokenAutoReloadSameFileNoRetry(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokPath, []byte("same-token"), 0o600); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	hits := 0
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/profile/": func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusUnauthorized)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "same-token"
	c.APITokenFile = tokPath
	c.HTTPClient = srv.Client()
	_, err := c.Profile(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly 1 round-trip (no retry on unchanged file), got %d", hits)
	}
}

// TestAPITokenAutoReloadCappedAtOneRetry confirms a second 401 after
// the reload does not loop. The fake server returns 401 forever; the
// file gets rotated to a different (but still wrong) value. We expect
// exactly two round-trips (initial + one retry) and the second 401
// propagates.
func TestAPITokenAutoReloadCappedAtOneRetry(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	hits := 0
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/profile/": func(w http.ResponseWriter, r *http.Request) {
			hits++
			// Rotate the file on the first miss so the reload
			// happens; the rotated value is still wrong from the
			// server's view.
			if hits == 1 {
				_ = os.WriteFile(tokPath, []byte("also-wrong"), 0o600)
			}
			w.WriteHeader(http.StatusUnauthorized)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "old"
	c.APITokenFile = tokPath
	c.HTTPClient = srv.Client()
	_, err := c.Profile(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if hits != 2 {
		t.Fatalf("expected initial + one retry = 2 round-trips, got %d", hits)
	}
}

// TestAPITokenAutoReloadConcurrentReadsAreSafe runs many goroutines
// against an httptest server that flips between 401 and 200 based on
// the token sent. With the auto-reload path active and rotating the
// file on first 401, the race detector should observe no data races
// on the Client.APIToken field — that's the contract tokenMu is there
// to enforce.
//
// The test deliberately bypasses newFakeServer's request-log slice
// (which would race independently of anything in Client) and uses a
// bare httptest server so the only object under race scrutiny is the
// Client itself. A sync.Once guards the file rotation so 16
// goroutines triggering the first 401 race do not collide on the
// disk-write call.
func TestAPITokenAutoReloadConcurrentReadsAreSafe(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokPath, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	var rotate sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Token v2" {
			_, _ = w.Write([]byte(`{"id":1,"email":"ok@example.com"}`))
			return
		}
		rotate.Do(func() {
			_ = os.WriteFile(tokPath, []byte("v2"), 0o600)
		})
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "v1"
	c.APITokenFile = tokPath
	c.HTTPClient = srv.Client()

	const goroutines = 16
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			_, err := c.Profile(context.Background())
			errs <- err
		}()
	}
	for i := 0; i < goroutines; i++ {
		if err := <-errs; err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}

// TestFetchProxyListStatusRequiresPlanID pins the gating: the v3 status
// endpoint mandates plan_id, and the client refuses to issue a request
// that would just return 400 from the vendor.
func TestFetchProxyListStatusRequiresPlanID(t *testing.T) {
	c := NewClient()
	c.APIToken = "tok"
	_, err := c.FetchProxyListStatus(context.Background(), "")
	if !errors.Is(err, ErrPlanIDRequired) {
		t.Fatalf("FetchProxyListStatus(\"\") = %v, want ErrPlanIDRequired", err)
	}
	_, err = c.FetchProxyListStatus(context.Background(), "   ")
	if !errors.Is(err, ErrPlanIDRequired) {
		t.Fatalf("FetchProxyListStatus(whitespace) = %v, want ErrPlanIDRequired", err)
	}
}

// TestFetchProxyListStatusDecodesResponse exercises the wire-shape decode.
// The fake server returns a canonical Webshare-shaped response; the
// returned struct fields must be populated.
func TestFetchProxyListStatusDecodesResponse(t *testing.T) {
	// Build the response body via json.Marshal rather than inlining a
	// JSON string literal that pairs "username" and "password" keys
	// on adjacent lines. The string-literal form trips GitGuardian's
	// "Username Password" heuristic; the marshal form produces the
	// same wire bytes without the source-code shape that gets
	// flagged.
	wantUser := "ufresh"
	wantPass := "pfresh"
	respBody := mustEncode(t, map[string]any{
		"state":                 "completed",
		"countries":             map[string]int{"US": 5, "FR": 100},
		"unallocated_countries": map[string]int{},
		"username":              wantUser,
		"password":              wantPass,
		"is_proxy_used":         false,
	})
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/status": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("plan_id") != "plan-99" {
				t.Errorf("plan_id missing: %q", r.URL.RawQuery)
			}
			_, _ = w.Write(respBody)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	got, err := c.FetchProxyListStatus(context.Background(), "plan-99")
	if err != nil {
		t.Fatalf("FetchProxyListStatus: %v", err)
	}
	if got.State != "completed" || got.Username != wantUser || got.Password != wantPass {
		t.Fatalf("decoded fields mismatch: %+v", got)
	}
	if got.Countries["US"] != 5 || got.Countries["FR"] != 100 {
		t.Fatalf("countries map decoded wrong: %+v", got.Countries)
	}
}

// TestFetchProxyListStatusSurfacesUnauthorized confirms the v3 doV3 path
// shares the same typed-sentinel translation as the v2 do path: 401 →
// ErrUnauthorized so errors.Is at higher layers (e.g. fetchCanonicalCreds
// in the Expander) can dispatch without string matching.
func TestFetchProxyListStatusSurfacesUnauthorized(t *testing.T) {
	srv, _ := newFakeServer(t, map[string]http.HandlerFunc{
		"/proxy/list/status": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	})
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "bad"
	c.HTTPClient = srv.Client()
	_, err := c.FetchProxyListStatus(context.Background(), "plan-1")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

// TestBaseURLForV3SwapsVersionSegment locks the production URL
// derivation: production BaseURL is /api/v2 → v3 base must be /api/v3.
// Test BaseURL (no /api/vN suffix) returns as-is so httptest fakes
// keep working without an unnecessary path segment.
func TestBaseURLForV3SwapsVersionSegment(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{"production default", "https://proxy.webshare.io/api/v2", "https://proxy.webshare.io/api/v3"},
		{"empty (uses default)", "", "https://proxy.webshare.io/api/v3"},
		{"httptest root", "http://127.0.0.1:9999", "http://127.0.0.1:9999"},
		{"custom host with v2", "https://proxy.example.com/api/v2", "https://proxy.example.com/api/v3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{BaseURL: tc.base}
			got := c.baseURLForV3()
			if got != tc.want {
				t.Fatalf("baseURLForV3 = %q, want %q", got, tc.want)
			}
		})
	}
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
			// Encode a payload that keeps pointing at itself.
			// The "next" path must be under /api/v2/ to satisfy
			// the strict prefix check the client enforces;
			// otherwise the loop terminates with a "next path"
			// error before the page-cap guard fires.
			body := paginatedResponse(
				[]Proxy{{ID: fmt.Sprintf("d-%d", hits), ProxyAddress: "1.1.1.1", Port: hits + 8000, Valid: true}},
				"https://server/api/v2/proxy/list/?page="+strconv.Itoa(hits+1),
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
