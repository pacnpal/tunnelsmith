package listener_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
		_ = srv.Shutdown(ctx)
		<-serveErr
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
// destination's hit count proves only one attempt landed: the in-flight
// request rotated through 429-tagged upstreams to a fresh one, not that the
// request was retried against the same destination.
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
