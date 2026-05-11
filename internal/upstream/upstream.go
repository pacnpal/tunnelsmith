// Package upstream wraps the three egress kinds Tunnelsmith supports:
// direct, HTTP CONNECT, and SOCKS5. The Upstream interface is the contract
// the listeners and (later) the scoreboard use to open connections to a
// destination through a chosen exit.
//
// Phase 2 covers the plumbing only. The listeners drive a single hardcoded
// upstream until Phase 3 introduces the priority pool and Phase 4 layers
// scoring on top.
package upstream

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

// Upstream is one egress option. Implementations dial the destination
// through whatever exit they wrap (direct, HTTP CONNECT, SOCKS5).
type Upstream interface {
	// ID is the configured upstream identifier, used for logs and metrics.
	ID() string

	// Kind reports the upstream kind for log fields and tests.
	Kind() config.UpstreamKind

	// Dial opens a connection to addr through this upstream. Network is
	// always "tcp" for now. The returned conn is owned by the caller.
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
}

// New builds the right upstream for the given config entry. The timeout
// applies to the connect step (TCP handshake plus, for HTTP CONNECT and
// SOCKS5, the protocol-level handshake to the proxy).
func New(cfg config.UpstreamConfig, timeout time.Duration) (Upstream, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("upstream %q: timeout must be > 0", cfg.ID)
	}
	switch cfg.Kind {
	case config.KindDirect:
		return newDirect(cfg, timeout), nil
	case config.KindHTTP:
		return newHTTP(cfg, timeout), nil
	case config.KindSOCKS5:
		return newSOCKS5(cfg, timeout)
	case "":
		return nil, fmt.Errorf("upstream %q: kind is required", cfg.ID)
	default:
		return nil, fmt.Errorf("upstream %q: unknown kind %q", cfg.ID, cfg.Kind)
	}
}

// directUpstream uses the host's default route. It is the fallback the
// proposal picks for hosts that do not need a tunnel.
type directUpstream struct {
	id      string
	dialer  *net.Dialer
	timeout time.Duration
}

func newDirect(cfg config.UpstreamConfig, timeout time.Duration) *directUpstream {
	return &directUpstream{
		id:      cfg.ID,
		dialer:  &net.Dialer{Timeout: timeout},
		timeout: timeout,
	}
}

func (u *directUpstream) ID() string                { return u.id }
func (u *directUpstream) Kind() config.UpstreamKind { return config.KindDirect }

func (u *directUpstream) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return u.dialer.DialContext(ctx, network, addr)
}

// httpUpstream forwards through an HTTP CONNECT proxy. It opens TCP to the
// upstream, sends a CONNECT request for the destination host:port, and
// returns the conn once the proxy answers 2xx. After that the conn is a
// raw byte tunnel.
//
// When username is non-empty the CONNECT request carries
// Proxy-Authorization: Basic <base64(user:pass)> per RFC 7617. The header
// is pre-encoded once at construction so each dial avoids the allocation.
type httpUpstream struct {
	id        string
	addr      string
	authValue string // empty disables proxy auth
	dialer    *net.Dialer
	timeout   time.Duration
}

func newHTTP(cfg config.UpstreamConfig, timeout time.Duration) *httpUpstream {
	return &httpUpstream{
		id:        cfg.ID,
		addr:      cfg.Addr,
		authValue: basicAuthHeader(cfg.Username, cfg.Password),
		dialer:    &net.Dialer{Timeout: timeout},
		timeout:   timeout,
	}
}

// basicAuthHeader returns the Proxy-Authorization value for the given
// credentials or "" when no username was configured. The "Basic " prefix
// is included so callers can set the header verbatim.
func basicAuthHeader(user, pass string) string {
	if user == "" {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func (u *httpUpstream) ID() string                { return u.id }
func (u *httpUpstream) Kind() config.UpstreamKind { return config.KindHTTP }

func (u *httpUpstream) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("http upstream %q: unsupported network %q", u.id, network)
	}
	conn, err := u.dialer.DialContext(ctx, "tcp", u.addr)
	if err != nil {
		return nil, fmt.Errorf("http upstream %q: dial proxy: %w", u.id, err)
	}
	// Bound the CONNECT handshake. If the caller's context fires sooner,
	// it wins; otherwise the per-upstream timeout caps things.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(u.timeout))
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u.authValue != "" {
		req.Header.Set("Proxy-Authorization", u.authValue)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("http upstream %q: write CONNECT: %w", u.id, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("http upstream %q: read CONNECT response: %w", u.id, err)
	}
	// CONNECT 2xx leaves the tunnel open. Any other code is a hard fail.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("http upstream %q: CONNECT got status %d", u.id, resp.StatusCode)
	}
	_ = resp.Body.Close()
	// Drop the deadline; the listener manages per-request deadlines after
	// the tunnel is established.
	_ = conn.SetDeadline(time.Time{})
	// If the bufio.Reader picked up bytes after the response, stitch them
	// back in front of the conn so the caller sees an unbroken byte stream.
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, br: br}, nil
	}
	return conn, nil
}

// socks5Upstream forwards through a SOCKS5 proxy using x/net/proxy. The
// underlying SOCKS5 dialer also implements proxy.ContextDialer, so we
// type-assert to call DialContext for cancellation support.
type socks5Upstream struct {
	id      string
	addr    string
	dialer  proxy.Dialer
	timeout time.Duration
}

func newSOCKS5(cfg config.UpstreamConfig, timeout time.Duration) (*socks5Upstream, error) {
	base := &net.Dialer{Timeout: timeout}
	var auth *proxy.Auth
	if cfg.Username != "" {
		auth = &proxy.Auth{User: cfg.Username, Password: cfg.Password}
	}
	d, err := proxy.SOCKS5("tcp", cfg.Addr, auth, base)
	if err != nil {
		return nil, fmt.Errorf("upstream %q: build socks5 dialer: %w", cfg.ID, err)
	}
	return &socks5Upstream{
		id:      cfg.ID,
		addr:    cfg.Addr,
		dialer:  d,
		timeout: timeout,
	}, nil
}

func (u *socks5Upstream) ID() string                { return u.id }
func (u *socks5Upstream) Kind() config.UpstreamKind { return config.KindSOCKS5 }

func (u *socks5Upstream) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if cd, ok := u.dialer.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	// Older fallback path. The current x/net release implements
	// ContextDialer, so this branch is unreachable in practice.
	return u.dialer.Dial(network, addr)
}

// bufferedConn lets a caller treat a net.Conn plus a bufio.Reader (carrying
// already-buffered bytes) as a single io.Reader. Writes pass through.
type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.br.Read(p)
}
