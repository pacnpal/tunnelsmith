package listener_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// scoreboardFor wraps pool in a Scoreboard with conservative defaults so the
// listener tests get the production dial path (Scoreboard.DialFor) rather
// than touching Pool.DialFor directly. The scoreboard's probe chance is
// zero so per-test outcomes do not depend on the random source; cascade
// TTL is also zero for the same reason.
func scoreboardFor(t *testing.T, pool *upstream.Pool) *scoreboard.Scoreboard {
	t.Helper()
	cfg := scoreboard.Config{
		ConnectionRefused: true, // match the TOML production default
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused:    {Penalty: 3, Cooldown: 30 * time.Second},
			failure.KindTimeout:    {Penalty: 2, Cooldown: 15 * time.Second},
			failure.KindRateLimit:  {Penalty: 4, Cooldown: 120 * time.Second},
			failure.KindForbidden:  {Penalty: 6, Cooldown: 30 * time.Minute},
			failure.KindLegalBlock: {Penalty: 8, Cooldown: 6 * time.Hour},
			failure.KindBodyMatch:  {Penalty: 5, Cooldown: 60 * time.Second},
		},
		SuccessWeight:  1,
		ScoreCap:       10,
		ProbeChance:    0,
		DecayInterval:  5 * time.Minute,
		DecayStep:      0.5,
		CascadeTTL:     0,
		DebounceWindow: 100 * time.Millisecond,
	}
	sb, err := scoreboard.New(pool, cfg, scoreboard.WithLogger(quietLogger()))
	if err != nil {
		t.Fatalf("build scoreboard: %v", err)
	}
	t.Cleanup(sb.Stop)
	return sb
}

// directScoreboard returns a single-upstream scoreboard that dials directly.
// Most listener tests use it so the test stays focused on listener behavior
// rather than scoreboard mechanics; the retry path has its own tests in
// internal/scoreboard and dedicated listener-level tests below.
func directScoreboard(t *testing.T) *scoreboard.Scoreboard {
	t.Helper()
	up, err := upstream.New(config.UpstreamConfig{ID: "direct", Kind: config.KindDirect}, 5*time.Second)
	if err != nil {
		t.Fatalf("build direct upstream: %v", err)
	}
	pool, err := upstream.NewPool(
		[]upstream.PoolEntry{{Up: up, Priority: 10}},
		5,
		quietLogger(),
	)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	return scoreboardFor(t, pool)
}

// quietLogger discards log output so tests do not spam stderr. Tests check
// behavior, not log content.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// defaultDetector returns a StatusDetector seeded with the proposal's
// recommended 429/403/451 rules. Listener tests that do not exercise
// status-code behavior still need a non-nil detector so the forward path
// runs the detection step (which is a no-op for 2xx responses).
func defaultDetector() *failure.StatusDetector {
	return failure.NewStatusDetector(config.DefaultStatusRules)
}

// startHTTPListener binds the HTTP listener to a free port and returns it
// plus a cleanup hook. Serve runs in a goroutine; cleanup blocks until
// shutdown completes.
func startHTTPListener(t *testing.T) (*listener.HTTPServer, *url.URL) {
	t.Helper()
	srv, err := listener.NewHTTP("127.0.0.1:0", directScoreboard(t), defaultDetector(), 5, quietLogger())
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

// startSOCKSListener mirrors startHTTPListener for the SOCKS5 path.
func startSOCKSListener(t *testing.T) *listener.SOCKSServer {
	t.Helper()
	srv, err := listener.NewSOCKS("127.0.0.1:0", directScoreboard(t), quietLogger())
	if err != nil {
		t.Fatalf("build socks listener: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(context.Background()) }()

	select {
	case <-srv.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("socks listener did not bind in time")
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Logf("socks shutdown: %v", err)
		}
		select {
		case err := <-serveErr:
			if err != nil {
				t.Logf("socks serve returned: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Logf("socks serve did not return after shutdown")
		}
	})
	return srv
}

// poolWithUnreachableThenDirect builds a two-entry pool whose first entry is
// a SOCKS5 upstream pointed at a closed local port (so its dial errors
// immediately) and whose second entry is direct. Used to confirm the
// listener composes with the pool's retry behavior.
func poolWithUnreachableThenDirect(t *testing.T) *upstream.Pool {
	t.Helper()

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closed: %v", err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	bad, err := upstream.New(
		config.UpstreamConfig{ID: "socks-bad", Kind: config.KindSOCKS5, Addr: closedAddr},
		2*time.Second,
	)
	if err != nil {
		t.Fatalf("build bad upstream: %v", err)
	}
	good, err := upstream.New(
		config.UpstreamConfig{ID: "direct", Kind: config.KindDirect},
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("build good upstream: %v", err)
	}
	pool, err := upstream.NewPool(
		[]upstream.PoolEntry{
			{Up: bad, Priority: 10},
			{Up: good, Priority: 20},
		},
		5,
		quietLogger(),
	)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	return pool
}

// TestHTTPForwardProxyFallsBackThroughPool exercises the listener composed
// with a pool whose first upstream refuses every dial. The listener must
// surface a 200 because the pool's second upstream (direct) succeeds. This
// is the listener-side counterpart to the pool-only tests in
// internal/upstream.
func TestHTTPForwardProxyFallsBackThroughPool(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-fallback")
	}))
	t.Cleanup(dest.Close)

	sb := scoreboardFor(t, poolWithUnreachableThenDirect(t))
	srv, err := listener.NewHTTP("127.0.0.1:0", sb, defaultDetector(), 5, quietLogger())
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
		_ = srv.Shutdown(ctx)
		<-serveErr
	})

	proxyURL := &url.URL{Scheme: "http", Host: srv.Addr().String()}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get via fallback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-fallback" {
		t.Fatalf("body = %q, want %q", string(body), "via-fallback")
	}
}

// TestSOCKS5FallsBackThroughPool is the SOCKS5 mirror of the HTTP test
// above. The library does not surface upstream-id metadata, so the test
// only asserts the request succeeds via the second pool entry.
func TestSOCKS5FallsBackThroughPool(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "socks-via-fallback")
	}))
	t.Cleanup(dest.Close)

	sb := scoreboardFor(t, poolWithUnreachableThenDirect(t))
	srv, err := listener.NewSOCKS("127.0.0.1:0", sb, quietLogger())
	if err != nil {
		t.Fatalf("build socks listener: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(context.Background()) }()
	select {
	case <-srv.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("socks listener did not bind in time")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-serveErr
	})

	dialer, err := proxy.SOCKS5("tcp", srv.Addr().String(), nil, &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("build SOCKS5 client dialer: %v", err)
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		},
	}
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get via SOCKS5 fallback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "socks-via-fallback" {
		t.Fatalf("body = %q, want %q", string(body), "socks-via-fallback")
	}
}

// TestNewHTTPErrorsOnNilScoreboard locks in the constructor's nil-scoreboard
// guard. Without it, the forward retry loop would nil-deref on the first
// request.
func TestNewHTTPErrorsOnNilScoreboard(t *testing.T) {
	t.Parallel()
	srv, err := listener.NewHTTP("127.0.0.1:0", nil, defaultDetector(), 5, quietLogger())
	if err == nil {
		t.Fatal("NewHTTP(nil scoreboard) returned nil error")
	}
	if srv != nil {
		t.Fatalf("NewHTTP(nil scoreboard) returned non-nil server alongside error: %v", srv)
	}
}

// TestNewHTTPErrorsOnZeroRetryCap locks in the constructor's retryCap guard.
// A retryCap of zero would mean the forward loop never runs, surfacing as a
// 502 on every plain-HTTP request without an obvious cause.
func TestNewHTTPErrorsOnZeroRetryCap(t *testing.T) {
	t.Parallel()
	srv, err := listener.NewHTTP("127.0.0.1:0", directScoreboard(t), defaultDetector(), 0, quietLogger())
	if err == nil {
		t.Fatal("NewHTTP(retryCap=0) returned nil error")
	}
	if srv != nil {
		t.Fatalf("NewHTTP(retryCap=0) returned non-nil server alongside error: %v", srv)
	}
}

// TestNewSOCKSErrorsOnNilScoreboard locks in the constructor's
// nil-scoreboard guard. Without it, the socks5 Dial callback would
// nil-deref on the first conn.
func TestNewSOCKSErrorsOnNilScoreboard(t *testing.T) {
	t.Parallel()
	srv, err := listener.NewSOCKS("127.0.0.1:0", nil, quietLogger())
	if err == nil {
		t.Fatal("NewSOCKS(nil scoreboard) returned nil error")
	}
	if srv != nil {
		t.Fatalf("NewSOCKS(nil scoreboard) returned non-nil server alongside error: %v", srv)
	}
}

// TestSOCKS5ShutdownForcesIdleConns confirms Shutdown does not hang on a
// client that opened a TCP conn but never sent the SOCKS5 handshake. The
// listener must force-close active conns when its context expires before
// the WaitGroup drains.
func TestSOCKS5ShutdownForcesIdleConns(t *testing.T) {
	t.Parallel()

	srv := startSOCKSListener(t)

	// Raw TCP conn to the SOCKS port. Do not send any SOCKS bytes: the
	// library's ServeConn will block on the method-selection read and
	// the handler goroutine will never finish on its own.
	idle, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial socks: %v", err)
	}
	defer func() { _ = idle.Close() }()

	// Poll the listener until it has registered the conn, so the test
	// does not race Accept on slow runners. 2s gives plenty of headroom
	// without making green runs slower in any meaningful way.
	deadline := time.Now().Add(2 * time.Second)
	for srv.ActiveConns() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("listener did not register idle conn within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	shutdownStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Logf("Shutdown returned %v (acceptable; the property is that it returned)", err)
	}
	elapsed := time.Since(shutdownStart)
	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s; expected to return well under 2s after force-close", elapsed)
	}

	// The idle conn should now read EOF or a closed-conn error.
	_ = idle.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf := make([]byte, 16)
	if _, err := idle.Read(buf); err == nil {
		t.Fatal("expected idle conn read to fail after Shutdown, got nil err")
	}
}

func TestHTTPForwardProxy(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "ok")
		_, _ = io.WriteString(w, "hello")
	}))
	t.Cleanup(dest.Close)

	_, proxyURL := startHTTPListener(t)
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Fatalf("body = %q, want %q", string(body), "hello")
	}
	if got := resp.Header.Get("X-Test"); got != "ok" {
		t.Fatalf("X-Test header = %q, want %q", got, "ok")
	}
}

func TestHTTPConnectTunnel(t *testing.T) {
	t.Parallel()
	dest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tunnel-ok")
	}))
	t.Cleanup(dest.Close)

	_, proxyURL := startHTTPListener(t)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server uses a self-signed cert
		},
	}
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get over CONNECT: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "tunnel-ok" {
		t.Fatalf("body = %q, want %q", string(body), "tunnel-ok")
	}
}

func TestSOCKS5Tunnel(t *testing.T) {
	t.Parallel()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "socks-ok")
	}))
	t.Cleanup(dest.Close)

	srv := startSOCKSListener(t)

	dialer, err := proxy.SOCKS5("tcp", srv.Addr().String(), nil, &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("build SOCKS5 client dialer: %v", err)
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		},
	}
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("client.Get via SOCKS5: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "socks-ok" {
		t.Fatalf("body = %q, want %q", string(body), "socks-ok")
	}
}

// TestHTTPConnectPipelinedBytesReachUpstream confirms the listener forwards
// bytes that arrive in the same TCP segment as the CONNECT request. net/http
// reads ahead while parsing headers and buffers anything after the trailing
// \r\n\r\n in the hijack's bufio.Reader. If the listener tunneled directly
// from the raw clientConn, those buffered bytes would be lost and the
// upstream would see a truncated stream.
func TestHTTPConnectPipelinedBytesReachUpstream(t *testing.T) {
	t.Parallel()

	// Echo destination: sends back whatever the client wrote.
	dest, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dest: %v", err)
	}
	t.Cleanup(func() { _ = dest.Close() })
	go func() {
		for {
			c, err := dest.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	srv, err := listener.NewHTTP("127.0.0.1:0", directScoreboard(t), defaultDetector(), 5, quietLogger())
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
		_ = srv.Shutdown(ctx)
		<-serveErr
	})

	clientConn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	// Send the CONNECT request and a pipelined payload in a single Write
	// so net/http's bufio.Reader sees the trailing payload while parsing
	// the request line and headers.
	const payload = "PIPELINED-AFTER-CONNECT"
	connectReq := "CONNECT " + dest.Addr().String() + " HTTP/1.1\r\nHost: " + dest.Addr().String() + "\r\n\r\n"
	if _, err := clientConn.Write([]byte(connectReq + payload)); err != nil {
		t.Fatalf("write CONNECT + payload: %v", err)
	}

	// Parse the CONNECT response with http.ReadResponse so partial TCP
	// reads do not break the assertion. The bufio.Reader keeps any
	// post-headers bytes it pulled while parsing, so the io.ReadFull
	// below picks up the echoed payload regardless of how the kernel
	// split the response and the payload across reads.
	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(clientConn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read echoed payload: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("echoed payload = %q, want %q (the listener dropped pipelined bytes from the hijack buffer)", string(got), payload)
	}
}

// TestHTTPConnectShutdownDrains opens a CONNECT tunnel that the destination
// holds open, calls Shutdown with a short timeout, and confirms the
// listener forces the tunnel closed within the timeout instead of hanging.
func TestHTTPConnectShutdownDrains(t *testing.T) {
	t.Parallel()

	// A TCP server that accepts and just blocks on read until the conn
	// is closed. Stand-in for a real long-lived CONNECT destination.
	dest, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dest: %v", err)
	}
	t.Cleanup(func() { _ = dest.Close() })
	go func() {
		for {
			c, err := dest.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	srv, err := listener.NewHTTP("127.0.0.1:0", directScoreboard(t), defaultDetector(), 5, quietLogger())
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

	// Open a raw client conn to the proxy, send CONNECT, expect 200.
	clientConn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = clientConn.Close() }()
	connectReq := "CONNECT " + dest.Addr().String() + " HTTP/1.1\r\nHost: " + dest.Addr().String() + "\r\n\r\n"
	if _, err := clientConn.Write([]byte(connectReq)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// Read just the CONNECT response status line + headers terminator.
	buf := make([]byte, 1024)
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
		t.Fatalf("unexpected CONNECT response: %q", string(buf[:n]))
	}
	_ = clientConn.SetReadDeadline(time.Time{})

	// Tunnel is up. Now shut down with a short timeout. Shutdown must
	// return within timeout-plus-slack, even though the tunnel would
	// otherwise stay open forever.
	shutdownStart := time.Now()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	elapsed := time.Since(shutdownStart)

	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s, expected to return well under 2s", elapsed)
	}
	// Force-close path returns the ctx error or http server's error.
	// Either is acceptable; the property under test is "Shutdown returns
	// promptly and the tunnel is closed".
	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Logf("Shutdown returned %v (acceptable)", shutdownErr)
	}

	// Reading from the client conn must now return EOF or a closed-conn
	// error; the proxy hung up its side.
	_ = clientConn.SetReadDeadline(time.Now().Add(1 * time.Second))
	if _, err := clientConn.Read(buf); err == nil {
		t.Fatal("expected client conn read to fail after Shutdown, got nil err")
	}

	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}
