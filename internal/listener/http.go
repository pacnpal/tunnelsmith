// Package listener owns the HTTP and SOCKS5 entry points. Each listener
// receives a client connection, asks the scoreboard to dial through to the
// destination, and pumps bytes between the two. The scoreboard wraps the
// priority pool with per-(host, upstream) scoring; before Phase 4 the
// listeners drove the pool directly.
package listener

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// MetricsSink is the listener-side metrics surface. metrics.Registry
// implements it; pass nil to disable emission.
type MetricsSink interface {
	ObserveDial(upstreamID, outcome string, latency time.Duration)
	ObserveStatusFailure(upstreamID, code string)
	ObserveRequestOutcome(outcome string)
}

// HTTPServer accepts HTTP CONNECT and plain-HTTP forward-proxy traffic.
type HTTPServer struct {
	addr    string
	sb      *scoreboard.Scoreboard
	logger  *slog.Logger
	metrics MetricsSink

	// runtime is the slice of knobs SIGHUP hot-reload can swap in place.
	// Guarded by runtimeMu so the request path's reads stay consistent
	// with concurrent Reload calls.
	runtimeMu       sync.RWMutex
	detector        *failure.StatusDetector
	retryCap        int
	rules           *upstream.RuleSet
	bodyBufferBytes int

	server *http.Server

	// transports caches one *http.Transport per upstream id. Each
	// transport's DialContext pins to one upstream's Dial method, so HTTP
	// keep-alive can pool conns to the destination through that upstream
	// without the dial path losing its per-upstream identity. The map is
	// lazy: an entry is created the first time the listener picks that
	// upstream. Guarded by transportsMu.
	transportsMu sync.Mutex
	transports   map[string]*http.Transport

	// ready closes once Serve has finished binding the listener (or
	// finished failing to bind). Callers waiting on Addr() block on this
	// to avoid racing the bind. Channel receive after close forms a
	// happens-before edge, so reads of the listener field below are race
	// free without a mutex.
	ready    chan struct{}
	listener net.Listener
	bindErr  error

	tunnelsMu sync.Mutex
	tunnels   map[*tunnel]struct{}
}

// HTTPOption customizes an HTTPServer at construction.
type HTTPOption func(*HTTPServer)

// WithHTTPMetrics attaches a metrics sink. Pass nil to disable emission.
func WithHTTPMetrics(m MetricsSink) HTTPOption {
	return func(h *HTTPServer) {
		h.metrics = m
	}
}

// WithHTTPRules attaches the per-host RuleSet the listener consults
// for body-regex inspection. The same set lives behind the scoreboard
// for routing; the listener reference lets handleForward decide
// whether to buffer a response body without reaching across packages.
// Pass nil to disable body inspection.
func WithHTTPRules(rs *upstream.RuleSet) HTTPOption {
	return func(h *HTTPServer) {
		h.rules = rs
	}
}

// WithHTTPBodyBufferKB sets the per-response cap on bytes the
// inspector buffers for regex matching. Values <= 0 are treated as
// "disabled"; the listener will skip body inspection and stream every
// response straight through. The default of 32 KiB matches
// FailureConfig.BodyBufferKB's default.
func WithHTTPBodyBufferKB(kb int) HTTPOption {
	return func(h *HTTPServer) {
		if kb < 0 {
			kb = 0
		}
		h.bodyBufferBytes = kb * 1024
	}
}

// NewHTTP builds an HTTP listener that routes everything through sb. The
// scoreboard must be non-nil; passing nil returns a clear error so callers
// see the contract violation at construction time instead of a nil-deref
// on the first request. detector may be nil, in which case status-code
// detection is disabled and every response is treated as success. retryCap
// must be at least 1 and bounds the per-request attempts on the plain-HTTP
// forward path; the value mirrors failure.max_retries_per_request from
// config so dial retries and status retries share the same budget.
func NewHTTP(addr string, sb *scoreboard.Scoreboard, detector *failure.StatusDetector, retryCap int, logger *slog.Logger, opts ...HTTPOption) (*HTTPServer, error) {
	if sb == nil {
		return nil, errors.New("listener.NewHTTP: scoreboard is nil")
	}
	if retryCap < 1 {
		return nil, fmt.Errorf("listener.NewHTTP: retryCap must be >= 1, got %d", retryCap)
	}
	if logger == nil {
		logger = slog.Default()
	}
	h := &HTTPServer{
		addr:       addr,
		sb:         sb,
		detector:   detector,
		retryCap:   retryCap,
		logger:     logger.With("listener", "http"),
		ready:      make(chan struct{}),
		tunnels:    make(map[*tunnel]struct{}),
		transports: make(map[string]*http.Transport),
	}
	// Reload knobs default to the constructor arguments above; the
	// runtimeMu fields are populated implicitly via the struct literal.

	for _, opt := range opts {
		opt(h)
	}
	h.server = &http.Server{
		Addr:              addr,
		Handler:           http.HandlerFunc(h.handle),
		ReadHeaderTimeout: 30 * time.Second,
	}
	return h, nil
}

// currentDetector returns the live failure detector. Reads are guarded by
// runtimeMu so a concurrent Reload cannot tear the pointer.
func (h *HTTPServer) currentDetector() *failure.StatusDetector {
	h.runtimeMu.RLock()
	defer h.runtimeMu.RUnlock()
	return h.detector
}

// currentRetryCap returns the live retry cap.
func (h *HTTPServer) currentRetryCap() int {
	h.runtimeMu.RLock()
	defer h.runtimeMu.RUnlock()
	return h.retryCap
}

// currentRules returns the live per-host rule set. Reads are guarded
// by runtimeMu so a concurrent ReloadRules cannot tear the pointer.
func (h *HTTPServer) currentRules() *upstream.RuleSet {
	h.runtimeMu.RLock()
	defer h.runtimeMu.RUnlock()
	return h.rules
}

// currentBodyBufferBytes returns the live per-response body buffer
// cap in bytes. Zero means body inspection is disabled regardless of
// the rule set.
func (h *HTTPServer) currentBodyBufferBytes() int {
	h.runtimeMu.RLock()
	defer h.runtimeMu.RUnlock()
	return h.bodyBufferBytes
}

// Reload swaps the live detector and retry cap. Called from the SIGHUP
// hot-reload path. Invalid retryCap values (< 1) are ignored with a
// log line so a misconfigured reload does not zero out the cap.
//
// Per-upstream HTTP transports are kept across reloads so connection
// pools survive a config bump that does not change the upstream list;
// when an upstream goes away in the new pool, CloseTransportsExcept
// drops its transport explicitly.
func (h *HTTPServer) Reload(detector *failure.StatusDetector, retryCap int) {
	if retryCap < 1 {
		h.logger.Warn("http listener reload ignored (retryCap < 1)", "retry_cap", retryCap)
		return
	}
	h.runtimeMu.Lock()
	h.detector = detector
	h.retryCap = retryCap
	h.runtimeMu.Unlock()
	h.logger.Info("http listener reloaded", "retry_cap", retryCap)
}

// ReloadRules swaps the per-host rule set in place. Pass nil to
// clear all rules. The runtimeMu guard mirrors Reload's, so a request
// path mid-Pick sees one coherent rule set or the other.
func (h *HTTPServer) ReloadRules(rs *upstream.RuleSet) {
	h.runtimeMu.Lock()
	h.rules = rs
	h.runtimeMu.Unlock()
	h.logger.Info("http listener rules reloaded", "rule_count", rs.Len())
}

// ReloadBodyBufferKB swaps the per-response body buffer cap. Zero or
// negative values turn body inspection off entirely.
func (h *HTTPServer) ReloadBodyBufferKB(kb int) {
	if kb < 0 {
		kb = 0
	}
	h.runtimeMu.Lock()
	h.bodyBufferBytes = kb * 1024
	h.runtimeMu.Unlock()
	h.logger.Info("http listener body buffer reloaded", "body_buffer_kb", kb)
}

// CloseTransportsExcept closes idle conns and drops cached transports for
// every upstream whose id is not in keep. Called from the hot-reload path
// after the upstream pool changes so per-upstream conn pools cannot leak.
func (h *HTTPServer) CloseTransportsExcept(keep map[string]struct{}) {
	h.transportsMu.Lock()
	var removed []string
	for id, t := range h.transports {
		if _, ok := keep[id]; ok {
			continue
		}
		t.CloseIdleConnections()
		delete(h.transports, id)
		removed = append(removed, id)
	}
	h.transportsMu.Unlock()
	if len(removed) > 0 {
		h.logger.Info("http listener dropped transports for removed upstreams", "ids", removed)
	}
}

// transportFor returns the Transport pinned to up, building it on first use.
// Each Transport's DialContext is closed over the upstream's Dial method, so
// HTTP keep-alive pools conns to the destination through the same upstream.
// MaxIdleConnsPerHost is set to 4 (slightly above stdlib's default of 2)
// so a small burst of concurrent clients hitting the same destination
// through this upstream can keep a handful of conns warm; MaxIdleConns is
// capped at 100 (versus stdlib's default of unlimited) so the total idle
// pool across destinations cannot grow without bound.
func (h *HTTPServer) transportFor(up upstream.Upstream) *http.Transport {
	h.transportsMu.Lock()
	defer h.transportsMu.Unlock()
	if t, ok := h.transports[up.ID()]; ok {
		return t
	}
	pinned := up
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return pinned.Dial(ctx, network, addr)
		},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Forward proxies must not auto-add Accept-Encoding and silently
		// decompress: net/http would strip Content-Encoding and
		// Content-Length on the way back, breaking byte-for-byte
		// transparency. With DisableCompression true, the destination's
		// encoded body and headers reach the client untouched.
		DisableCompression: true,
	}
	h.transports[up.ID()] = t
	return t
}

// Ready returns a channel that closes once Serve has either bound the
// listener or failed to bind. Callers can wait on it before reading Addr().
func (h *HTTPServer) Ready() <-chan struct{} { return h.ready }

// Addr returns the resolved listening address. Returns nil before the
// listener has bound; wait on Ready() in tests that need the OS-picked
// port.
func (h *HTTPServer) Addr() net.Addr {
	select {
	case <-h.ready:
	default:
		return nil
	}
	if h.listener == nil {
		return nil
	}
	return h.listener.Addr()
}

// Serve binds the listener and blocks serving until Shutdown is called.
// The returned error is nil on a clean shutdown.
func (h *HTTPServer) Serve(ctx context.Context) error {
	l, err := net.Listen("tcp", h.addr)
	if err != nil {
		h.bindErr = fmt.Errorf("http listen %s: %w", h.addr, err)
		close(h.ready)
		return h.bindErr
	}
	h.listener = l
	close(h.ready)
	h.logger.Info("listening", "addr", l.Addr().String())
	if err := h.server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http serve: %w", err)
	}
	return nil
}

// Shutdown stops accepting new connections, waits for HTTP handlers to
// drain, then forces any CONNECT tunnels still in flight to close. The
// caller's context bounds the wait. Safe to call before Serve binds: it
// waits on Ready() first so internal field reads are race free.
func (h *HTTPServer) Shutdown(ctx context.Context) error {
	select {
	case <-h.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Stop accepting; let in-flight HTTP handlers drain on their own
	// terms. CONNECT handlers are hijacked, so http.Server.Shutdown does
	// not see them; we close them explicitly below.
	httpErr := h.server.Shutdown(ctx)

	// Close idle conns held by every per-upstream transport so the OS can
	// reap them promptly instead of waiting on IdleConnTimeout.
	h.closeAllIdleConns()

	// Drain hijacked tunnels.
	tunnels := h.snapshotTunnels()
	if len(tunnels) == 0 {
		return httpErr
	}
	done := make(chan struct{})
	go func() {
		for _, t := range tunnels {
			t.wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return httpErr
	case <-ctx.Done():
		// Force-close any remaining tunnels so the caller's wg unblocks.
		for _, t := range tunnels {
			t.forceClose()
		}
		<-done
		if httpErr == nil {
			return ctx.Err()
		}
		return httpErr
	}
}

func (h *HTTPServer) closeAllIdleConns() {
	h.transportsMu.Lock()
	transports := make([]*http.Transport, 0, len(h.transports))
	for _, t := range h.transports {
		transports = append(transports, t)
	}
	h.transportsMu.Unlock()
	for _, t := range transports {
		t.CloseIdleConnections()
	}
}

func (h *HTTPServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		h.handleConnect(w, r)
		return
	}
	h.handleForward(w, r)
}

func (h *HTTPServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	host := r.Host
	upConn, upID, err := h.sb.DialFor(r.Context(), "tcp", host)
	if err != nil {
		h.logger.Warn("connect dial failed", "host", host, "err", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		h.observeOutcome(connectOutcome(err))
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upConn.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, brw, err := hj.Hijack()
	if err != nil {
		_ = upConn.Close()
		h.logger.Warn("connect hijack failed", "host", host, "err", err)
		return
	}
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = clientConn.Close()
		_ = upConn.Close()
		h.logger.Warn("connect write 200 failed", "host", host, "err", err)
		return
	}
	if err := brw.Flush(); err != nil {
		_ = clientConn.Close()
		_ = upConn.Close()
		return
	}

	// net/http may have buffered bytes past the CONNECT headers in
	// brw.Reader: clients commonly pipeline a TLS ClientHello right after
	// CONNECT. Reading from clientConn directly would skip those bytes and
	// corrupt the tunnel. Wrap clientConn so the tunnel's read side pulls
	// the buffered bytes first before falling through to the wire.
	clientForTunnel := newHijackedConn(clientConn, brw.Reader)

	t := h.registerTunnel(clientForTunnel, upConn)
	defer h.unregisterTunnel(t)
	t.run()

	h.logger.Info("connect closed",
		"host", host,
		"upstream_id", upID,
		"latency_ms", time.Since(start).Milliseconds(),
	)
	h.observeOutcome("success")
}

// handleForward serves the plain-HTTP forward-proxy path. Each request runs
// through a Pick + RoundTrip + status-detect loop bounded by retryCap. Dial
// failures and status-code failures both consume the retry budget; the
// scoreboard records each failure under the right Kind so cooldowns and
// global counters stay in sync. On success the response carries
// X-Tunnelsmith-Upstream and X-Tunnelsmith-Retries; on cascade-failure 502
// the response carries X-Tunnelsmith-Cascade and X-Tunnelsmith-Retries.
func (h *HTTPServer) handleForward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() {
		http.Error(w, "absolute URL required for forward proxy", http.StatusBadRequest)
		return
	}
	// Restrict to http/https. Unsupported schemes (ftp://, gopher://, etc.)
	// would make every RoundTrip fail deterministically, burning the retry
	// budget and incorrectly tripping cascade for the host. Reject up front
	// so the scoreboard does not blame any upstream for a request the
	// listener cannot serve.
	if scheme := strings.ToLower(r.URL.Scheme); scheme != "http" && scheme != "https" {
		http.Error(w, "unsupported scheme: forward proxy supports http and https only", http.StatusBadRequest)
		return
	}
	start := time.Now()
	host := r.URL.Hostname()
	// IsAbs is true for malformed absolute-form URIs like "http:/path"
	// where the scheme is set but the URL host is missing. Even if a Host:
	// header is present, outbound RoundTrip still fails deterministically
	// because req.URL.Host is empty. Reject up front so the scoreboard
	// does not burn retries or trip cascade for a host the listener cannot
	// actually serve.
	if host == "" {
		http.Error(w, "request URL missing host", http.StatusBadRequest)
		return
	}

	// Buffer the request body so the retry loop can replay it on each
	// attempt. Cap the buffer so a client cannot exhaust the proxy's
	// memory by streaming an unbounded body through a forward request:
	// 8 MiB is comfortably larger than any homelab traffic Tunnelsmith
	// is meant to carry while still bounded. Bodies above the cap get a
	// 413; they cannot be safely retried without spooling to disk, which
	// is out of scope for v1.
	//
	// When the client declared Content-Length, reject early without
	// reading any body bytes: the alternative is to drain the oversize
	// body just to send a 413, which defeats the memory bound.
	if r.ContentLength > maxBufferedRequestBody {
		h.logger.Warn("forward body too large for retry buffer (declared)",
			"host", host,
			"declared_bytes", r.ContentLength,
			"cap_bytes", maxBufferedRequestBody,
		)
		_ = r.Body.Close()
		http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
		return
	}
	bodyBytes, oversize, err := readBoundedBody(r.Body, maxBufferedRequestBody)
	if err != nil {
		_ = r.Body.Close()
		h.logger.Warn("forward read body failed", "host", host, "err", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	_ = r.Body.Close()
	if oversize {
		h.logger.Warn("forward body too large for retry buffer (streamed)",
			"host", host,
			"cap_bytes", maxBufferedRequestBody,
		)
		http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
		return
	}

	retryCap := h.currentRetryCap()
	detector := h.currentDetector()
	tried := make(map[string]bool, retryCap)
	// Resolve the rule and the body-inspect knobs once per request:
	// the destination host is constant across retry attempts, the
	// rule pointer is stable for the duration of the request (Reload
	// installs a new pointer atomically), and rebuilding the
	// failure.Pattern slice on every retry would just allocate the
	// same content over and over. The slice is non-nil only when
	// inspection is actually wired up for this host.
	rule := h.currentRules().Match(host)
	bodyBufferBytes := h.currentBodyBufferBytes()
	var bodyPatterns []failure.Pattern
	if rule.HasBodyRegex() && bodyBufferBytes > 0 {
		bodyPatterns = make([]failure.Pattern, len(rule.BodyRegex))
		for i, re := range rule.BodyRegex {
			bodyPatterns[i] = re
		}
	}
	var (
		retries        int
		cascadeAlready bool
	)
	for attempt := 0; attempt < retryCap; attempt++ {
		if err := r.Context().Err(); err != nil {
			h.logger.Warn("forward client context done",
				"host", host,
				"attempt", attempt+1,
				"err", err,
			)
			panic(http.ErrAbortHandler)
		}
		up, pickErr := h.sb.Pick(host, tried)
		if pickErr != nil {
			cascadeAlready = errors.Is(pickErr, scoreboard.ErrCascadeCooling)
			h.logger.Warn("forward pick exhausted",
				"host", host,
				"attempt", attempt+1,
				"err", pickErr,
			)
			break
		}
		outReq := h.newOutReq(r, bodyBytes)
		tport := h.transportFor(up)
		attemptStart := time.Now()
		resp, rtErr := tport.RoundTrip(outReq)
		latency := time.Since(attemptStart)

		if rtErr != nil {
			// Distinguish caller-cancelled from upstream failure: if the
			// client's request context is done, the dial error reflects a
			// teardown rather than the upstream being unable to reach the
			// host. Skip RecordFailure so the upstream is not penalized
			// for the client's hangup.
			if ctxErr := r.Context().Err(); ctxErr != nil {
				h.logger.Warn("forward client context done mid-attempt",
					"host", host,
					"upstream_id", up.ID(),
					"attempt", attempt+1,
					"err", ctxErr,
				)
				panic(http.ErrAbortHandler)
			}
			// RoundTrip errors come in two flavors:
			//   1. dial-level (refused, timeout) - the upstream could not
			//      open a TCP/TLS path to the destination.
			//   2. post-dial (TLS verification, HTTP parse, server hung up
			//      mid-response, etc.) - the upstream connected fine but
			//      something past the dial layer failed.
			// Only the first kind is fairly attributed to the upstream.
			// Recording the second kind would penalize the upstream for
			// problems that may belong to the destination, so we narrow
			// to explicit timeout / refused matches and skip RecordFailure
			// for everything else. The request still rotates to the next
			// upstream because tried[] gets the id either way.
			var kind failure.Kind
			switch {
			case failure.IsTimeout(rtErr):
				kind = failure.KindTimeout
			case failure.IsConnectionRefused(rtErr):
				kind = failure.KindRefused
			}
			if kind != "" {
				h.sb.RecordFailure(host, up.ID(), kind, nil)
			}
			h.observeForwardDial(up.ID(), forwardDialOutcome(kind), latency)
			tried[up.ID()] = true
			retries++
			h.logger.Warn("forward attempt failed",
				"host", host,
				"upstream_id", up.ID(),
				"kind", string(kind),
				"latency_ms", latency.Milliseconds(),
				"attempt", attempt+1,
				"err", rtErr,
			)
			continue
		}

		// detector may be nil per NewHTTP / Reload's contract: that
		// mode means "status detection disabled, treat every response
		// as success". Skip the call entirely so the nil-detector path
		// cannot panic.
		var (
			det    failure.Detection
			isFail bool
		)
		if detector != nil {
			det, isFail = detector.Detect(resp.StatusCode, resp.Header)
		}
		if isFail {
			h.sb.RecordFailure(host, up.ID(), det.Kind, det.CooldownOverride)
			// The dial itself succeeded (the upstream answered with a
			// status code); the failure signal is the response body, not
			// the connection. Record dial success so dial_attempts_total
			// stays consistent with the retry loop, and let
			// status_failures_total carry the failure-shaped dimension.
			h.observeForwardDial(up.ID(), "success", latency)
			h.observeStatusFailure(up.ID(), resp.StatusCode)
			drainAndClose(resp.Body)
			tried[up.ID()] = true
			retries++
			retryAfterMS := int64(-1)
			if det.CooldownOverride != nil {
				retryAfterMS = det.CooldownOverride.Milliseconds()
			}
			h.logger.Warn("forward status failure",
				"host", host,
				"upstream_id", up.ID(),
				"status", resp.StatusCode,
				"kind", string(det.Kind),
				"retry_after_ms", retryAfterMS,
				"latency_ms", latency.Milliseconds(),
				"attempt", attempt+1,
			)
			continue
		}

		// Phase 8: response-body inspection. Skip the call entirely
		// when bodyPatterns is nil (no rule matched the host, no
		// patterns on the matching rule, or the buffer cap is 0).
		// Status-code path above already consumed any failure shape,
		// so a body match here is a fresh signal: the destination
		// served 2xx but the page itself looks like a soft block.
		// failure.BufferAndDecide handles the encoded-body skip, the
		// limit, and the replay reader so the listener stays simple.
		if len(bodyPatterns) > 0 {
			dec, bdErr := failure.BufferAndDecide(resp.Body, resp.Header.Get("Content-Encoding"), bodyBufferBytes, bodyPatterns)
			if bdErr != nil {
				// Read failed mid-body. The upstream's TCP path is
				// suspect but not necessarily broken; rotating to the
				// next upstream is the right move, but pinning a
				// penalty on this kind of transient is not. Skip
				// RecordFailure and just rotate.
				h.logger.Warn("forward body inspect failed",
					"host", host,
					"upstream_id", up.ID(),
					"attempt", attempt+1,
					"err", bdErr,
				)
				h.observeForwardDial(up.ID(), "other", latency)
				tried[up.ID()] = true
				retries++
				continue
			}
			if dec.Matched {
				h.sb.RecordFailure(host, up.ID(), failure.KindBodyMatch, nil)
				// dial_attempts_total: the dial succeeded and the
				// response started, so this still counts as a
				// successful dial outcome. The body-match dimension
				// rides on the structured log line below.
				h.observeForwardDial(up.ID(), "success", latency)
				drainAndClose(dec.Replay)
				tried[up.ID()] = true
				retries++
				h.logger.Warn("forward body match",
					"host", host,
					"upstream_id", up.ID(),
					"pattern", dec.Pattern,
					"latency_ms", latency.Milliseconds(),
					"attempt", attempt+1,
				)
				continue
			}
			// No match (or skipped due to encoding). Replace resp.Body
			// with the replay reader so writeForwardSuccess streams the
			// already-buffered prefix plus rest to the client without
			// dropping a byte.
			resp.Body = dec.Replay
		}

		h.sb.RecordSuccess(host, up.ID(), latency)
		h.observeForwardDial(up.ID(), "success", latency)
		h.writeForwardSuccess(w, resp, up.ID(), retries)
		h.logger.Info("forward done",
			"url", r.URL.String(),
			"host", host,
			"upstream_id", up.ID(),
			"status", resp.StatusCode,
			"retries", retries,
			"latency_ms", time.Since(start).Milliseconds(),
		)
		h.observeOutcome("success")
		return
	}

	// Exhausted: every attempt either failed to dial or returned a
	// status-code-detected failure. Trip cascade if Pick did not already
	// hand back a cascade error, then respond 502 with the cascade headers.
	if !cascadeAlready {
		h.sb.TripCascade(host)
	}
	w.Header().Set("X-Tunnelsmith-Cascade", host)
	w.Header().Set("X-Tunnelsmith-Retries", strconv.Itoa(retries))
	http.Error(w, "Bad Gateway", http.StatusBadGateway)
	h.logger.Warn("forward exhausted",
		"url", r.URL.String(),
		"host", host,
		"retries", retries,
		"latency_ms", time.Since(start).Milliseconds(),
	)
	if cascadeAlready {
		h.observeOutcome("cascade")
	} else {
		h.observeOutcome("exhausted")
	}
}

// forwardDialOutcome maps a classified RoundTrip dial error to the metrics
// outcome label. Empty kind (post-dial failure we cannot fairly attribute)
// becomes "other" so it stays counted but distinct from refused/timeout.
func forwardDialOutcome(kind failure.Kind) string {
	switch kind {
	case failure.KindRefused:
		return "refused"
	case failure.KindTimeout:
		return "timeout"
	default:
		return "other"
	}
}

// newOutReq clones r for an outbound attempt. RequestURI is cleared, hop
// and proxy headers are stripped, and the body is rebuilt from the buffered
// bytes so retries replay the same payload. GetBody is set as well; the
// stdlib transport calls GetBody on internal retries (e.g. an HTTP/1
// connection that the server closed mid-request) and on 307/308 redirects.
//
// TransferEncoding and Trailer are cleared on the outbound request: when
// the inbound request was chunked, r.Clone preserves
// TransferEncoding=["chunked"], which would race ContentLength on the
// outbound side and surface a "request has both Content-Length and
// Transfer-Encoding" error from the transport. The buffered body has a
// known length, so chunked framing is not needed on the way out.
//
// Host is pinned to URL.Host so the outbound Host header matches the dial
// target. Clients can legally send a forward-proxy request whose Host
// header disagrees with the absolute URL (e.g. GET http://a.example/...
// HTTP/1.1\r\nHost: b.example); without this normalization the transport
// would dial a.example but advertise b.example, which misroutes virtual
// hosts at the destination and lets the cascade key (URL.Hostname) drift
// from what the origin actually serves.
func (h *HTTPServer) newOutReq(r *http.Request, body []byte) *http.Request {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.TransferEncoding = nil
	out.Trailer = nil
	out.Host = out.URL.Host
	if len(body) > 0 {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	} else {
		out.Body = http.NoBody
		out.ContentLength = 0
		out.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
	}
	stripHopHeaders(out.Header)
	stripProxyHeaders(out.Header)
	return out
}

// writeForwardSuccess copies the upstream response back to the client and
// adds the Tunnelsmith-specific headers. Hop-by-hop headers are stripped
// per RFC 7230 §6.1; any X-Tunnelsmith-* headers the destination tried to
// inject are stripped too so a malicious or careless server cannot spoof
// the proxy's own observability namespace. Everything else (Set-Cookie,
// Cache-Control, ...) is forwarded verbatim. Closes resp.Body when done.
func (h *HTTPServer) writeForwardSuccess(w http.ResponseWriter, resp *http.Response, upID string, retries int) {
	defer func() { _ = resp.Body.Close() }()
	respHeader := resp.Header.Clone()
	stripHopHeaders(respHeader)
	stripTunnelsmithHeaders(respHeader)
	for k, vs := range respHeader {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Tunnelsmith-Upstream", upID)
	w.Header().Set("X-Tunnelsmith-Retries", strconv.Itoa(retries))
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		h.logger.Warn("forward copy failed", "upstream_id", upID, "err", err)
	}
}

// observeOutcome records the terminal outcome of one client request when a
// metrics sink is attached. Outcomes match metrics package constants:
// "success" / "cascade" / "exhausted".
func (h *HTTPServer) observeOutcome(outcome string) {
	if h.metrics == nil {
		return
	}
	h.metrics.ObserveRequestOutcome(outcome)
}

// observeStatusFailure records a per-(upstream, status) counter for a
// listener-detected failure response.
func (h *HTTPServer) observeStatusFailure(upstreamID string, code int) {
	if h.metrics == nil {
		return
	}
	h.metrics.ObserveStatusFailure(upstreamID, strconv.Itoa(code))
}

// observeForwardDial records the forward path's per-attempt dial outcome.
// outcome mirrors the scoreboard's labels ("success" / "refused" / "timeout"
// / "other") so /metrics shows the same shape across paths.
func (h *HTTPServer) observeForwardDial(upstreamID string, outcome string, latency time.Duration) {
	if h.metrics == nil {
		return
	}
	h.metrics.ObserveDial(upstreamID, outcome, latency)
}

// connectOutcome maps a CONNECT-side dial error to a terminal outcome label.
// Cascade is its own outcome; everything else is "exhausted".
func connectOutcome(err error) string {
	if errors.Is(err, scoreboard.ErrCascadeCooling) {
		return "cascade"
	}
	return "exhausted"
}

// stripTunnelsmithHeaders removes any X-Tunnelsmith-* headers from h.
// http.Header keys are stored in canonical MIME form ("X-Tunnelsmith-...")
// so a single prefix check covers every variant a destination might send.
func stripTunnelsmithHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(k, "X-Tunnelsmith-") {
			h.Del(k)
		}
	}
}

// drainAndClose drains and closes a response body that the listener
// intends to discard. The drain is bounded two ways: by bytes (64 KiB
// via io.LimitReader) and by time (drainTimeout via a goroutine plus
// timer). A misbehaving upstream that ships a status line plus headers
// and then stalls on body bytes would otherwise block the retry loop
// here indefinitely, since LimitReader caps bytes but not wall time.
// On timeout the body is closed (which interrupts the in-flight read in
// the helper goroutine) and the goroutine is awaited so it does not
// leak. Either path means keep-alive reuse is best-effort: bodies that
// fit within the cap and arrive before the timeout let the transport
// return the conn to its pool; anything else makes the transport close
// the conn instead.
func drainAndClose(body io.ReadCloser) {
	const (
		maxDrain     = 64 << 10 // 64 KiB
		drainTimeout = 250 * time.Millisecond
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrain))
	}()
	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()
	select {
	case <-done:
		_ = body.Close()
	case <-timer.C:
		// Close interrupts the in-flight Read inside the goroutine; wait
		// for the goroutine to observe the close and exit so we never
		// return with a live drain still running.
		_ = body.Close()
		<-done
	}
}

// maxBufferedRequestBody caps how much of an inbound request body the
// listener buffers for retry replay. 8 MiB covers homelab traffic
// (Sonarr, browsers, scrapers) with plenty of margin and bounds the
// memory a single request can pin. Requests above this size get a 413;
// adding a streaming or disk-spool fallback for larger bodies is a
// post-v1 question.
const maxBufferedRequestBody = 8 << 20 // 8 MiB

// readBoundedBody reads body up to limit+1 bytes. If the result is
// strictly larger than limit, the body is reported as oversize and the
// returned slice contains the prefix that was read (the rest of body is
// not drained: the caller is expected to respond and bail). Errors
// propagate as-is; an EOF inside Read is not an error.
func readBoundedBody(body io.Reader, limit int64) ([]byte, bool, error) {
	if body == nil {
		return nil, false, nil
	}
	buf, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > limit {
		return buf, true, nil
	}
	return buf, false, nil
}

// hopHeaders are the connection-scoped headers RFC 7230 §6.1 says a proxy
// must not forward.
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func stripHopHeaders(h http.Header) {
	// Headers listed in Connection are also hop-by-hop for this exchange.
	if c := h.Get("Connection"); c != "" {
		for _, name := range strings.Split(c, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func stripProxyHeaders(h http.Header) {
	h.Del("Proxy-Connection")
	h.Del("Proxy-Authenticate")
	h.Del("Proxy-Authorization")
}

// hijackedConn wraps the net.Conn returned by http.Hijacker.Hijack so that
// reads pull from the bufio.Reader first. After Hijack, that reader may
// already hold bytes the client sent after the CONNECT headers (typically
// a pipelined TLS ClientHello); reading the conn directly would lose them.
type hijackedConn struct {
	net.Conn
	r *bufio.Reader
}

func newHijackedConn(c net.Conn, r *bufio.Reader) *hijackedConn {
	return &hijackedConn{Conn: c, r: r}
}

func (c *hijackedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

// tunnel pairs a client conn with an upstream conn and pumps bytes both
// directions until either side closes. Tracking these explicitly lets
// Shutdown drain or force-close them.
type tunnel struct {
	client net.Conn
	up     net.Conn
	once   sync.Once
	done   chan struct{}
}

func newTunnel(client, up net.Conn) *tunnel {
	return &tunnel{client: client, up: up, done: make(chan struct{})}
}

func (t *tunnel) run() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(t.up, t.client)
		_ = t.up.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(t.client, t.up)
		_ = t.client.Close()
	}()
	wg.Wait()
	t.once.Do(func() { close(t.done) })
}

func (t *tunnel) wait() { <-t.done }

func (t *tunnel) forceClose() {
	_ = t.client.Close()
	_ = t.up.Close()
}

func (h *HTTPServer) registerTunnel(client, up net.Conn) *tunnel {
	t := newTunnel(client, up)
	h.tunnelsMu.Lock()
	h.tunnels[t] = struct{}{}
	h.tunnelsMu.Unlock()
	return t
}

func (h *HTTPServer) unregisterTunnel(t *tunnel) {
	h.tunnelsMu.Lock()
	delete(h.tunnels, t)
	h.tunnelsMu.Unlock()
}

func (h *HTTPServer) snapshotTunnels() []*tunnel {
	h.tunnelsMu.Lock()
	defer h.tunnelsMu.Unlock()
	out := make([]*tunnel, 0, len(h.tunnels))
	for t := range h.tunnels {
		out = append(out, t)
	}
	return out
}
