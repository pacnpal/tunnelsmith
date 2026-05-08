package listener_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// directPoolWith returns a pool whose entries are direct upstreams with the
// given ids, in priority order. The forward path picks each id in turn until
// one succeeds; pairing this with a destination handler that returns
// per-attempt status codes is enough to drive the Phase 5 status-cycling
// tests without spinning up real proxy upstreams.
func directPoolWith(t *testing.T, ids ...string) *upstream.Pool {
	t.Helper()
	entries := make([]upstream.PoolEntry, 0, len(ids))
	for i, id := range ids {
		up, err := upstream.New(config.UpstreamConfig{ID: id, Kind: config.KindDirect}, 5*time.Second)
		if err != nil {
			t.Fatalf("build upstream %q: %v", id, err)
		}
		entries = append(entries, upstream.PoolEntry{Up: up, Priority: 10 * (i + 1)})
	}
	pool, err := upstream.NewPool(entries, len(ids), quietLogger())
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	return pool
}

// startForwardListener spins up an HTTP listener with the given scoreboard,
// detector, and retry cap. Mirrors startHTTPListener but lets the caller
// control the detector and cap so each status-detection test can construct
// the configuration it needs.
func startForwardListener(t *testing.T, sb *scoreboard.Scoreboard, detector *failure.StatusDetector, retryCap int) (*listener.HTTPServer, *url.URL) {
	t.Helper()
	srv, err := listener.NewHTTP("127.0.0.1:0", sb, detector, retryCap, quietLogger())
	if err != nil {
		t.Fatalf("build http listener: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(context.Background()) }()
	select {
	case <-srv.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("http listener did not bind in time")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Logf("http shutdown: %v", err)
		}
		// Bound the wait for Serve to return: if a regression keeps it
		// blocked, the whole suite would otherwise hang on this Cleanup.
		select {
		case err := <-serveErr:
			if err != nil {
				t.Logf("http serve returned: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Logf("http serve did not return after shutdown")
		}
	})
	return srv, &url.URL{Scheme: "http", Host: srv.Addr().String()}
}

// proxyClient builds an http.Client whose transport routes every request
// through proxyURL. Closes idle conns at test cleanup so the test does not
// leave dangling sockets.
func proxyClient(t *testing.T, proxyURL *url.URL) *http.Client {
	t.Helper()
	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			DisableKeepAlives: true,
		},
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// TestForwardCyclesThroughUpstreamsOn429 covers the headline Phase 5
// scenario: three upstreams each return 429 once and then succeed. The
// listener must cycle through them and surface a 200 to the client. The
// destination's hit count of exactly 3 proves the listener rotated
// through all three upstreams (one request per upstream) within a single
// in-flight client request rather than retrying through the same upstream
// or stopping early.
//
// The pool is built from three direct upstreams; the destination handler
// returns 429 for the first two requests (one per upstream) and 200 on the
// third. The retry cap is 5, so the listener has room to walk all three.
func TestForwardCyclesThroughUpstreamsOn429(t *testing.T) {
	t.Parallel()

	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hit.Add(1)
		if n < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "third-time-lucky")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b", "c")
	sb := scoreboardFor(t, pool)
	detector := failure.NewStatusDetector(config.DefaultStatusRules)
	_, proxyURL := startForwardListener(t, sb, detector, 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "third-time-lucky" {
		t.Fatalf("body = %q, want %q", string(body), "third-time-lucky")
	}
	if hit.Load() != 3 {
		t.Fatalf("destination hit count = %d, want 3 (one per upstream)", hit.Load())
	}
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "2" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want %q", got, "2")
	}
	if got := resp.Header.Get("X-Tunnelsmith-Upstream"); got == "" {
		t.Error("X-Tunnelsmith-Upstream missing on success response")
	}
}

// TestForwardSuccessHeaders confirms a happy-path success response carries
// X-Tunnelsmith-Upstream (the upstream that served) and X-Tunnelsmith-Retries=0.
func TestForwardSuccessHeaders(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "only")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Tunnelsmith-Upstream"); got != "only" {
		t.Errorf("X-Tunnelsmith-Upstream = %q, want %q", got, "only")
	}
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "0" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want %q", got, "0")
	}
}

// TestForwardCascadeOn429AllUpstreams covers the second cascade trigger:
// every upstream returns 429 for the same request. The listener exhausts
// its retry budget and 502s, with X-Tunnelsmith-Cascade naming the host.
func TestForwardCascadeOn429AllUpstreams(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 2)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	destURL, _ := url.Parse(dest.URL)
	wantHost := destURL.Hostname()
	if got := resp.Header.Get("X-Tunnelsmith-Cascade"); got != wantHost {
		t.Errorf("X-Tunnelsmith-Cascade = %q, want %q", got, wantHost)
	}
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "2" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want 2", got)
	}
}

// TestForwardHonors429RetryAfterCooldown drives one 429 with Retry-After: 30,
// then asserts the resulting cooldown on the (host, upstream) entry is
// roughly 30 seconds, not the configured default cooldown for the kind.
func TestForwardHonors429RetryAfterCooldown(t *testing.T) {
	t.Parallel()
	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hit.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	destURL, _ := url.Parse(dest.URL)
	host := destURL.Hostname()
	now := time.Now()
	var penalized scoreboard.EntrySnapshot
	var found bool
	for _, e := range sb.Snapshot() {
		if e.Host == host && e.Score < 0 {
			penalized = e
			found = true
		}
	}
	if !found {
		t.Fatal("no penalized entry found for host; expected the 429-served upstream to be penalized")
	}
	if penalized.CooldownUntil.IsZero() {
		t.Fatal("penalized entry has zero cooldown; expected ~30s from Retry-After")
	}
	delta := penalized.CooldownUntil.Sub(now)
	if delta < 28*time.Second || delta > 32*time.Second {
		t.Errorf("cooldown = %v from now, want ~30s (RFC 7231 Retry-After honored)", delta)
	}
}

// TestForwardHonorsHTTPDateRetryAfter confirms an IMF-fixdate Retry-After
// value is parsed and applied to the cooldown. The server announces a date
// 45 seconds in the future; the resulting cooldown should land near 45s.
func TestForwardHonorsHTTPDateRetryAfter(t *testing.T) {
	t.Parallel()
	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hit.Add(1)
		if n == 1 {
			when := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
			w.Header().Set("Retry-After", when)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	destURL, _ := url.Parse(dest.URL)
	host := destURL.Hostname()
	now := time.Now()
	var found bool
	var cooldown time.Duration
	for _, e := range sb.Snapshot() {
		if e.Host == host && !e.CooldownUntil.IsZero() {
			cooldown = e.CooldownUntil.Sub(now)
			found = true
		}
	}
	if !found {
		t.Fatal("no entry with cooldown found; expected the 429-served upstream to carry one")
	}
	if cooldown < 40*time.Second || cooldown > 50*time.Second {
		t.Errorf("cooldown = %v, want ~45s (HTTP-date Retry-After honored)", cooldown)
	}
}

// TestForward403LongCooldown drives a single 403 and asserts the cooldown
// matches the configured 30-minute default for KindForbidden, not 429's
// 120-second default.
func TestForward403LongCooldown(t *testing.T) {
	t.Parallel()
	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hit.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	destURL, _ := url.Parse(dest.URL)
	host := destURL.Hostname()
	now := time.Now()
	var cooldown time.Duration
	for _, e := range sb.Snapshot() {
		if e.Host == host && e.Score < 0 {
			cooldown = e.CooldownUntil.Sub(now)
		}
	}
	// Default 403 cooldown is 30 minutes; a small jitter window is fine.
	if cooldown < 29*time.Minute || cooldown > 31*time.Minute {
		t.Errorf("403 cooldown = %v, want ~30m", cooldown)
	}
}

// TestForward451LongestCooldown drives a single 451 and asserts the
// cooldown matches the configured 6-hour default.
func TestForward451LongestCooldown(t *testing.T) {
	t.Parallel()
	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hit.Add(1) == 1 {
			w.WriteHeader(http.StatusUnavailableForLegalReasons)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	destURL, _ := url.Parse(dest.URL)
	host := destURL.Hostname()
	now := time.Now()
	var cooldown time.Duration
	for _, e := range sb.Snapshot() {
		if e.Host == host && e.Score < 0 {
			cooldown = e.CooldownUntil.Sub(now)
		}
	}
	if cooldown < 5*time.Hour+55*time.Minute || cooldown > 6*time.Hour+5*time.Minute {
		t.Errorf("451 cooldown = %v, want ~6h", cooldown)
	}
}

// TestForward5xxIsNotFailure confirms the listener forwards a 5xx response
// to the client unchanged. 5xx is the destination's problem; rotating to
// another exit will not help. No retry, no penalty, no header surgery
// beyond the standard X-Tunnelsmith-Upstream/Retries pair on success.
func TestForward5xxIsNotFailure(t *testing.T) {
	t.Parallel()
	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "destination is down")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if hit.Load() != 1 {
		t.Errorf("destination hit count = %d, want 1 (5xx is not retried)", hit.Load())
	}
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "0" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want %q", got, "0")
	}
	destURL, _ := url.Parse(dest.URL)
	host := destURL.Hostname()
	for _, e := range sb.Snapshot() {
		if e.Host == host && e.Score < 0 {
			t.Errorf("upstream %s got penalized for a 5xx; should not", e.UpstreamID)
		}
	}
}

// TestForward404IsNotFailure confirms the listener does not treat 404 as an
// upstream failure. Rotating exits cannot fix a 404; treating it as one
// would create infinite-retry storms on legitimately bad URLs.
func TestForward404IsNotFailure(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	destURL, _ := url.Parse(dest.URL)
	host := destURL.Hostname()
	for _, e := range sb.Snapshot() {
		if e.Host == host && e.Score < 0 {
			t.Errorf("upstream %s got penalized for a 404; should not", e.UpstreamID)
		}
	}
}

// TestForwardCascadeWhenPickReportsCascade confirms the listener returns
// the cascade headers when sb.Pick returns ErrCascadeCooling on the first
// attempt (rather than the loop tripping cascade itself). The scoreboard
// is built with a real CascadeTTL and tripped manually before the request
// runs so Pick short-circuits.
func TestForwardCascadeWhenPickReportsCascade(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "should-not-reach")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a")
	cfg := scoreboard.Config{
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 3, Cooldown: 30 * time.Second},
		},
		SuccessWeight:  1,
		ScoreCap:       10,
		DecayInterval:  5 * time.Minute,
		CascadeTTL:     30 * time.Second,
		DebounceWindow: 100 * time.Millisecond,
	}
	sb, err := scoreboard.New(pool, cfg, scoreboard.WithLogger(quietLogger()))
	if err != nil {
		t.Fatalf("build scoreboard: %v", err)
	}
	t.Cleanup(sb.Stop)

	destURL, _ := url.Parse(dest.URL)
	host := destURL.Hostname()
	sb.TripCascade(host)

	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Tunnelsmith-Cascade"); got != host {
		t.Errorf("X-Tunnelsmith-Cascade = %q, want %q", got, host)
	}
	if got, _ := strconv.Atoi(resp.Header.Get("X-Tunnelsmith-Retries")); got != 0 {
		t.Errorf("X-Tunnelsmith-Retries = %q, want 0 (no attempt fired)", resp.Header.Get("X-Tunnelsmith-Retries"))
	}
}

// TestForwardHonorsRetryAfterZero pins down the bug Copilot flagged on
// PR #13: Retry-After: 0 is a legal RFC 7231 §7.1.3 value meaning "retry
// immediately". The detector and scoreboard must honor it as an explicit
// zero cooldown, not fall back to the kind's configured default. The
// resulting (host, upstream) entry has the penalty applied (score < 0)
// but no cooldown extension.
func TestForwardHonorsRetryAfterZero(t *testing.T) {
	t.Parallel()
	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hit.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	destURL, _ := url.Parse(dest.URL)
	host := destURL.Hostname()
	var penalized scoreboard.EntrySnapshot
	var found bool
	for _, e := range sb.Snapshot() {
		if e.Host == host && e.Score < 0 {
			penalized = e
			found = true
		}
	}
	if !found {
		t.Fatal("no penalized entry for host; expected the 429-served upstream to be penalized")
	}
	// CooldownUntil zero (or in the past relative to now) means the
	// upstream is immediately re-eligible. The default 429 cooldown is
	// 120s; if Retry-After: 0 had been ignored, this entry would carry a
	// far-future expiry instead.
	if !penalized.CooldownUntil.IsZero() && penalized.CooldownUntil.After(time.Now()) {
		t.Errorf("CooldownUntil = %v, want zero / past (Retry-After: 0 should honor 'retry immediately')",
			penalized.CooldownUntil)
	}
}

// TestForwardOversizeBodyRejected confirms the listener bounds the
// request-body buffer it builds for retry replay. A request whose body
// exceeds the cap is rejected without ever reaching the destination.
//
// The client may surface either a clean 413 response or a broken-pipe
// write error depending on how the kernel schedules the close: when the
// proxy responds 413 from the Content-Length pre-check and closes the
// conn while the client is still streaming the body, Go's transport
// races the response read against the body write and either side may
// win. Both outcomes prove what the test cares about: the proxy did not
// forward the oversize body to the destination.
func TestForwardOversizeBodyRejected(t *testing.T) {
	t.Parallel()
	var destHits atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destHits.Add(1)
		_, _ = io.WriteString(w, "should-not-reach")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	const oversize = 9 << 20 // 9 MiB, just above the 8 MiB listener cap
	body := bytes.Repeat([]byte("A"), oversize)
	req, err := http.NewRequest(http.MethodPost, dest.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", resp.StatusCode)
		}
	}
	if destHits.Load() != 0 {
		t.Errorf("destination got %d hits; oversize body should not have been forwarded", destHits.Load())
	}
}

// TestForwardChunkedRequestBodyWorks confirms the listener forwards a
// chunked-encoded request without tripping the transport's "request has
// both Content-Length and Transfer-Encoding" rejection. The request
// arrives with TransferEncoding=["chunked"]; newOutReq must clear that
// when it switches to a buffered body with ContentLength.
func TestForwardChunkedRequestBodyWorks(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("dest read body: %v", err)
		}
		if string(body) != "chunked-payload" {
			t.Errorf("dest body = %q, want %q", string(body), "chunked-payload")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	// Build a request whose Body is non-seekable so net/http has to send
	// it chunked: ContentLength is unknown.
	body := io.NopCloser(strings.NewReader("chunked-payload"))
	req, err := http.NewRequest(http.MethodPost, dest.URL, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.ContentLength = -1 // -1 marks unknown length so the transport chunks

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestForwardRejectsUnsupportedScheme covers Copilot's review on PR #13:
// a forward request with a non-http(s) scheme would otherwise let
// http.Transport.RoundTrip fail deterministically through every upstream,
// burn the retry budget, and trip cascade for a host the listener never
// could have served. The listener must reject up front.
func TestForwardRejectsUnsupportedScheme(t *testing.T) {
	t.Parallel()
	pool := directPoolWith(t, "a")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)

	// Build the raw request manually so we can ship a non-http URL the
	// stdlib client would otherwise refuse to send.
	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	const req = "GET ftp://example.com/foo HTTP/1.1\r\nHost: example.com\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// Cascade must NOT trip: the listener rejected before any dial fired.
	if got := sb.CascadeUntil("example.com"); !got.IsZero() {
		t.Errorf("CascadeUntil(example.com) = %v, want zero (no upstream was tried)", got)
	}
}

// TestForwardPassesGzippedBodyThrough covers Copilot's review on PR #13:
// the per-upstream Transport must not auto-add Accept-Encoding and
// silently decompress, because that would strip Content-Encoding and
// Content-Length on the way back to the client and break byte-for-byte
// proxy transparency. The destination here serves a gzipped body with
// the matching Content-Encoding header; the client must see the encoded
// bytes and the header verbatim.
//
// Both the test client and the proxy must keep DisableCompression=true.
// Without it on either side, net/http would auto-add Accept-Encoding to
// the outgoing request and transparently decompress the response,
// hiding whether the proxy itself was preserving the encoding.
func TestForwardPassesGzippedBodyThrough(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	gz := gzip.NewWriter(&encoded)
	if _, err := gz.Write([]byte("compressed payload")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	gzippedBody := encoded.Bytes()

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(gzippedBody)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzippedBody)
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:              http.ProxyURL(proxyURL),
			DisableKeepAlives:  true,
			DisableCompression: true,
		},
	}
	t.Cleanup(client.CloseIdleConnections)

	req, err := http.NewRequest(http.MethodGet, dest.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q (proxy stripped or rewrote it)", got, "gzip")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, gzippedBody) {
		t.Fatalf("body bytes differ: len(got)=%d len(want)=%d (proxy decompressed transparently)",
			len(got), len(gzippedBody))
	}
}

// TestForwardNonDialRoundTripErrorSkipsPenalty covers Copilot's review on
// PR #13: a RoundTrip error that is neither a timeout nor a refusal should
// not be classified as a dial-level failure. Recording such an error as
// KindRefused would unfairly penalize the upstream for problems that may
// belong to the destination (TLS misconfiguration, HTTP parse errors,
// server hangup mid-response, ...). The listener still rotates to the
// next upstream, but the scoreboard does not blame the upstream.
//
// Stand-in here: a destination that accepts the TCP conn and immediately
// closes it. The proxy's RoundTrip writes the request and reads EOF on
// the response, which neither IsTimeout nor IsConnectionRefused matches.
func TestForwardNonDialRoundTripErrorSkipsPenalty(t *testing.T) {
	t.Parallel()
	dest, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dest: %v", err)
	}
	t.Cleanup(func() { _ = dest.Close() })
	go func() {
		for {
			c, err := dest.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	pool := directPoolWith(t, "a")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 2)
	client := proxyClient(t, proxyURL)

	destURL := &url.URL{Scheme: "http", Host: dest.Addr().String()}
	resp, err := client.Get(destURL.String())
	if err == nil {
		_ = resp.Body.Close()
		// Acceptable shape too: 502 from the listener's exhaustion path.
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (the destination drops the conn)", resp.StatusCode)
		}
	}

	// The actual property under test: no upstream got penalized for a
	// non-dial RoundTrip error. Score for "a" should stay at zero.
	host := destURL.Hostname()
	for _, e := range sb.Snapshot() {
		if e.Host == host && e.UpstreamID == "a" {
			if e.Score < 0 {
				t.Errorf("a.Score = %v, want >= 0 (non-dial RoundTrip error must not penalize upstream)", e.Score)
			}
			if e.GlobalFailure > 0 {
				t.Errorf("a.GlobalFailure = %d, want 0 (no penalty event recorded)", e.GlobalFailure)
			}
		}
	}
}

// TestForwardRejectsEmptyHost covers Copilot's review on PR #13: a
// malformed absolute-form URI like "http:/path" is IsAbs() = true but
// has no host. Without an explicit empty-host check the listener would
// run the retry loop with host = "" and trip cascade for the empty
// string. The listener must reject up front so the scoreboard never
// sees an empty host key.
func TestForwardRejectsEmptyHost(t *testing.T) {
	t.Parallel()
	pool := directPoolWith(t, "a")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)

	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Request line that parses as IsAbs() = true with an empty URL host.
	// Host: is set to prove the listener rejects based on URL host
	// absence rather than falling back to Host and entering the retry
	// loop with a request RoundTrip cannot serve.
	const req = "GET http:/path HTTP/1.1\r\nHost: example.com\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// Empty-string cascade key must NOT have been tripped.
	if got := sb.CascadeUntil(""); !got.IsZero() {
		t.Errorf("CascadeUntil(\"\") = %v, want zero (no upstream was tried)", got)
	}
	// Host-header key must also stay clean: malformed URL host must be
	// rejected before any attempt can trip cascade for example.com.
	if got := sb.CascadeUntil("example.com"); !got.IsZero() {
		t.Errorf("CascadeUntil(\"example.com\") = %v, want zero (no upstream was tried)", got)
	}
}

// TestForwardStripsDestinationTunnelsmithHeaders covers Copilot's review
// on PR #13: a destination that ships X-Tunnelsmith-* headers (e.g. a
// spoofed X-Tunnelsmith-Cascade or a stale X-Tunnelsmith-Upstream from
// a chained Tunnelsmith) must not have those forwarded to the client.
// The proxy owns that header namespace; only Tunnelsmith's own values
// for the response shape are allowed through.
func TestForwardStripsDestinationTunnelsmithHeaders(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tunnelsmith-Cascade", "spoofed.example")
		w.Header().Set("X-Tunnelsmith-Upstream", "spoofed-upstream")
		w.Header().Set("X-Tunnelsmith-Retries", "999")
		w.Header().Set("X-Tunnelsmith-Future", "irrelevant")
		w.Header().Set("X-Real-Header", "untouched")
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "real-upstream")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Tunnelsmith-Cascade"); got != "" {
		t.Errorf("X-Tunnelsmith-Cascade = %q, want \"\" (destination spoof must be stripped)", got)
	}
	if got := resp.Header.Get("X-Tunnelsmith-Future"); got != "" {
		t.Errorf("X-Tunnelsmith-Future = %q, want \"\" (any X-Tunnelsmith-* must be stripped)", got)
	}
	if got := resp.Header.Get("X-Tunnelsmith-Upstream"); got != "real-upstream" {
		t.Errorf("X-Tunnelsmith-Upstream = %q, want %q (proxy's value, not the spoof)", got, "real-upstream")
	}
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "0" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want %q (proxy's value, not the spoof)", got, "0")
	}
	if got := resp.Header.Get("X-Real-Header"); got != "untouched" {
		t.Errorf("X-Real-Header = %q, want %q (only X-Tunnelsmith-* should be stripped)", got, "untouched")
	}
}

// TestForwardSinglePool429RetryCapStops confirms a 429-returning pool with
// only one upstream halts after one attempt because the retry cap caps total
// attempts and the only upstream is now in tried[].
func TestForwardSinglePool429RetryCapStops(t *testing.T) {
	t.Parallel()
	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "only")
	sb := scoreboardFor(t, pool)
	_, proxyURL := startForwardListener(t, sb, failure.NewStatusDetector(config.DefaultStatusRules), 5)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if hit.Load() != 1 {
		t.Errorf("destination hit count = %d, want 1 (single upstream, single attempt)", hit.Load())
	}
}
