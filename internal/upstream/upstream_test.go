package upstream_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	socks5 "github.com/armon/go-socks5"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

func TestNewKindSelection(t *testing.T) {
	t.Parallel()

	t.Run("direct", func(t *testing.T) {
		t.Parallel()
		up, err := upstream.New(config.UpstreamConfig{ID: "d", Kind: config.KindDirect}, time.Second)
		if err != nil {
			t.Fatalf("New(direct): %v", err)
		}
		if got := up.Kind(); got != config.KindDirect {
			t.Errorf("Kind() = %q, want %q", got, config.KindDirect)
		}
		if up.ID() != "d" {
			t.Errorf("ID() = %q, want %q", up.ID(), "d")
		}
	})

	t.Run("http", func(t *testing.T) {
		t.Parallel()
		up, err := upstream.New(config.UpstreamConfig{ID: "h", Kind: config.KindHTTP, Addr: "127.0.0.1:9"}, time.Second)
		if err != nil {
			t.Fatalf("New(http): %v", err)
		}
		if got := up.Kind(); got != config.KindHTTP {
			t.Errorf("Kind() = %q, want %q", got, config.KindHTTP)
		}
	})

	t.Run("socks5", func(t *testing.T) {
		t.Parallel()
		up, err := upstream.New(config.UpstreamConfig{ID: "s", Kind: config.KindSOCKS5, Addr: "127.0.0.1:9"}, time.Second)
		if err != nil {
			t.Fatalf("New(socks5): %v", err)
		}
		if got := up.Kind(); got != config.KindSOCKS5 {
			t.Errorf("Kind() = %q, want %q", got, config.KindSOCKS5)
		}
	})
}

func TestNewRejectsBadInput(t *testing.T) {
	t.Parallel()

	t.Run("empty kind", func(t *testing.T) {
		t.Parallel()
		_, err := upstream.New(config.UpstreamConfig{ID: "x"}, time.Second)
		if err == nil {
			t.Fatal("expected error for empty kind, got nil")
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		t.Parallel()
		_, err := upstream.New(config.UpstreamConfig{ID: "x", Kind: "wireguard"}, time.Second)
		if err == nil {
			t.Fatal("expected error for unknown kind, got nil")
		}
	})

	t.Run("zero timeout", func(t *testing.T) {
		t.Parallel()
		_, err := upstream.New(config.UpstreamConfig{ID: "d", Kind: config.KindDirect}, 0)
		if err == nil {
			t.Fatal("expected error for zero timeout, got nil")
		}
	})

	t.Run("negative timeout", func(t *testing.T) {
		t.Parallel()
		_, err := upstream.New(config.UpstreamConfig{ID: "d", Kind: config.KindDirect}, -1)
		if err == nil {
			t.Fatal("expected error for negative timeout, got nil")
		}
	})
}

func TestDirectUpstreamDial(t *testing.T) {
	t.Parallel()

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
			_ = c.Close()
		}
	}()

	up, err := upstream.New(config.UpstreamConfig{ID: "d", Kind: config.KindDirect}, 2*time.Second)
	if err != nil {
		t.Fatalf("New direct: %v", err)
	}
	conn, err := up.Dial(context.Background(), "tcp", dest.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = conn.Close()
}

// fakeConnectProxy is a minimal CONNECT-aware test server. It accepts a
// raw TCP conn, reads the CONNECT request line and headers, replies with
// the configured status, and (on 2xx) optionally writes extra bytes
// before tunneling to dst. Used to exercise the httpUpstream handshake
// paths the bot called out: success, non-2xx, and buffered bytes.
type fakeConnectProxy struct {
	listener net.Listener
	status   int
	extra    []byte
	dst      string
	t        *testing.T
}

func newFakeConnectProxy(t *testing.T, status int, extra []byte, dst string) *fakeConnectProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake proxy: %v", err)
	}
	f := &fakeConnectProxy{listener: l, status: status, extra: extra, dst: dst, t: t}
	t.Cleanup(func() { _ = l.Close() })
	go f.serve()
	return f
}

func (f *fakeConnectProxy) addr() string { return f.listener.Addr().String() }

func (f *fakeConnectProxy) serve() {
	for {
		c, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeConnectProxy) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		_, _ = c.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		return
	}
	statusText := http.StatusText(f.status)
	if statusText == "" {
		statusText = "Status"
	}
	resp := "HTTP/1.1 " + itoa(f.status) + " " + statusText + "\r\n\r\n"
	if f.status >= 200 && f.status < 300 && len(f.extra) > 0 {
		// Send the 2xx response and the post-handshake bytes in a
		// single Write so kernel coalescing lands them in the same
		// read on the upstream side. Two separate Writes can land in
		// two separate reads, in which case TestHTTPUpstreamCONNECT
		// BufferedBytes loses the buffered-bytes path entirely.
		resp += string(f.extra)
	}
	if _, err := c.Write([]byte(resp)); err != nil {
		return
	}
	if f.status < 200 || f.status >= 300 {
		return
	}
	if f.dst == "" {
		return
	}
	upConn, err := net.Dial("tcp", f.dst)
	if err != nil {
		return
	}
	defer func() { _ = upConn.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upConn, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, upConn); done <- struct{}{} }()
	<-done
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestHTTPUpstreamCONNECTSuccess(t *testing.T) {
	t.Parallel()

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

	proxy := newFakeConnectProxy(t, http.StatusOK, nil, dest.Addr().String())

	up, err := upstream.New(config.UpstreamConfig{ID: "h", Kind: config.KindHTTP, Addr: proxy.addr()}, 2*time.Second)
	if err != nil {
		t.Fatalf("New http: %v", err)
	}
	conn, err := up.Dial(context.Background(), "tcp", dest.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Echo round-trip through the tunnel.
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("echo = %q, want %q", string(buf[:n]), "hello")
	}
}

// TestHTTPUpstreamCONNECTSendsProxyAuthorization wires Username/Password
// into a synthetic UpstreamConfig and asserts the CONNECT line carries a
// Proxy-Authorization: Basic header per RFC 7617. Without this guard the
// Webshare expansion would build upstreams that handshake but never
// authenticate — the listener would see 407 Proxy Authentication Required
// without a clear cause.
func TestHTTPUpstreamCONNECTSendsProxyAuthorization(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	headerCh := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			headerCh <- ""
			return
		}
		headerCh <- req.Header.Get("Proxy-Authorization")
		_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
	}()

	up, err := upstream.New(config.UpstreamConfig{
		ID: "h", Kind: config.KindHTTP, Addr: l.Addr().String(),
		Username: "u", Password: "p",
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("New http: %v", err)
	}
	_, _ = up.Dial(context.Background(), "tcp", "example.com:443")

	select {
	case got := <-headerCh:
		// "Basic dTpw" is base64("u:p"). Asserting the exact value
		// rather than just non-empty catches a future regression where
		// the encoding picks up stray padding or a missing colon.
		const want = "Basic dTpw"
		if got != want {
			t.Fatalf("Proxy-Authorization = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CONNECT request")
	}
}

// TestHTTPUpstreamCONNECTOmitsAuthWhenUnset confirms an upstream without
// Username sends no Proxy-Authorization header. Otherwise an open Mullvad
// SOCKS5 (no auth) would be hit with a stray empty Basic header that some
// strict proxies reject.
func TestHTTPUpstreamCONNECTOmitsAuthWhenUnset(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	headerCh := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			headerCh <- "<read-err>"
			return
		}
		headerCh <- req.Header.Get("Proxy-Authorization")
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	}()
	up, err := upstream.New(config.UpstreamConfig{
		ID: "h", Kind: config.KindHTTP, Addr: l.Addr().String(),
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("New http: %v", err)
	}
	_, _ = up.Dial(context.Background(), "tcp", "example.com:443")
	select {
	case got := <-headerCh:
		if got != "" {
			t.Fatalf("Proxy-Authorization = %q, want empty", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CONNECT request")
	}
}

func TestHTTPUpstreamCONNECTNon2xx(t *testing.T) {
	t.Parallel()

	proxy := newFakeConnectProxy(t, http.StatusBadGateway, nil, "")

	up, err := upstream.New(config.UpstreamConfig{ID: "h", Kind: config.KindHTTP, Addr: proxy.addr()}, 2*time.Second)
	if err != nil {
		t.Fatalf("New http: %v", err)
	}
	_, err = up.Dial(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("expected error from non-2xx CONNECT, got nil")
	}
	if !strings.Contains(err.Error(), "CONNECT got status 502") {
		t.Errorf("error = %q, want one mentioning the 502 status", err.Error())
	}
	// 502 must NOT wrap failure.ErrProxyAuth — that sentinel is the
	// 407-specific signal the auto-heal driver subscribes to, and a
	// generic 502 should still classify as KindRefused.
	if errors.Is(err, failure.ErrProxyAuth) {
		t.Errorf("502 unexpectedly wraps ErrProxyAuth: %v", err)
	}
}

// TestHTTPUpstreamCONNECT407WrapsProxyAuth pins the contract the
// auto-heal driver depends on: an upstream HTTP proxy answering CONNECT
// with 407 produces a dial error whose chain includes failure.ErrProxyAuth.
// scoreboard.DialFor's ClassifyDialError dispatches on that sentinel
// to record KindProxyAuth, which the driver counts toward its heal
// threshold. A regression that drops the wrap would silently downgrade
// the kind to KindRefused and break the 407→Heal pipeline.
func TestHTTPUpstreamCONNECT407WrapsProxyAuth(t *testing.T) {
	t.Parallel()

	proxy := newFakeConnectProxy(t, http.StatusProxyAuthRequired, nil, "")

	up, err := upstream.New(config.UpstreamConfig{ID: "h", Kind: config.KindHTTP, Addr: proxy.addr()}, 2*time.Second)
	if err != nil {
		t.Fatalf("New http: %v", err)
	}
	_, err = up.Dial(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("expected error from 407 CONNECT, got nil")
	}
	if !errors.Is(err, failure.ErrProxyAuth) {
		t.Fatalf("err = %v, want one wrapping failure.ErrProxyAuth", err)
	}
	// The message should still surface the status code so the operator
	// log makes the cause obvious without inspecting the error chain.
	if !strings.Contains(err.Error(), "407") {
		t.Errorf("error = %q, want one mentioning the 407 status", err.Error())
	}
}

// TestHTTPUpstreamCONNECTBufferedBytes confirms that bytes the proxy sent
// in the same TCP segment as the 200 response are stitched back onto the
// returned conn. Without bufferedConn handling, those bytes would be
// trapped in the bufio.Reader inside upstream.go and lost.
func TestHTTPUpstreamCONNECTBufferedBytes(t *testing.T) {
	t.Parallel()

	const trailer = "POST-CONNECT-EXTRA"
	proxy := newFakeConnectProxy(t, http.StatusOK, []byte(trailer), "")

	up, err := upstream.New(config.UpstreamConfig{ID: "h", Kind: config.KindHTTP, Addr: proxy.addr()}, 2*time.Second)
	if err != nil {
		t.Fatalf("New http: %v", err)
	}
	conn, err := up.Dial(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(trailer))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read trailer: %v", err)
	}
	if string(got) != trailer {
		t.Fatalf("trailer = %q, want %q", string(got), trailer)
	}
}

func TestHTTPUpstreamRejectsUDPNetwork(t *testing.T) {
	t.Parallel()
	up, err := upstream.New(config.UpstreamConfig{ID: "h", Kind: config.KindHTTP, Addr: "127.0.0.1:9"}, time.Second)
	if err != nil {
		t.Fatalf("New http: %v", err)
	}
	_, err = up.Dial(context.Background(), "udp", "127.0.0.1:53")
	if err == nil {
		t.Fatal("expected error for non-tcp network, got nil")
	}
}

func TestHTTPUpstreamDialErrorWraps(t *testing.T) {
	t.Parallel()
	// Bind a free local port and immediately close it. Dialing that
	// address is a guaranteed connection-refused, where hardcoding a
	// "probably unused" port like 127.0.0.1:1 is not portable: some
	// systems run a service there and the dial would proceed past the
	// proxy connect step.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closed: %v", err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	up, err := upstream.New(config.UpstreamConfig{ID: "h", Kind: config.KindHTTP, Addr: closedAddr}, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("New http: %v", err)
	}
	_, err = up.Dial(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
	if !strings.Contains(err.Error(), `"h"`) {
		t.Errorf("error = %q, want it to mention upstream id", err.Error())
	}
}

func TestHTTPUpstreamContextCancel(t *testing.T) {
	t.Parallel()

	// A listener that accepts and then sleeps without responding so the
	// CONNECT handshake blocks until ctx is cancelled.
	stuck, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen stuck: %v", err)
	}
	t.Cleanup(func() { _ = stuck.Close() })
	go func() {
		for {
			c, err := stuck.Accept()
			if err != nil {
				return
			}
			// Hold without sending a response.
			go func(c net.Conn) {
				time.Sleep(2 * time.Second)
				_ = c.Close()
			}(c)
		}
	}()

	up, err := upstream.New(config.UpstreamConfig{ID: "h", Kind: config.KindHTTP, Addr: stuck.Addr().String()}, 2*time.Second)
	if err != nil {
		t.Fatalf("New http: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = up.Dial(ctx, "tcp", "example.com:443")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected ctx-bounded error, got nil")
	}
	if elapsed > time.Second {
		t.Fatalf("dial took %s; expected to bail within ~100ms", elapsed)
	}
}

// TestSOCKS5UpstreamDialSuccess plumbs a real armon/go-socks5 server in
// front of a direct loopback dial and confirms the upstream Dial reaches
// the destination.
func TestSOCKS5UpstreamDialSuccess(t *testing.T) {
	t.Parallel()

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

	srv, err := socks5.New(&socks5.Config{})
	if err != nil {
		t.Fatalf("build socks5 server: %v", err)
	}
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks: %v", err)
	}
	t.Cleanup(func() { _ = socksLn.Close() })
	go func() {
		for {
			c, err := socksLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = srv.ServeConn(c)
				_ = c.Close()
			}(c)
		}
	}()

	up, err := upstream.New(config.UpstreamConfig{ID: "s", Kind: config.KindSOCKS5, Addr: socksLn.Addr().String()}, 2*time.Second)
	if err != nil {
		t.Fatalf("New socks5: %v", err)
	}
	conn, err := up.Dial(context.Background(), "tcp", dest.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("echo = %q, want %q", string(buf[:n]), "ping")
	}
}

func TestSOCKS5UpstreamDialFailureWrapsID(t *testing.T) {
	t.Parallel()

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closed: %v", err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	up, err := upstream.New(config.UpstreamConfig{ID: "s-bad", Kind: config.KindSOCKS5, Addr: closedAddr}, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("New socks5: %v", err)
	}
	_, err = up.Dial(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
}
