// Package client is the Phase 11 Go SDK app maintainers drop into their
// services to participate in Tunnelsmith's cooperative outcome reporting.
//
// The integration is three lines:
//
//	c, err := client.New(client.Options{
//	    ProxyURL:   "http://tunnelsmith:8080",
//	    ControlURL: "http://tunnelsmith:9092",
//	})
//	resp, err := c.Get("https://example.com/api/things")
//	_ = client.Report(resp, "ok")
//
// What this gives you:
//
//   - All HTTP and HTTPS traffic is routed through Tunnelsmith.
//   - The chosen upstream id is captured automatically (from the
//     X-Tunnelsmith-Upstream header on plain-HTTP responses, and from
//     the same header on the CONNECT 200 response for HTTPS).
//   - For HTTPS requests, status codes 429, 403, and 451 auto-report
//     to the control endpoint as rate_limited / forbidden / legal_block.
//     Apps that want richer signal call Report(resp, outcome) with one
//     of the outcomes documented in docs/cooperative-reporting.md.
//
// What this gives up:
//
//   - Connection keep-alive defaults to off because OnProxyConnectResponse
//     only fires on connection establishment; reusing connections means
//     later requests on the same conn cannot be attributed to a specific
//     upstream. KeepAlive can be enabled in Options for users who accept
//     that Report() may return ErrNoUpstream for some requests.
//
// Reports are best-effort. Network errors talking to the control
// endpoint are logged via the optional Logger but never bubble up to
// the user-facing request.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Options configures New. ProxyURL and ControlURL are required.
type Options struct {
	// ProxyURL is the Tunnelsmith proxy listener (default :8080).
	// Must include scheme; "http://tunnelsmith:8080" is canonical.
	ProxyURL string

	// ControlURL is the Tunnelsmith control endpoint (default :9092).
	// Must include scheme; the SDK appends /v1/report.
	ControlURL string

	// KeepAlive controls whether the underlying *http.Transport pools
	// CONNECT-tunneled connections. Default false (DisableKeepAlives=true)
	// because OnProxyConnectResponse only fires on connection
	// establishment; reused connections mean only the first request per
	// conn has its upstream id captured. Set to true if you accept that
	// Report() will return ErrNoUpstream for requests that landed on a
	// reused conn.
	KeepAlive bool

	// Timeout sets the total deadline for individual report POSTs to the
	// control endpoint. Zero defaults to 2s. Negative disables.
	Timeout time.Duration

	// Logger receives report-failure messages. nil disables logging.
	// Reports are best-effort; the user-facing request never fails
	// because a report could not be delivered.
	Logger *slog.Logger
}

// ErrNoUpstream is returned by Report when the response has no recorded
// upstream id. Common causes: the response was made by an http.Client
// other than the one returned by New; the request landed on a reused
// keep-alive connection (set KeepAlive=false to avoid this).
var ErrNoUpstream = errors.New("client: response has no recorded upstream id")

// reportingTransport wraps an *http.Transport, captures the chosen
// upstream id for each request, and emits auto-reports for HTTPS status
// codes in {429, 403, 451}. It is the SDK's RoundTripper.
type reportingTransport struct {
	base            *http.Transport
	controlURL      string
	timeout         time.Duration
	logger          *slog.Logger
	reporter        *http.Client  // dedicated client for control-plane POSTs
	autoReportSlots chan struct{} // bounds concurrent async report goroutines
}

// upstreamBox is the per-request slot OnProxyConnectResponse writes
// through and Report reads from. A pointer to it lives on the request
// context; resp.Request preserves that context so Report can recover it
// from the response alone.
type upstreamBox struct {
	upstream        atomic.Value // string; "" until populated
	controlURL      string
	timeout         time.Duration
	logger          *slog.Logger
	reporter        *http.Client
	autoReportSlots chan struct{}
}

func (b *upstreamBox) get() string {
	v, _ := b.upstream.Load().(string)
	return v
}

type ctxKeyUpstream struct{}

// New returns an *http.Client configured for cooperative reporting.
// Drop the returned client into your existing call sites and call
// Report(resp, outcome) on the response when you have a semantic
// outcome to share with the scoreboard.
func New(opts Options) (*http.Client, error) {
	if opts.ProxyURL == "" {
		return nil, errors.New("client: Options.ProxyURL is required")
	}
	if opts.ControlURL == "" {
		return nil, errors.New("client: Options.ControlURL is required")
	}
	pu, err := url.Parse(opts.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("client: parse ProxyURL: %w", err)
	}
	if pu.Scheme != "http" && pu.Scheme != "https" {
		return nil, fmt.Errorf("client: ProxyURL scheme must be http or https, got %q", pu.Scheme)
	}
	cu, err := url.Parse(opts.ControlURL)
	if err != nil {
		return nil, fmt.Errorf("client: parse ControlURL: %w", err)
	}
	if cu.Scheme != "http" && cu.Scheme != "https" {
		return nil, fmt.Errorf("client: ControlURL scheme must be http or https, got %q", cu.Scheme)
	}
	if cu.Host == "" {
		return nil, errors.New("client: ControlURL must include host")
	}
	if cu.Path != "" && cu.Path != "/" {
		return nil, fmt.Errorf("client: ControlURL path must be empty or '/', got %q", cu.Path)
	}

	timeout := opts.Timeout
	if timeout < 0 {
		timeout = 0
	} else if timeout == 0 {
		timeout = 2 * time.Second
	}

	base := &http.Transport{
		Proxy:             http.ProxyURL(pu),
		DisableKeepAlives: !opts.KeepAlive,
		// Capture the upstream id from the CONNECT 200 response; the
		// proxy injects X-Tunnelsmith-Upstream there for HTTPS.
		OnProxyConnectResponse: func(ctx context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
			box, ok := ctx.Value(ctxKeyUpstream{}).(*upstreamBox)
			if !ok || box == nil {
				return nil
			}
			if up := res.Header.Get("X-Tunnelsmith-Upstream"); up != "" {
				box.upstream.Store(up)
			}
			return nil
		},
	}

	rt := &reportingTransport{
		base:       base,
		controlURL: strings.TrimRight(opts.ControlURL, "/"),
		timeout:    timeout,
		logger:     opts.Logger,
		// Reuse a single client (with its own short-lived transport)
		// for control-plane POSTs so report bursts share a connection.
		// The control endpoint is local to tunnelsmith and never goes
		// through the proxy itself. CheckRedirect refuses every redirect
		// because net/http silently downgrades POST to GET on 301/302/303,
		// which would let a misconfigured front-end (or trailing-slash
		// redirect) drop reports without surfacing the misconfiguration.
		reporter: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:               func(*http.Request) (*url.URL, error) { return nil, nil },
				MaxIdleConnsPerHost: 4,
			},
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("client: control endpoint redirected to %s; reports must hit /v1/report directly", req.URL)
			},
		},
		autoReportSlots: make(chan struct{}, 64),
	}

	return &http.Client{Transport: rt}, nil
}

// RoundTrip is the SDK's request hook. It:
//  1. Stashes a per-request upstreamBox in the context so OnProxyConnectResponse
//     can write the captured upstream id through it.
//  2. Delegates to the base transport.
//  3. Reads X-Tunnelsmith-Upstream from the response (covers the plain-HTTP path).
//  4. Auto-reports HTTPS 429 / 403 / 451 to the control endpoint.
func (r *reportingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	box := &upstreamBox{
		controlURL:      r.controlURL,
		timeout:         r.timeout,
		logger:          r.logger,
		reporter:        r.reporter,
		autoReportSlots: r.autoReportSlots,
	}
	ctx := context.WithValue(req.Context(), ctxKeyUpstream{}, box)
	req2 := req.WithContext(ctx)

	resp, err := r.base.RoundTrip(req2)
	if resp == nil {
		return resp, err
	}

	if up := resp.Header.Get("X-Tunnelsmith-Upstream"); up != "" {
		box.upstream.Store(up)
	}

	if outcome := autoOutcomeFor(req2, resp.StatusCode); outcome != "" {
		host := hostForReport(req2)
		upID := box.get()
		if upID != "" {
			httpStatus := resp.StatusCode
			postReport(box, host, upID, outcome, &httpStatus)
		}
	}

	return resp, err
}

// Report submits an outcome to the control endpoint for resp. Returns
// ErrNoUpstream if the response has no recorded upstream id (most often
// because resp was made by some other client, or because KeepAlive=true
// is in use and this request landed on a reused conn).
//
// Outcome must be one of the values documented in
// docs/cooperative-reporting.md. The SDK does not validate the outcome
// string locally; the control endpoint rejects unknown values with 400
// so a typo is visible in the next response.
func Report(resp *http.Response, outcome string) error {
	if resp == nil || resp.Request == nil {
		return ErrNoUpstream
	}
	box, ok := resp.Request.Context().Value(ctxKeyUpstream{}).(*upstreamBox)
	if !ok || box == nil {
		return ErrNoUpstream
	}
	upID := box.get()
	if upID == "" {
		return ErrNoUpstream
	}
	host := hostForReport(resp.Request)
	httpStatus := resp.StatusCode
	return postReportSync(box, host, upID, outcome, &httpStatus)
}

// autoOutcomeForStatus maps an HTTP status to its auto-report outcome,
// or returns the empty string when no auto-report should fire. We
// auto-report only the three status codes that map cleanly to known
// outcomes; 5xx is intentionally excluded because it conflates
// destination bugs with proxy-side failures.
func autoOutcomeForStatus(status int) string {
	switch status {
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusUnavailableForLegalReasons:
		return "legal_block"
	}
	return ""
}

func autoOutcomeFor(req *http.Request, status int) string {
	if req == nil || req.URL == nil || !strings.EqualFold(req.URL.Scheme, "https") {
		return ""
	}
	return autoOutcomeForStatus(status)
}

// hostForReport returns the host key the report should reference. For
// HTTPS it returns host:port (defaulting to :443 when absent) to match
// the CONNECT key used by the proxy path. For plain HTTP it returns just
// the hostname to match the proxy's forward-path keying.
func hostForReport(req *http.Request) string {
	if req == nil {
		return ""
	}
	if req.URL != nil && req.URL.Host != "" {
		return normalizeHostForScheme(req.URL.Hostname(), req.URL.Port(), req.URL.Scheme)
	}
	if req.Host != "" {
		host, port := splitHostAndPort(req.Host)
		scheme := ""
		if req.URL != nil {
			scheme = req.URL.Scheme
		}
		return normalizeHostForScheme(host, port, scheme)
	}
	return ""
}

func normalizeHostForScheme(host, port, scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		if port == "" {
			port = "443"
		}
		return net.JoinHostPort(host, port)
	case "http":
		return host
	default:
		if port == "" {
			return host
		}
		return net.JoinHostPort(host, port)
	}
}

func splitHostAndPort(raw string) (string, string) {
	if splitHost, splitPort, err := net.SplitHostPort(raw); err == nil {
		return splitHost, splitPort
	}
	// Fallback path for host values without an explicit port, or inputs
	// where SplitHostPort fails to split. We intentionally leverage
	// url.URL's Hostname/Port helpers on a host-only value.
	u := &url.URL{Host: raw}
	return u.Hostname(), u.Port()
}

// reportPayload mirrors internal/control/handlers.go's reportRequest.
// We keep the shape inline (no shared package) so the SDK has zero
// internal imports and can be vendored standalone.
type reportPayload struct {
	Host       string `json:"host"`
	Upstream   string `json:"upstream"`
	Outcome    string `json:"outcome"`
	HTTPStatus *int   `json:"http_status,omitempty"`
}

// postReport fires a report asynchronously so the user-facing request
// returns without waiting on the control plane. The SDK never surfaces
// a report failure to the caller; failures are logged through Options.Logger
// when set.
func postReport(b *upstreamBox, host, upstream, outcome string, httpStatus *int) {
	select {
	case b.autoReportSlots <- struct{}{}:
	default:
		if b.logger != nil {
			b.logger.Warn("client: dropping auto-report due to local backpressure",
				"host", host, "upstream", upstream, "outcome", outcome)
		}
		return
	}
	go func() {
		defer func() { <-b.autoReportSlots }()
		if err := postReportSync(b, host, upstream, outcome, httpStatus); err != nil && b.logger != nil {
			b.logger.Warn("client: report failed",
				"host", host, "upstream", upstream, "outcome", outcome, "err", err)
		}
	}()
}

// postReportSync POSTs the report and waits for the response. Used by
// Report (the synchronous public API) so the caller can observe the
// error and by postReport (wrapped in a goroutine) for auto-reports.
func postReportSync(b *upstreamBox, host, upstream, outcome string, httpStatus *int) error {
	payload, err := json.Marshal(reportPayload{
		Host:       host,
		Upstream:   upstream,
		Outcome:    outcome,
		HTTPStatus: httpStatus,
	})
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}

	ctx := context.Background()
	cancel := func() {}
	if b.timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), b.timeout)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.controlURL+"/v1/report", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.reporter.Do(req)
	if err != nil {
		return fmt.Errorf("post report: %w", err)
	}
	// Drain and close so the reporter can pool the conn.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("control endpoint returned %s", resp.Status)
	}
	return nil
}
