package listener_test

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// refusedAddr returns a TCP address that will immediately return ECONNREFUSED.
// It works by binding a listener to a random port, recording the address, and
// closing the listener before returning. On Linux/macOS the OS responds with
// ECONNREFUSED as soon as nothing is listening on the port.
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// sbWithRefusedPolicy builds a single-upstream scoreboard whose KindRefused
// policy is non-zero so RecordFailure has a visible score/cooldown effect.
// ConnectionRefused is set to true to match the production default and avoid
// the scoreboard's own DialFor gate from masking the listener-level gate.
func sbWithRefusedPolicy(t *testing.T, ids ...string) (*scoreboard.Scoreboard, *upstream.Pool) {
	t.Helper()
	pool := directPoolWith(t, ids...)
	cfg := scoreboard.Config{
		ConnectionRefused: true,
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 3, Cooldown: 30 * time.Second},
		},
		SuccessWeight:  1,
		ScoreCap:       10,
		DecayInterval:  5 * time.Minute,
		DebounceWindow: 100 * time.Millisecond,
	}
	sb, err := scoreboard.New(pool, cfg, scoreboard.WithLogger(quietLogger()))
	if err != nil {
		t.Fatalf("scoreboard.New: %v", err)
	}
	t.Cleanup(sb.Stop)
	return sb, pool
}

// startForwardListenerOpts starts a forward HTTP listener with caller-supplied
// options appended after the required arguments. It mirrors startForwardListener
// but allows per-test options (e.g. WithHTTPConnectionRefused) without
// requiring the caller to set up the full serve/cleanup dance.
func startForwardListenerOpts(t *testing.T, sb *scoreboard.Scoreboard, detector *failure.StatusDetector, retryCap int, opts ...listener.HTTPOption) (*listener.HTTPServer, *url.URL) {
	t.Helper()
	srv, err := listener.NewHTTP("127.0.0.1:0", sb, detector, retryCap, quietLogger(), opts...)
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

// forwardRefusedFailureCount returns the GlobalFailure count for (host, upstreamID)
// in the scoreboard snapshot, or 0 if no entry exists.
func forwardRefusedFailureCount(sb *scoreboard.Scoreboard, host, upstreamID string) uint64 {
	for _, e := range sb.Snapshot() {
		if e.Host == host && e.UpstreamID == upstreamID {
			return e.GlobalFailure
		}
	}
	return 0
}

// TestForwardConnectionRefusedGate covers the connection_refused gate in the
// HTTP forward path (handleForward). When the gate is on (default), a dial
// that returns ECONNREFUSED calls RecordFailure against the upstream. When the
// gate is off, RecordFailure is skipped so the upstream's score is unchanged,
// even though the dial is still logged and metered.
func TestForwardConnectionRefusedGate(t *testing.T) {
	t.Parallel()

	detector := failure.NewStatusDetector(config.DefaultStatusRules)

	t.Run("ECONNREFUSED is scored when gate is on", func(t *testing.T) {
		t.Parallel()
		addr := refusedAddr(t)
		sb, _ := sbWithRefusedPolicy(t, "a")

		// NewHTTP defaults connectionRefused=true so no explicit option needed.
		_, proxyURL := startForwardListenerOpts(t, sb, detector, 2)
		client := proxyClient(t, proxyURL)

		destURL := &url.URL{Scheme: "http", Host: addr}
		resp, err := client.Get(destURL.String())
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (ECONNREFUSED destination)", resp.StatusCode)
		}

		host, _, _ := net.SplitHostPort(addr)
		failures := forwardRefusedFailureCount(sb, host, "a")
		if failures == 0 {
			t.Error("upstream 'a' GlobalFailure = 0; expected RecordFailure to have been called (gate is on)")
		}
	})

	t.Run("ECONNREFUSED is not scored when gate is off", func(t *testing.T) {
		t.Parallel()
		addr := refusedAddr(t)
		sb, _ := sbWithRefusedPolicy(t, "a")

		_, proxyURL := startForwardListenerOpts(t, sb, detector, 2,
			listener.WithHTTPConnectionRefused(false),
		)
		client := proxyClient(t, proxyURL)

		destURL := &url.URL{Scheme: "http", Host: addr}
		resp, err := client.Get(destURL.String())
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (ECONNREFUSED destination)", resp.StatusCode)
		}

		host, _, _ := net.SplitHostPort(addr)
		failures := forwardRefusedFailureCount(sb, host, "a")
		if failures != 0 {
			t.Errorf("upstream 'a' GlobalFailure = %d; expected 0 (gate is off, ECONNREFUSED must not score)", failures)
		}
	})

	t.Run("ReloadConnectionRefused enables scoring live", func(t *testing.T) {
		t.Parallel()
		addr := refusedAddr(t)
		sb, _ := sbWithRefusedPolicy(t, "a")

		srv, proxyURL := startForwardListenerOpts(t, sb, detector, 2,
			listener.WithHTTPConnectionRefused(false),
		)
		client := proxyClient(t, proxyURL)

		destURL := &url.URL{Scheme: "http", Host: addr}

		// First request: gate off — no penalty.
		resp, err := client.Get(destURL.String())
		if err != nil {
			t.Fatalf("first GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("first GET status = %d, want 502 (ECONNREFUSED destination)", resp.StatusCode)
		}

		host, _, _ := net.SplitHostPort(addr)
		if got := forwardRefusedFailureCount(sb, host, "a"); got != 0 {
			t.Fatalf("before reload: upstream 'a' GlobalFailure = %d, want 0 (gate off)", got)
		}

		// Flip the gate on live.
		srv.ReloadConnectionRefused(true)

		// Second request: gate on — penalty must be recorded.
		resp2, err := client.Get(destURL.String())
		if err != nil {
			t.Fatalf("second GET: %v", err)
		}
		_ = resp2.Body.Close()
		if resp2.StatusCode != http.StatusBadGateway {
			t.Fatalf("second GET status = %d, want 502 (ECONNREFUSED destination)", resp2.StatusCode)
		}

		if got := forwardRefusedFailureCount(sb, host, "a"); got == 0 {
			t.Error("after ReloadConnectionRefused(true): upstream 'a' GlobalFailure = 0; expected RecordFailure to have been called")
		}
	})
}
