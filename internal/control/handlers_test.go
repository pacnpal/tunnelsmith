package control

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/metrics"
)

// fakeBackend implements Backend for handler unit tests without spinning
// up a real scoreboard. Records every Record* call so assertions can
// verify the handler dispatched correctly.
type fakeBackend struct {
	mu        sync.Mutex
	poolIDs   []string
	successes []recordedSuccess
	failures  []recordedFailure
}

type recordedSuccess struct {
	host, upstreamID string
	latency          time.Duration
}
type recordedFailure struct {
	host, upstreamID string
	kind             failure.Kind
	cooldown         *time.Duration
}

func (f *fakeBackend) PoolIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.poolIDs))
	copy(out, f.poolIDs)
	return out
}

func (f *fakeBackend) RecordSuccess(host, upstreamID string, latency time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successes = append(f.successes, recordedSuccess{host, upstreamID, latency})
}

func (f *fakeBackend) RecordFailure(host, upstreamID string, kind failure.Kind, cooldown *time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, recordedFailure{host, upstreamID, kind, cooldown})
}

// fakeMetrics records reports/rejections for assertion.
type fakeMetrics struct {
	mu       sync.Mutex
	received []string // outcome|upstream
	rejected []string // reason
}

func (f *fakeMetrics) ObserveReportReceived(outcome, upstreamID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, outcome+"|"+upstreamID)
}

func (f *fakeMetrics) ObserveReportRejected(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, reason)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T, backend Backend, m MetricsSink) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mountHandlers(mux, backend, m, quietLogger())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, srv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func TestReportOKRecordsSuccess(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"direct", "mullvad-se-got"}}
	m := &fakeMetrics{}
	srv := newTestServer(t, backend, m)

	resp := postJSON(t, srv, "/v1/report", `{
		"host": "example.com:443",
		"upstream": "mullvad-se-got",
		"outcome": "ok",
		"http_status": 200
	}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := len(backend.successes); got != 1 {
		t.Fatalf("RecordSuccess calls = %d, want 1", got)
	}
	if backend.successes[0].host != "example.com:443" || backend.successes[0].upstreamID != "mullvad-se-got" {
		t.Errorf("RecordSuccess called with %+v", backend.successes[0])
	}
	if len(backend.failures) != 0 {
		t.Errorf("unexpected RecordFailure calls: %+v", backend.failures)
	}
	if len(m.received) != 1 || m.received[0] != "ok|mullvad-se-got" {
		t.Errorf("received metrics = %v", m.received)
	}
}

func TestReportEachOutcomeMapsToExpectedKind(t *testing.T) {
	t.Parallel()
	cases := map[string]failure.Kind{
		"rate_limited": failure.KindRateLimit,
		"forbidden":    failure.KindForbidden,
		"legal_block":  failure.KindLegalBlock,
		"geo_block":    failure.KindBodyMatch,
		"timeout":      failure.KindTimeout,
		"refused":      failure.KindRefused,
	}
	for outcome, want := range cases {
		outcome, want := outcome, want
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			backend := &fakeBackend{poolIDs: []string{"u1"}}
			srv := newTestServer(t, backend, nil)

			resp := postJSON(t, srv, "/v1/report", `{
				"host": "x.example:443",
				"upstream": "u1",
				"outcome": "`+outcome+`"
			}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", resp.StatusCode)
			}
			if got := len(backend.failures); got != 1 {
				t.Fatalf("RecordFailure calls = %d, want 1", got)
			}
			if backend.failures[0].kind != want {
				t.Errorf("kind = %s, want %s", backend.failures[0].kind, want)
			}
			if backend.failures[0].cooldown != nil {
				t.Errorf("cooldownOverride should be nil, got %v", *backend.failures[0].cooldown)
			}
		})
	}
}

func TestReportWrongMethodReturns405(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeBackend{}, nil)
	resp, err := http.Get(srv.URL + "/v1/report")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow header = %q, want POST", got)
	}
}

func TestReportBadJSONReturns400(t *testing.T) {
	t.Parallel()
	m := &fakeMetrics{}
	srv := newTestServer(t, &fakeBackend{}, m)
	resp := postJSON(t, srv, "/v1/report", `{not-json`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if len(m.rejected) != 1 || m.rejected[0] != metrics.ReportRejectBadJSON {
		t.Errorf("rejected metrics = %v", m.rejected)
	}
}

func TestReportTooLargeReturns413(t *testing.T) {
	t.Parallel()
	m := &fakeMetrics{}
	srv := newTestServer(t, &fakeBackend{poolIDs: []string{"u1"}}, m)
	// Build an oversized body. The handler caps reads at 4 KiB; pad to
	// 5 KiB with whitespace inside an otherwise-valid JSON envelope.
	pad := strings.Repeat(" ", 5*1024)
	body := `{"host":"a","upstream":"u1","outcome":"ok"` + pad + `}`
	resp := postJSON(t, srv, "/v1/report", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if len(m.rejected) != 1 || m.rejected[0] != metrics.ReportRejectBadJSON {
		t.Errorf("rejected metrics = %v", m.rejected)
	}
}

func TestReportMissingFieldsReturns400(t *testing.T) {
	t.Parallel()
	m := &fakeMetrics{}
	srv := newTestServer(t, &fakeBackend{poolIDs: []string{"u1"}}, m)
	cases := []string{
		`{"upstream":"u1","outcome":"ok"}`,
		`{"host":"a","outcome":"ok"}`,
		`{"host":"a","upstream":"u1"}`,
	}
	for _, body := range cases {
		resp := postJSON(t, srv, "/v1/report", body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body=%q status = %d, want 400", body, resp.StatusCode)
		}
	}
	if len(m.rejected) != len(cases) {
		t.Fatalf("rejected count = %d, want %d", len(m.rejected), len(cases))
	}
	for _, r := range m.rejected {
		if r != metrics.ReportRejectMissingField {
			t.Errorf("reject reason = %q, want %q", r, metrics.ReportRejectMissingField)
		}
	}
}

func TestReportUnknownOutcomeReturns400(t *testing.T) {
	t.Parallel()
	m := &fakeMetrics{}
	srv := newTestServer(t, &fakeBackend{poolIDs: []string{"u1"}}, m)
	resp := postJSON(t, srv, "/v1/report", `{
		"host": "a",
		"upstream": "u1",
		"outcome": "tea_pot"
	}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// The error message lists the allowed vocabulary so the client sees
	// the contract inline.
	if !bytes.Contains(body, []byte("ok")) || !bytes.Contains(body, []byte("geo_block")) {
		t.Errorf("error body did not list allowed outcomes: %s", body)
	}
	if len(m.rejected) != 1 || m.rejected[0] != metrics.ReportRejectUnknownOutcome {
		t.Errorf("rejected metrics = %v", m.rejected)
	}
}

func TestReportUnknownUpstreamReturns404(t *testing.T) {
	t.Parallel()
	m := &fakeMetrics{}
	srv := newTestServer(t, &fakeBackend{poolIDs: []string{"u1"}}, m)
	resp := postJSON(t, srv, "/v1/report", `{
		"host": "a",
		"upstream": "ghost",
		"outcome": "ok"
	}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if len(m.rejected) != 1 || m.rejected[0] != metrics.ReportRejectUnknownUpstream {
		t.Errorf("rejected metrics = %v", m.rejected)
	}
}

func TestReportTrailingContentReturns400(t *testing.T) {
	t.Parallel()
	m := &fakeMetrics{}
	srv := newTestServer(t, &fakeBackend{poolIDs: []string{"u1"}}, m)
	resp := postJSON(t, srv, "/v1/report",
		`{"host":"a","upstream":"u1","outcome":"ok"}{"oops":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if len(m.rejected) != 1 || m.rejected[0] != metrics.ReportRejectBadJSON {
		t.Errorf("rejected metrics = %v", m.rejected)
	}
}

func TestReportRejectsUnknownJSONFields(t *testing.T) {
	t.Parallel()
	m := &fakeMetrics{}
	srv := newTestServer(t, &fakeBackend{poolIDs: []string{"u1"}}, m)
	resp := postJSON(t, srv, "/v1/report",
		`{"host":"a","upstream":"u1","outcome":"ok","extra":"not-allowed"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if len(m.rejected) != 1 || m.rejected[0] != metrics.ReportRejectBadJSON {
		t.Errorf("rejected metrics = %v", m.rejected)
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeBackend{}, nil)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// Defense-in-depth: every Kind the outcomeMap targets is one
// scoreboard.New accepts. If a future refactor renames a Kind, this
// catches it before runtime.
func TestOutcomeMapTargetsValidKinds(t *testing.T) {
	t.Parallel()
	for outcome, kind := range outcomeMap {
		if !kind.Valid() {
			t.Errorf("outcome %q maps to invalid Kind %q", outcome, kind)
		}
	}
}

// Server lifecycle smoke test: bind, accept, shut down.
func TestServerServeAndShutdown(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"u1"}}
	s := NewServer("127.0.0.1:0", backend, nil, quietLogger())

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(context.Background()) }()
	select {
	case <-s.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("control listener did not bind in time")
	}

	addr := s.Addr()
	if addr == nil {
		t.Fatal("Addr() returned nil after Ready closed")
	}

	resp, err := http.Get("http://" + addr.String() + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if err := <-serveErr; err != nil {
		t.Errorf("Serve returned: %v", err)
	}
}
