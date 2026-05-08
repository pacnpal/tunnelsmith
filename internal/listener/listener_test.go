package listener_test

import (
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
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// directPool returns a single-upstream pool that dials directly. Most
// listener tests use it so the test stays focused on listener behavior
// rather than pool retry mechanics; the retry path has its own tests in
// internal/upstream and a dedicated listener-level test below.
func directPool(t *testing.T) *upstream.Pool {
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
	return pool
}

// quietLogger discards log output so tests do not spam stderr. Tests check
// behavior, not log content.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// startHTTPListener binds the HTTP listener to a free port and returns it
// plus a cleanup hook. Serve runs in a goroutine; cleanup blocks until
// shutdown completes.
func startHTTPListener(t *testing.T) (*listener.HTTPServer, *url.URL) {
	t.Helper()
	srv := listener.NewHTTP("127.0.0.1:0", directPool(t), quietLogger())

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
	srv, err := listener.NewSOCKS("127.0.0.1:0", directPool(t), quietLogger())
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

	pool := poolWithUnreachableThenDirect(t)
	srv := listener.NewHTTP("127.0.0.1:0", pool, quietLogger())

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

	pool := poolWithUnreachableThenDirect(t)
	srv, err := listener.NewSOCKS("127.0.0.1:0", pool, quietLogger())
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

// TestNewHTTPPanicsOnNilPool locks in the constructor's nil-pool guard.
// Without it, the first request would nil-deref inside dialThroughPool.
func TestNewHTTPPanicsOnNilPool(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewHTTP(nil pool) did not panic")
		}
	}()
	_ = listener.NewHTTP("127.0.0.1:0", nil, quietLogger())
}

// TestNewSOCKSErrorsOnNilPool locks in the constructor's nil-pool guard.
// Without it, the socks5 Dial callback would nil-deref on the first conn.
func TestNewSOCKSErrorsOnNilPool(t *testing.T) {
	t.Parallel()
	srv, err := listener.NewSOCKS("127.0.0.1:0", nil, quietLogger())
	if err == nil {
		t.Fatal("NewSOCKS(nil pool) returned nil error")
	}
	if srv != nil {
		t.Fatalf("NewSOCKS(nil pool) returned non-nil server alongside error: %v", srv)
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

	// Brief pause so Serve picks up the conn and registers it before
	// Shutdown takes a snapshot.
	time.Sleep(50 * time.Millisecond)

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

	srv := listener.NewHTTP("127.0.0.1:0", directPool(t), quietLogger())
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
	req := "CONNECT " + dest.Addr().String() + " HTTP/1.1\r\nHost: " + dest.Addr().String() + "\r\n\r\n" + payload
	if _, err := clientConn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT + payload: %v", err)
	}

	// Read CONNECT 200.
	buf := make([]byte, 1024)
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
		t.Fatalf("unexpected CONNECT response: %q", string(buf[:n]))
	}
	// CONNECT response can include the echoed payload in the same read on
	// some systems; if it does, slice it off and read the rest below.
	got := ""
	if idx := strings.Index(string(buf[:n]), "\r\n\r\n"); idx >= 0 {
		got = string(buf[:n][idx+4:])
	}

	// Read until we have the full echoed payload back.
	deadline := time.Now().Add(3 * time.Second)
	for len(got) < len(payload) && time.Now().Before(deadline) {
		_ = clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := clientConn.Read(buf)
		if n > 0 {
			got += string(buf[:n])
		}
		if err != nil && !errors.Is(err, io.EOF) {
			break
		}
	}

	if got != payload {
		t.Fatalf("echoed payload = %q, want %q (the listener dropped pipelined bytes from the hijack buffer)", got, payload)
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

	srv := listener.NewHTTP("127.0.0.1:0", directPool(t), quietLogger())
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
