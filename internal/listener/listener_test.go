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

// directUpstream returns an upstream that dials with no proxy in the way.
// All listener tests in this file use it so the test stays focused on
// listener behavior, not upstream-specific dial logic.
func directUpstream(t *testing.T) upstream.Upstream {
	t.Helper()
	up, err := upstream.New(config.UpstreamConfig{ID: "direct", Kind: config.KindDirect}, 5*time.Second)
	if err != nil {
		t.Fatalf("build direct upstream: %v", err)
	}
	return up
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
	srv := listener.NewHTTP("127.0.0.1:0", directUpstream(t), quietLogger())

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
	srv, err := listener.NewSOCKS("127.0.0.1:0", directUpstream(t), quietLogger())
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

	srv := listener.NewHTTP("127.0.0.1:0", directUpstream(t), quietLogger())
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
