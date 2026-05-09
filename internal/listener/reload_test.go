package listener_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
)

// TestHTTPReloadSwapsDetector confirms a Reload call swaps in a new
// failure detector mid-flight: a destination that returned 429 (treated
// as failure under the original detector) now succeeds because the
// reloaded detector has no rules and accepts every status code.
func TestHTTPReloadSwapsDetector(t *testing.T) {
	t.Parallel()

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(dest.Close)

	srv, proxyURL := startHTTPListener(t)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("first GET status = %d, want 502 (cascade after every retry returned 429)", resp.StatusCode)
	}

	// Reload swaps in an empty status detector: 429 is no longer treated
	// as a failure, so the next request flows through to the client with
	// the original 429 the destination served.
	srv.Reload(failure.NewStatusDetector(nil), 5)

	resp2, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("post-reload GET: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Errorf("post-reload status = %d, want 429 (no detector means no retry)", resp2.StatusCode)
	}
}

// TestHTTPReloadRejectsBadRetryCap proves the guard: a misconfigured
// reload that hands in retryCap < 1 must not zero out the live cap.
func TestHTTPReloadRejectsBadRetryCap(t *testing.T) {
	t.Parallel()

	srv, proxyURL := startHTTPListener(t)
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dest.Close)

	srv.Reload(failure.NewStatusDetector(config.DefaultStatusRules), 0)

	client := proxyClient(t, proxyURL)
	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("post-bad-reload GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("post-bad-reload status = %d, want 200", resp.StatusCode)
	}
}

// TestCloseTransportsExceptIsSafeWithLiveTransports calls
// CloseTransportsExcept against a listener that has a cached transport
// to make sure the call does not panic and does not block on the cache
// mutex while the request path is using it. The cache itself is private,
// so this is a smoke test, not a count assertion; it covers the call
// site is wired and is concurrent-safe.
func TestCloseTransportsExceptIsSafeWithLiveTransports(t *testing.T) {
	t.Parallel()

	srv, proxyURL := startHTTPListener(t)
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dest.Close)

	// Seed the transport cache by issuing one request.
	client := proxyClient(t, proxyURL)
	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("seed GET: %v", err)
	}
	_ = resp.Body.Close()

	// Drop everything from the cache.
	srv.CloseTransportsExcept(map[string]struct{}{})

	// Subsequent request still succeeds: a fresh transport gets built.
	resp2, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("post-close GET: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("post-close status = %d, want 200", resp2.StatusCode)
	}
}
