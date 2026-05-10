package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/client"
)

// fakeProxy stands in for tunnelsmith's plain-HTTP forward proxy in unit
// tests. It RoundTrips the absolute-form request URL through a vanilla
// http.DefaultClient, then injects X-Tunnelsmith-Upstream and copies the
// response back. Tests pass the upstream id they want the SDK to capture.
func fakeProxy(t *testing.T, upstreamID string) (proxyURL *url.URL, srv *httptest.Server) {
	t.Helper()
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// In forward-proxy mode, r.URL is absolute and r.RequestURI is
		// the same. Build a clean outbound request.
		outURL := *r.URL
		req, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for k, v := range r.Header {
			req.Header[k] = v
		}
		req.Header.Del("Proxy-Connection")
		// Use the global default client; the destinations are local
		// httptest servers so latency is negligible.
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, v := range resp.Header {
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}
		w.Header().Set("X-Tunnelsmith-Upstream", upstreamID)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	pu, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake proxy URL: %v", err)
	}
	return pu, srv
}

// recordingControl captures /v1/report POSTs for assertions.
type recordingControl struct {
	mu       sync.Mutex
	received []recordedReport
	status   int // status to return (default 204)
}

type recordedReport struct {
	Host       string `json:"host"`
	Upstream   string `json:"upstream"`
	Outcome    string `json:"outcome"`
	HTTPStatus *int   `json:"http_status,omitempty"`
}

func (rc *recordingControl) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/report" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var rep recordedReport
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rc.mu.Lock()
		rc.received = append(rc.received, rep)
		s := rc.status
		rc.mu.Unlock()
		if s == 0 {
			s = http.StatusNoContent
		}
		w.WriteHeader(s)
	})
}

func (rc *recordingControl) snapshot() []recordedReport {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := make([]recordedReport, len(rc.received))
	copy(out, rc.received)
	return out
}

func newControl(t *testing.T) (*recordingControl, *httptest.Server) {
	t.Helper()
	rc := &recordingControl{}
	srv := httptest.NewServer(rc.handler(t))
	t.Cleanup(srv.Close)
	return rc, srv
}

func waitForReports(t *testing.T, rc *recordingControl, want int) []recordedReport {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := rc.snapshot()
		if len(got) >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return rc.snapshot()
}

// mustNewClient builds a client and fails the test immediately if
// initialization errors, so a misconfigured Options surfaces with the
// real error instead of panicking on a later c.Get / c.Do.
func mustNewClient(t *testing.T, opts client.Options) *http.Client {
	t.Helper()
	c, err := client.New(opts)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts client.Options
	}{
		{"missing proxy", client.Options{ControlURL: "http://x"}},
		{"missing control", client.Options{ProxyURL: "http://x"}},
		{"bad proxy scheme", client.Options{ProxyURL: "ftp://x", ControlURL: "http://y"}},
		{"bad control scheme", client.Options{ProxyURL: "http://x", ControlURL: "ftp://y"}},
		{"control missing host", client.Options{ProxyURL: "http://x", ControlURL: "http:///"}},
		{"control non-root path", client.Options{ProxyURL: "http://x", ControlURL: "http://y/base"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := client.New(tc.opts); err == nil {
				t.Fatalf("expected error for %+v", tc.opts)
			}
		})
	}
}

func TestPlainHTTPCapturesUpstreamFromResponse(t *testing.T) {
	t.Parallel()
	// Destination.
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	proxyURL, _ := fakeProxy(t, "mullvad-se-got")
	rc, controlSrv := newControl(t)

	c, err := client.New(client.Options{
		ProxyURL:   proxyURL.String(),
		ControlURL: controlSrv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := c.Get(dest.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("X-Tunnelsmith-Upstream"); got != "mullvad-se-got" {
		t.Errorf("response header = %q, want mullvad-se-got", got)
	}

	// Report should reach the control endpoint.
	if err := client.Report(resp, "ok"); err != nil {
		t.Fatalf("Report: %v", err)
	}
	got := waitForReports(t, rc, 1)
	if len(got) != 1 {
		t.Fatalf("control received %d reports, want 1", len(got))
	}
	if got[0].Upstream != "mullvad-se-got" || got[0].Outcome != "ok" {
		t.Errorf("report = %+v", got[0])
	}
	if got[0].Host == "" {
		t.Errorf("host should be populated, got empty")
	}
}

func TestAutoReportFiresOn429(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(dest.Close)

	proxyURL, _ := fakeProxy(t, "u1")
	rc, controlSrv := newControl(t)

	c, err := client.New(client.Options{
		ProxyURL:   proxyURL.String(),
		ControlURL: controlSrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(dest.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	got := waitForReports(t, rc, 1)
	if len(got) != 1 {
		t.Fatalf("auto-report missing; received %d", len(got))
	}
	if got[0].Outcome != "rate_limited" || got[0].Upstream != "u1" {
		t.Errorf("auto-report = %+v", got[0])
	}
	if got[0].HTTPStatus == nil || *got[0].HTTPStatus != 429 {
		t.Errorf("http_status = %v, want 429", got[0].HTTPStatus)
	}
}

func TestAutoReportFiresOn403And451(t *testing.T) {
	t.Parallel()
	cases := map[int]string{
		http.StatusForbidden:                  "forbidden",
		http.StatusUnavailableForLegalReasons: "legal_block",
	}
	for status, wantOutcome := range cases {
		status, wantOutcome := status, wantOutcome
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(dest.Close)
			proxyURL, _ := fakeProxy(t, "u")
			rc, controlSrv := newControl(t)

			c := mustNewClient(t, client.Options{
				ProxyURL:   proxyURL.String(),
				ControlURL: controlSrv.URL,
			})
			resp, err := c.Get(dest.URL)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			got := waitForReports(t, rc, 1)
			if len(got) != 1 || got[0].Outcome != wantOutcome {
				t.Errorf("got %+v, want one report with outcome=%s", got, wantOutcome)
			}
		})
	}
}

func TestNoAutoReportOn2xxOr5xx(t *testing.T) {
	t.Parallel()
	for _, status := range []int{200, 204, 301, 404, 500, 502, 503} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(dest.Close)
			proxyURL, _ := fakeProxy(t, "u")
			rc, controlSrv := newControl(t)

			c := mustNewClient(t, client.Options{
				ProxyURL:   proxyURL.String(),
				ControlURL: controlSrv.URL,
			})
			resp, err := c.Get(dest.URL)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			// Poll a bounded window. A misbehaving auto-report fires
			// asynchronously; checking once at a fixed deadline can
			// miss a fast-firing report that has already returned.
			// Polling catches a report whenever it appears within the
			// window and avoids a single brittle Sleep.
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				if got := rc.snapshot(); len(got) != 0 {
					t.Fatalf("unexpected auto-report on %d: %+v", status, got)
				}
				time.Sleep(15 * time.Millisecond)
			}
		})
	}
}

func TestReportOnAlienResponseReturnsErrNoUpstream(t *testing.T) {
	t.Parallel()
	// Build a response that did not go through New(). It has no
	// upstreamBox in its context.
	req, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Request: req, Body: http.NoBody}

	if err := client.Report(resp, "ok"); !errors.Is(err, client.ErrNoUpstream) {
		t.Fatalf("Report on alien response: %v, want ErrNoUpstream", err)
	}
}

func TestReportOnNilResponseReturnsErrNoUpstream(t *testing.T) {
	t.Parallel()
	if err := client.Report(nil, "ok"); !errors.Is(err, client.ErrNoUpstream) {
		t.Fatalf("Report(nil): %v, want ErrNoUpstream", err)
	}
}

func TestReportOnSDKResponseWithoutHeaderReturnsErrNoUpstream(t *testing.T) {
	t.Parallel()
	// Proxy that does NOT inject X-Tunnelsmith-Upstream.
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		// No X-Tunnelsmith-Upstream header.
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxySrv.Close)

	rc, controlSrv := newControl(t)
	c := mustNewClient(t, client.Options{
		ProxyURL:   proxySrv.URL,
		ControlURL: controlSrv.URL,
	})
	resp, err := c.Get(dest.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := client.Report(resp, "ok"); !errors.Is(err, client.ErrNoUpstream) {
		t.Fatalf("Report: %v, want ErrNoUpstream", err)
	}
	if got := rc.snapshot(); len(got) != 0 {
		t.Errorf("control received reports it should not have: %+v", got)
	}
}

// TestReportFailureDoesNotPropagateOnAutoReport ensures auto-report
// goroutine errors stay local; user-facing requests do not surface them.
func TestReportFailureDoesNotPropagateOnAutoReport(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(dest.Close)
	proxyURL, _ := fakeProxy(t, "u")
	// Control endpoint returns 500 for every report.
	rc, controlSrv := newControl(t)
	rc.mu.Lock()
	rc.status = http.StatusInternalServerError
	rc.mu.Unlock()

	c := mustNewClient(t, client.Options{
		ProxyURL:   proxyURL.String(),
		ControlURL: controlSrv.URL,
		Timeout:    500 * time.Millisecond,
	})
	resp, err := c.Get(dest.URL)
	if err != nil {
		t.Fatalf("user-facing Get must not fail when control endpoint errors: %v", err)
	}
	_ = resp.Body.Close()
}

func TestReportRespectsTimeout(t *testing.T) {
	t.Parallel()
	// Control endpoint that holds the request open longer than the
	// SDK's report timeout so the timeout path is exercised. Bounded
	// so httptest.Server.Close can drain the conn during cleanup.
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(hung.Close)
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)
	proxyURL, _ := fakeProxy(t, "u1")

	c := mustNewClient(t, client.Options{
		ProxyURL:   proxyURL.String(),
		ControlURL: hung.URL,
		Timeout:    150 * time.Millisecond,
	})
	resp, err := c.Get(dest.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	start := time.Now()
	err = client.Report(resp, "ok")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Report should have failed against a hung endpoint")
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("Report did not respect timeout (elapsed=%v)", elapsed)
	}
}

func TestNegativeTimeoutDisablesReportDeadline(t *testing.T) {
	t.Parallel()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(control.Close)
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)
	proxyURL, _ := fakeProxy(t, "u1")

	c := mustNewClient(t, client.Options{
		ProxyURL:   proxyURL.String(),
		ControlURL: control.URL,
		Timeout:    -1 * time.Second,
	})
	resp, err := c.Get(dest.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	start := time.Now()
	if err := client.Report(resp, "ok"); err != nil {
		t.Fatalf("Report with disabled timeout failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("Report returned too early with disabled timeout: elapsed=%v", elapsed)
	}
}

// TestConcurrentReportsDoNotRace is a smoke test under -race for the
// per-request upstream box. Each request gets its own box; any
// inadvertent shared state would surface as a race or as cross-bleed
// of upstream ids between requests.
func TestConcurrentReportsDoNotRace(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dest.Close)

	// Proxy that picks a different upstream id per request, so a race
	// would surface as wrong attributions in the recorded reports.
	var counter atomic.Int64
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		id := "u" + strings.Repeat("x", int(counter.Add(1))%5)
		w.Header().Set("X-Tunnelsmith-Upstream", id)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxySrv.Close)
	rc, controlSrv := newControl(t)

	c := mustNewClient(t, client.Options{
		ProxyURL:   proxySrv.URL,
		ControlURL: controlSrv.URL,
	})

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, dest.URL, nil)
			resp, err := c.Do(req)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
			_ = client.Report(resp, "ok")
		}()
	}
	wg.Wait()
	got := waitForReports(t, rc, N)
	if len(got) != N {
		t.Fatalf("control received %d reports, want %d", len(got), N)
	}
	for _, r := range got {
		if !strings.HasPrefix(r.Upstream, "u") {
			t.Errorf("upstream attribution corrupted: %+v", r)
		}
	}
}
