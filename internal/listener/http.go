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

// HTTPServer accepts HTTP CONNECT and plain-HTTP forward-proxy traffic.
type HTTPServer struct {
	addr     string
	sb       *scoreboard.Scoreboard
	detector *failure.StatusDetector
	retryCap int
	logger   *slog.Logger

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

// NewHTTP builds an HTTP listener that routes everything through sb. The
// scoreboard must be non-nil; passing nil returns a clear error so callers
// see the contract violation at construction time instead of a nil-deref
// on the first request. detector may be nil, in which case status-code
// detection is disabled and every response is treated as success. retryCap
// must be at least 1 and bounds the per-request attempts on the plain-HTTP
// forward path; the value mirrors failure.max_retries_per_request from
// config so dial retries and status retries share the same budget.
func NewHTTP(addr string, sb *scoreboard.Scoreboard, detector *failure.StatusDetector, retryCap int, logger *slog.Logger) (*HTTPServer, error) {
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
	h.server = &http.Server{
		Addr:              addr,
		Handler:           http.HandlerFunc(h.handle),
		ReadHeaderTimeout: 30 * time.Second,
	}
	return h, nil
}

// transportFor returns the Transport pinned to up, building it on first use.
// Each Transport's DialContext is closed over the upstream's Dial method, so
// HTTP keep-alive pools conns to the destination through the same upstream.
// MaxIdleConnsPerHost is intentionally small: a homelab proxy serving a few
// concurrent clients does not need the stdlib default of 2 to stretch into
// the hundreds, and bounding it makes idle-conn churn observable.
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
	start := time.Now()
	host := r.URL.Hostname()
	if host == "" {
		host = hostOnly(r.Host)
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Warn("forward read body failed", "host", host, "err", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	_ = r.Body.Close()

	tried := make(map[string]bool, h.retryCap)
	var (
		retries        int
		cascadeAlready bool
	)
	for attempt := 0; attempt < h.retryCap; attempt++ {
		if err := r.Context().Err(); err != nil {
			h.logger.Warn("forward client context done",
				"host", host,
				"attempt", attempt+1,
				"err", err,
			)
			return
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
				return
			}
			kind := failure.ClassifyDialError(rtErr)
			if kind != "" {
				h.sb.RecordFailure(host, up.ID(), kind, 0)
			}
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

		det, isFail := h.detector.Detect(resp.StatusCode, resp.Header)
		if isFail {
			h.sb.RecordFailure(host, up.ID(), det.Kind, det.RetryAfter)
			drainAndClose(resp.Body)
			tried[up.ID()] = true
			retries++
			h.logger.Warn("forward status failure",
				"host", host,
				"upstream_id", up.ID(),
				"status", resp.StatusCode,
				"kind", string(det.Kind),
				"retry_after_ms", det.RetryAfter.Milliseconds(),
				"latency_ms", latency.Milliseconds(),
				"attempt", attempt+1,
			)
			continue
		}

		h.sb.RecordSuccess(host, up.ID(), latency)
		h.writeForwardSuccess(w, resp, up.ID(), retries)
		h.logger.Info("forward done",
			"url", r.URL.String(),
			"host", host,
			"upstream_id", up.ID(),
			"status", resp.StatusCode,
			"retries", retries,
			"latency_ms", time.Since(start).Milliseconds(),
		)
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
}

// newOutReq clones r for an outbound attempt. RequestURI is cleared, hop
// and proxy headers are stripped, and the body is rebuilt from the buffered
// bytes so retries replay the same payload. GetBody is set as well; the
// stdlib transport calls GetBody on internal retries (e.g. an HTTP/1
// connection that the server closed mid-request) and on 307/308 redirects.
func (h *HTTPServer) newOutReq(r *http.Request, body []byte) *http.Request {
	out := r.Clone(r.Context())
	out.RequestURI = ""
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
// per RFC 7230 §6.1; everything else (including Set-Cookie, Cache-Control,
// etc.) is forwarded verbatim. Closes resp.Body when done.
func (h *HTTPServer) writeForwardSuccess(w http.ResponseWriter, resp *http.Response, upID string, retries int) {
	defer func() { _ = resp.Body.Close() }()
	respHeader := resp.Header.Clone()
	stripHopHeaders(respHeader)
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

// drainAndClose drains and closes a response body that the listener intends
// to discard. Draining lets the underlying transport reuse the conn for
// keep-alive instead of closing it; the body cap is bounded so a malicious
// or buggy upstream cannot make Tunnelsmith block here forever.
func drainAndClose(body io.ReadCloser) {
	const maxDrain = 64 << 10 // 64 KiB
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrain))
	_ = body.Close()
}

// hostOnly returns the host portion of a host:port pair. Falls back to the
// whole input if SplitHostPort fails so logs and metrics still carry
// something useful.
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
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
