// Package listener owns the HTTP and SOCKS5 entry points. Each listener
// receives a client connection, asks an upstream.Pool to dial through to
// the destination, and pumps bytes between the two. The pool tries each
// upstream in priority order and retries on hard failure up to the
// configured cap; Phase 4 layers per-host scoring on top of the same
// iteration.
package listener

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// HTTPServer accepts HTTP CONNECT and plain-HTTP forward-proxy traffic.
type HTTPServer struct {
	addr   string
	pool   *upstream.Pool
	logger *slog.Logger

	server *http.Server

	// transport is shared across all forward-proxy requests so the
	// per-Transport allocation and bookkeeping happens once at boot, not
	// on every request. DisableKeepAlives stays true: with the priority
	// pool deciding upstreams per dial and Phase 4 about to layer scoring
	// on top, reusing TCP connections would pin requests to whichever
	// upstream served the cold dial. Phase 4 revisits this trade-off.
	transport *http.Transport

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

// chosenIDCtxKey carries a *string into the forward-proxy DialContext so
// the dial callback can hand the upstream id back to the per-request log
// line without sharing mutable state across goroutines on the Transport.
type chosenIDCtxKey struct{}

// NewHTTP builds an HTTP listener that routes everything through pool.
func NewHTTP(addr string, pool *upstream.Pool, logger *slog.Logger) *HTTPServer {
	if logger == nil {
		logger = slog.Default()
	}
	h := &HTTPServer{
		addr:    addr,
		pool:    pool,
		logger:  logger.With("listener", "http"),
		ready:   make(chan struct{}),
		tunnels: make(map[*tunnel]struct{}),
	}
	h.transport = &http.Transport{
		DialContext:       h.dialThroughPool,
		DisableKeepAlives: true,
	}
	h.server = &http.Server{
		Addr:              addr,
		Handler:           http.HandlerFunc(h.handle),
		ReadHeaderTimeout: 30 * time.Second,
	}
	return h
}

// dialThroughPool is the Transport's DialContext. The pool picks an
// upstream and returns the conn; if a *string is stashed in ctx under
// chosenIDCtxKey, the chosen upstream id is written there for the caller's
// log line.
func (h *HTTPServer) dialThroughPool(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, id, err := h.pool.DialFor(ctx, network, addr)
	if err == nil {
		if box, ok := ctx.Value(chosenIDCtxKey{}).(*string); ok {
			*box = id
		}
	}
	return conn, err
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
	upConn, upID, err := h.pool.DialFor(r.Context(), "tcp", host)
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

func (h *HTTPServer) handleForward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() {
		http.Error(w, "absolute URL required for forward proxy", http.StatusBadRequest)
		return
	}
	start := time.Now()

	// Per-request *string box passed to the shared Transport's
	// DialContext via the request context. Each in-flight request has
	// its own box, so concurrent requests do not race on the chosen id.
	var chosenID string
	ctx := context.WithValue(r.Context(), chosenIDCtxKey{}, &chosenID)
	outReq := r.Clone(ctx)
	outReq.RequestURI = ""
	stripHopHeaders(outReq.Header)
	stripProxyHeaders(outReq.Header)

	resp, err := h.transport.RoundTrip(outReq)
	if err != nil {
		h.logger.Warn("forward roundtrip failed", "url", r.URL.String(), "err", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respHeader := resp.Header.Clone()
	stripHopHeaders(respHeader)
	for k, vs := range respHeader {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		h.logger.Warn("forward copy failed", "url", r.URL.String(), "err", err)
		return
	}

	h.logger.Info("forward done",
		"url", r.URL.String(),
		"upstream_id", chosenID,
		"status", resp.StatusCode,
		"latency_ms", time.Since(start).Milliseconds(),
	)
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
