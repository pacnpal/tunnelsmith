package listener_test

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// startForwardListenerWithRules mirrors startForwardListener but attaches a
// compiled rule set and a body buffer cap. Tests pass nil rules / 0 KB to
// land back on the no-body-inspection default.
func startForwardListenerWithRules(t *testing.T, sb *scoreboard.Scoreboard, detector *failure.StatusDetector, retryCap int, rules *upstream.RuleSet, bodyKB int) (*listener.HTTPServer, *url.URL) {
	t.Helper()
	srv, err := listener.NewHTTP(
		"127.0.0.1:0", sb, detector, retryCap, quietLogger(),
		listener.WithHTTPRules(rules),
		listener.WithHTTPBodyBufferKB(bodyKB),
	)
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

func mustRuleSet(t *testing.T, cfgs ...config.RuleConfig) *upstream.RuleSet {
	t.Helper()
	rs, err := upstream.NewRuleSet(cfgs)
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	return rs
}

// TestForwardBodyMatchTriggersRetry covers the headline Phase 8 scenario:
// the destination returns 200 with a "not available in your region" page
// and the listener cycles through to the next upstream, which serves a
// clean 200. The destination's hit count must be exactly 2 (one per
// upstream); the client must see the clean response, not the geo-block
// page.
func TestForwardBodyMatchTriggersRetry(t *testing.T) {
	t.Parallel()

	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hit.Add(1)
		if n == 1 {
			_, _ = io.WriteString(w, "Sorry, this content is not available in your region.")
			return
		}
		_, _ = io.WriteString(w, "actual content")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	rules := mustRuleSet(t, config.RuleConfig{
		HostGlob:  "*",
		Prefer:    []string{"a", "b"},
		BodyRegex: []string{"(?i)not available in your region"},
	})
	_, proxyURL := startForwardListenerWithRules(t, sb, defaultDetector(), 5, rules, 32)
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
	if string(body) != "actual content" {
		t.Errorf("body = %q, want %q (the second upstream's clean response)", string(body), "actual content")
	}
	if hit.Load() != 2 {
		t.Errorf("destination hit count = %d, want 2 (geo-block then retry)", hit.Load())
	}
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "1" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want 1", got)
	}
}

// TestForwardBodyMatchAllUpstreamsCascade covers the case where every
// upstream returns the geo-block page. The listener exhausts retries and
// 502s with the cascade header set.
func TestForwardBodyMatchAllUpstreamsCascade(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "geo-locked content here")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	rules := mustRuleSet(t, config.RuleConfig{
		HostGlob:  "*",
		Prefer:    []string{"a", "b"},
		BodyRegex: []string{"geo-locked"},
	})
	_, proxyURL := startForwardListenerWithRules(t, sb, defaultDetector(), 2, rules, 32)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Tunnelsmith-Cascade"); got == "" {
		t.Errorf("X-Tunnelsmith-Cascade header missing on cascade response")
	}
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "2" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want 2", got)
	}
}

// TestForwardBodyMatchPassesCleanResponse checks that a benign body (no
// pattern match) still streams to the client byte-for-byte. The Replay
// reader is the integration risk here: any byte loss on the first
// inspection pass would corrupt every clean forward request.
func TestForwardBodyMatchPassesCleanResponse(t *testing.T) {
	t.Parallel()
	const payload = "<html><body>nothing geo-blocked here, just normal page content</body></html>"
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "only")
	sb := scoreboardFor(t, pool)
	rules := mustRuleSet(t, config.RuleConfig{
		HostGlob:  "*",
		Prefer:    []string{"only"},
		BodyRegex: []string{"not available", "region.?lock"},
	})
	_, proxyURL := startForwardListenerWithRules(t, sb, defaultDetector(), 5, rules, 32)
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
	if string(body) != payload {
		t.Errorf("body = %q, want %q (byte-for-byte)", string(body), payload)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/html" {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "0" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want 0", got)
	}
}

// TestForwardBodyMatchSkipsGzipBody ensures gzip-encoded bodies bypass
// regex inspection (regex over compressed bytes would never match
// useful patterns). The pattern matches the cleartext but the body is
// gzip; the client must still receive the response untouched and no
// retry must fire.
func TestForwardBodyMatchSkipsGzipBody(t *testing.T) {
	t.Parallel()
	const cleartext = "this content is not available in your region"
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(cleartext))
		_ = gz.Close()
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "only")
	sb := scoreboardFor(t, pool)
	rules := mustRuleSet(t, config.RuleConfig{
		HostGlob:  "*",
		Prefer:    []string{"only"},
		BodyRegex: []string{"not available in your region"},
	})
	_, proxyURL := startForwardListenerWithRules(t, sb, defaultDetector(), 5, rules, 32)
	client := proxyClient(t, proxyURL)

	// Set Accept-Encoding explicitly on the request: when the user
	// declares it, net/http's client does not auto-decode the response,
	// so resp.Body retains the gzip framing the upstream wrote. That
	// lets the test gzip.NewReader the body and confirm the bytes
	// reached the client unaltered.
	req, err := http.NewRequest(http.MethodGet, dest.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "0" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want 0 (gzip body must skip inspection)", got)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip (header must reach client)", got)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if string(got) != cleartext {
		t.Errorf("decoded body = %q, want %q", string(got), cleartext)
	}
}

// TestForwardBodyMatchHostNotInRule confirms a request whose host does
// not match any rule bypasses inspection entirely. A pattern that would
// otherwise match must NOT trigger a retry.
func TestForwardBodyMatchHostNotInRule(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "this content is not available")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "only")
	sb := scoreboardFor(t, pool)
	// Rule globs to a different host than the test request will use.
	rules := mustRuleSet(t, config.RuleConfig{
		HostGlob:  "*.somewhere.else",
		Prefer:    []string{"only"},
		BodyRegex: []string{"not available"},
	})
	_, proxyURL := startForwardListenerWithRules(t, sb, defaultDetector(), 5, rules, 32)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "0" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want 0 (host outside any rule should skip body inspection)", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not available") {
		t.Errorf("body = %q; want the pattern's text passed through unchanged", string(body))
	}
}

// TestForwardBodyMatchHonorsBufferLimit confirms that a pattern lying
// past the configured buffer cap never triggers a match. The body has
// padding past the cap followed by the pattern; with the cap small
// enough that the pattern lies past it, the listener must NOT detect
// and the client must receive the full body.
func TestForwardBodyMatchHonorsBufferLimit(t *testing.T) {
	t.Parallel()
	// 1 KiB cap. Build a body whose pattern starts well past the cap
	// so the inspector never sees it. 4 KiB of padding plus the
	// trigger comfortably exceeds 1 KiB.
	padding := strings.Repeat("a", 4096)
	tail := "geo-block-trigger"
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, padding+tail)
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "only")
	sb := scoreboardFor(t, pool)
	rules := mustRuleSet(t, config.RuleConfig{
		HostGlob:  "*",
		Prefer:    []string{"only"},
		BodyRegex: []string{"geo-block-trigger"},
	})
	_, proxyURL := startForwardListenerWithRules(t, sb, defaultDetector(), 5, rules, 1)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "0" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want 0 (pattern lay past 1 KiB cap)", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != padding+tail {
		t.Errorf("body length mismatch: got %d, want %d", len(body), len(padding)+len(tail))
	}
}

// TestForwardBodyBufferZeroDisablesInspection covers the explicit
// "disable inspection at runtime" path: even with patterns configured
// on a matching rule, body_buffer_kb=0 should bypass inspection
// entirely and stream the full body to the client. Mirrors the
// behavior of WithHTTPBodyBufferKB(0) and ReloadBodyBufferKB(0).
func TestForwardBodyBufferZeroDisablesInspection(t *testing.T) {
	t.Parallel()
	const payload = "Sorry, this content is not available in your region."
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "only")
	sb := scoreboardFor(t, pool)
	rules := mustRuleSet(t, config.RuleConfig{
		HostGlob:  "*",
		Prefer:    []string{"only"},
		BodyRegex: []string{"not available in your region"},
	})
	_, proxyURL := startForwardListenerWithRules(t, sb, defaultDetector(), 5, rules, 0)
	client := proxyClient(t, proxyURL)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("X-Tunnelsmith-Retries"); got != "0" {
		t.Errorf("X-Tunnelsmith-Retries = %q, want 0 (body_buffer_kb=0 disables inspection)", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != payload {
		t.Errorf("body = %q, want full payload", string(body))
	}
}

// TestForwardReloadRulesSwapsBehavior covers hot-reload: starting with no
// rules, then attaching a rule via ReloadRules. The first request streams
// the geo-block body to the client; the second (after ReloadRules) cycles
// through to the next upstream.
func TestForwardReloadRulesSwapsBehavior(t *testing.T) {
	t.Parallel()
	var hit atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hit.Add(1)
		if n%2 == 1 {
			_, _ = io.WriteString(w, "Sorry, this content is not available in your region.")
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	pool := directPoolWith(t, "a", "b")
	sb := scoreboardFor(t, pool)
	srv, proxyURL := startForwardListenerWithRules(t, sb, defaultDetector(), 5, nil, 32)
	client := proxyClient(t, proxyURL)

	// No rules attached yet: the geo-block body reaches the client unchanged.
	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "not available") {
		t.Fatalf("first response body = %q; want geo-block content (no rule attached yet)", string(body))
	}

	// Attach the rule via hot reload.
	srv.ReloadRules(mustRuleSet(t, config.RuleConfig{
		HostGlob:  "*",
		Prefer:    []string{"a", "b"},
		BodyRegex: []string{"not available in your region"},
	}))

	// Now the listener should cycle to the second upstream. The destination
	// returns the geo-block on hit 3 and the clean body on hit 4 because
	// the cycle alternates.
	resp, err = client.Get(dest.URL)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Errorf("second response body = %q; want %q (rule should retry past the geo-block)", string(body), "ok")
	}
}
