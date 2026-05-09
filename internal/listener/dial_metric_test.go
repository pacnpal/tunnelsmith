package listener_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/listener"
)

// TestForwardPathDialMetricRecordsStatusFailureAttempts locks in the fix
// for the Copilot review on PR #15: a 429 / 403 / 451 response burns a
// retry, so dial_attempts_total should record one increment per attempt
// with outcome=success (the connection succeeded; the body is what got
// flagged). Before the fix the status-failure branch skipped
// observeForwardDial entirely and the dial counter undercounted.
func TestForwardPathDialMetricRecordsStatusFailureAttempts(t *testing.T) {
	t.Parallel()

	hits := 0
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		// First two requests get a 429; third 200s. The forward path
		// retries through the same upstream because the test only
		// configures one direct upstream, so each retry should produce
		// a dial-success metric increment.
		if hits < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dest.Close)

	sink := &recordingMetricsSink{}
	srv, proxyURL := startHTTPListenerWithMetrics(t, sink)

	client := proxyClient(t, proxyURL)
	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("forward GET: %v", err)
	}
	_ = resp.Body.Close()

	// The test configures a default detector (429 IS a failure rule),
	// so the listener cycles through the only upstream until the retry
	// budget is exhausted. With a single upstream and the default cap
	// of 5, every attempt hits the same destination. We assert that
	// every attempt produced a dial-success metric.
	successDials := sink.dialOutcomeCount("success")
	statusFailures := sink.statusFailureCount()
	if successDials < statusFailures {
		t.Errorf("dial successes (%d) < status failures (%d); a status-failure attempt did not record a dial metric", successDials, statusFailures)
	}
	if statusFailures == 0 {
		t.Fatalf("no status_failures observed; test setup is wrong (hits=%d)", hits)
	}
	_ = srv
}

func startHTTPListenerWithMetrics(t *testing.T, sink listener.MetricsSink) (*listener.HTTPServer, *url.URL) {
	t.Helper()
	srv, err := listener.NewHTTP("127.0.0.1:0", directScoreboard(t), defaultDetector(), 5, quietLogger(),
		listener.WithHTTPMetrics(sink),
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
	if srv.Addr() == nil {
		t.Fatal("http listener bound but Addr() is nil")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Logf("http shutdown: %v", err)
		}
		<-serveErr
	})
	u := &url.URL{Scheme: "http", Host: srv.Addr().String()}
	return srv, u
}

// recordingMetricsSink captures every metric call so tests can assert
// on counts without standing up a Prometheus registry.
type recordingMetricsSink struct {
	mu              sync.Mutex
	dialOutcomes    map[string]int
	statusFailures  int
	requestOutcomes map[string]int
}

func (r *recordingMetricsSink) ObserveDial(_ string, outcome string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dialOutcomes == nil {
		r.dialOutcomes = map[string]int{}
	}
	r.dialOutcomes[outcome]++
}

func (r *recordingMetricsSink) ObserveStatusFailure(_ string, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusFailures++
}

func (r *recordingMetricsSink) ObserveRequestOutcome(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requestOutcomes == nil {
		r.requestOutcomes = map[string]int{}
	}
	r.requestOutcomes[outcome]++
}

func (r *recordingMetricsSink) dialOutcomeCount(outcome string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dialOutcomes[outcome]
}

func (r *recordingMetricsSink) statusFailureCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusFailures
}

// keep imports referenced if subsets of this file get commented out.
var _ = io.Discard
var _ config.UpstreamConfig
var _ failure.Kind
