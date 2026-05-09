package metrics_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/metrics"
)

// TestRegistryExpositionShape verifies the /metrics surface speaks the
// Prometheus text format and includes every metric name the rest of the
// binary references.
func TestRegistryExpositionShape(t *testing.T) {
	t.Parallel()

	reg := metrics.New()
	reg.ObserveDial("upstream-a", metrics.DialOutcomeSuccess, 25*time.Millisecond)
	reg.ObserveDial("upstream-a", metrics.DialOutcomeRefused, 5*time.Millisecond)
	reg.ObserveStatusFailure("upstream-a", "429")
	reg.ObserveRequestOutcome(metrics.OutcomeSuccess)
	reg.ObserveCascadeTrip()
	reg.ObserveProbePick()
	reg.ObservePersistenceWrite(metrics.ResultSuccess)
	reg.ObserveConfigReload(metrics.ResultError)
	reg.SetUpstreamPoolSize(7)
	reg.SetScoreboardSnapshot(3, map[string]int{"upstream-a": 2}, 1, []string{"upstream-a"})

	body := scrapeRegistry(t, reg)
	wantNames := []string{
		"tunnelsmith_dial_attempts_total",
		"tunnelsmith_dial_latency_seconds",
		"tunnelsmith_status_failures_total",
		"tunnelsmith_request_outcomes_total",
		"tunnelsmith_cascade_trips_total",
		"tunnelsmith_probe_picks_total",
		"tunnelsmith_persistence_writes_total",
		"tunnelsmith_config_reloads_total",
		"tunnelsmith_upstream_pool_size",
		"tunnelsmith_scoreboard_entries",
		"tunnelsmith_upstream_cooled_hosts",
		"tunnelsmith_cascade_active_hosts",
	}
	for _, name := range wantNames {
		if !strings.Contains(body, name) {
			t.Errorf("metric name %q missing from /metrics output", name)
		}
	}
}

// TestSetScoreboardSnapshotResetsCooledForUnseenUpstreams locks in the
// "every poolID gauge gets reset" behavior. Without it, an upstream that
// recovered would keep reporting its previous cooled-hosts count forever.
func TestSetScoreboardSnapshotResetsCooledForUnseenUpstreams(t *testing.T) {
	t.Parallel()

	reg := metrics.New()
	reg.SetScoreboardSnapshot(2, map[string]int{"a": 5, "b": 1}, 0, []string{"a", "b"})

	body := scrapeRegistry(t, reg)
	if !strings.Contains(body, `tunnelsmith_upstream_cooled_hosts{upstream_id="a"} 5`) {
		t.Errorf("first snapshot did not record a=5; body:\n%s", body)
	}

	// Second pass: a recovered (no entry in cooledByUpstream), b unchanged.
	reg.SetScoreboardSnapshot(2, map[string]int{"b": 1}, 0, []string{"a", "b"})
	body = scrapeRegistry(t, reg)
	if !strings.Contains(body, `tunnelsmith_upstream_cooled_hosts{upstream_id="a"} 0`) {
		t.Errorf("second snapshot did not reset a to 0; body:\n%s", body)
	}
	if !strings.Contains(body, `tunnelsmith_upstream_cooled_hosts{upstream_id="b"} 1`) {
		t.Errorf("second snapshot did not preserve b=1; body:\n%s", body)
	}
}

// TestServerServesMetricsAndHealthz confirms the metrics.Server wires the
// /metrics handler and a small /healthz endpoint that serves 200 OK.
func TestServerServesMetricsAndHealthz(t *testing.T) {
	t.Parallel()

	reg := metrics.New()
	reg.ObserveDial("u", metrics.DialOutcomeSuccess, 1*time.Millisecond)

	srv := metrics.NewServer("127.0.0.1:0", reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx) }()
	<-srv.Ready()
	addr := srv.Addr()
	if addr == nil {
		t.Fatal("metrics.Server.Addr returned nil after Ready closed")
	}

	body := httpGet(t, "http://"+addr.String()+"/metrics")
	if !strings.Contains(body, "tunnelsmith_dial_attempts_total") {
		t.Errorf("scrape missing dial counter; body:\n%s", body)
	}

	healthBody := httpGet(t, "http://"+addr.String()+"/healthz")
	if strings.TrimSpace(healthBody) != "ok" {
		t.Errorf("healthz body = %q, want \"ok\"", healthBody)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(shutdownCancel)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Errorf("metrics.Server.Shutdown: %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Errorf("metrics.Server.Serve returned non-nil error: %v", err)
	}
}

// scrapeRegistry stands up a one-shot httptest-style server backed by reg
// and returns the body of GET /metrics.
func scrapeRegistry(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = httpSrv.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		<-serveDone
	})
	return httpGet(t, "http://"+listener.Addr().String()+"/metrics")
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", url, err)
	}
	return string(body)
}
