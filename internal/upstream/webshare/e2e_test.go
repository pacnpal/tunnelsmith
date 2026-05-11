package webshare_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
	"github.com/pacnpal/tunnelsmith/internal/upstream/webshare"
)

// TestEndToEnd_WebshareDirectMode is the headline integration test:
// build the Webshare provider from a config block, expand it against a
// fake Webshare API, mount the expanded upstreams behind the production
// listener stack, and prove a CONNECT request lands on the destination
// after being authenticated by the per-proxy username/password the
// Webshare API handed back.
//
// The fake "Webshare proxy" listener requires the same Proxy-
// Authorization header the real Webshare expects (Basic
// base64(user:pass)) and rejects anything else with 407, so a
// regression that drops the credentials would surface here as a test
// failure rather than silently hitting Webshare in production.
func TestEndToEnd_WebshareDirectMode(t *testing.T) {
	t.Parallel()

	// 1. Destination httptest server — what the operator actually
	//    wanted to reach.
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(dest.Close)
	destURL, err := url.Parse(dest.URL)
	if err != nil {
		t.Fatalf("parse dest: %v", err)
	}

	// 2. Auth-required upstream HTTP proxy that pretends to be a
	//    Webshare exit. Echoes whatever it tunnels to dest. Rejects
	//    Proxy-Authorization mismatches with 407 so the test would
	//    fail loudly if the listener forgot to send the header.
	const wantUser = "ws-user"
	const wantPass = "ws-pass"
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(wantUser+":"+wantPass))
	proxyAuthHits := atomic.Int64{}
	upstreamProxy := newAuthCONNECTProxy(t, wantAuth, destURL.Host, &proxyAuthHits)

	// 3. Fake Webshare API. Serves /api/v2/profile/ and
	//    /api/v2/proxy/list/. The list returns ONE proxy pointing at
	//    upstreamProxy.addr with the credentials wantUser/wantPass so
	//    the expander materialises a config.UpstreamConfig whose Dial
	//    will send the right Proxy-Authorization header.
	host, portStr, err := net.SplitHostPort(upstreamProxy.addr())
	if err != nil {
		t.Fatalf("split upstreamProxy addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/profile/":
			_, _ = w.Write([]byte(`{"id": 1, "email": "u@example.com"}`))
		// /proxy/list/refresh/ must come before the broader
		// /proxy/list/ prefix branch — Go's switch evaluates
		// top-to-bottom and HasPrefix("/proxy/list/refresh/", "/proxy/list/")
		// is true, so the order matters.
		case strings.HasPrefix(r.URL.Path, "/proxy/list/refresh/"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/proxy/list/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count":    1,
				"next":     nil,
				"previous": nil,
				"results": []webshare.Proxy{{
					ID:           "d-1",
					Username:     wantUser,
					Password:     wantPass,
					ProxyAddress: host,
					Port:         port,
					Valid:        true,
					CountryCode:  "US",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)

	// 4. Build the Webshare client + expander as the real provider
	//    would, but pointed at the test API server.
	prov := webshare.NewProvider()
	cfg := config.UpstreamPoolConfig{
		Provider: "webshare",
		IDPrefix: "ws",
		APIToken: "test-token",
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	rawExp, err := prov.BuildExpander(cfg, logger)
	if err != nil {
		t.Fatalf("BuildExpander: %v", err)
	}
	exp, ok := rawExp.(*webshare.Expander)
	if !ok {
		t.Fatalf("BuildExpander: got %T, want *webshare.Expander", rawExp)
	}
	// Retarget the internal Client at the test API. The Client field
	// is exposed by the helper below so this E2E test can stay in
	// the *_test package without poking package internals.
	webshare.RetargetExpanderForTest(exp, api.URL, api.Client())

	// 5. Snapshot → upstreams → priority pool.
	startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upstreams, err := exp.Snapshot(startCtx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(upstreams) != 1 {
		t.Fatalf("expected 1 upstream, got %d", len(upstreams))
	}
	if upstreams[0].Username != wantUser || upstreams[0].Password != wantPass {
		t.Fatalf("credentials not threaded through expansion: %+v", upstreams[0])
	}

	upObj, err := upstream.New(upstreams[0], 5*time.Second)
	if err != nil {
		t.Fatalf("build upstream: %v", err)
	}
	pool, err := upstream.NewPool(
		[]upstream.PoolEntry{{Up: upObj, Priority: upstreams[0].PriorityValue()}},
		5,
		logger,
	)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}

	// 6. Scoreboard + listener.
	sb, err := scoreboard.New(pool, scoreboardConfigFor(t), scoreboard.WithLogger(logger))
	if err != nil {
		t.Fatalf("scoreboard.New: %v", err)
	}
	t.Cleanup(sb.Stop)
	httpSrv, err := listener.NewHTTP("127.0.0.1:0", sb, failure.NewStatusDetector(config.DefaultStatusRules), 5, logger)
	if err != nil {
		t.Fatalf("listener.NewHTTP: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(context.Background()) }()
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = httpSrv.Shutdown(shutCtx)
	})
	<-httpSrv.Ready()

	// 7. Drive a plain-HTTP request through the listener and prove
	//    the response came from dest, which proves the credentials
	//    passed the auth-required proxy in between.
	proxyURL := &url.URL{Scheme: "http", Host: httpSrv.Addr().String()}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(dest.URL)
	if err != nil {
		t.Fatalf("GET via listener: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Fatalf("body = %q, want %q", body, "hello")
	}
	if proxyAuthHits.Load() == 0 {
		t.Fatal("auth-required upstream proxy was never hit; the listener bypassed Webshare?")
	}
}

func scoreboardConfigFor(_ *testing.T) scoreboard.Config {
	return scoreboard.Config{
		ConnectionRefused: true,
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
}

// authCONNECTProxy is a minimal upstream HTTP-CONNECT proxy that demands
// a specific Proxy-Authorization header before tunneling to dst. Mirrors
// the shape of Webshare's actual proxy listeners closely enough for an
// end-to-end test.
type authCONNECTProxy struct {
	l        net.Listener
	wantAuth string
	dst      string
	hits     *atomic.Int64
}

func newAuthCONNECTProxy(t *testing.T, wantAuth, dst string, hits *atomic.Int64) *authCONNECTProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake proxy: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	p := &authCONNECTProxy{l: l, wantAuth: wantAuth, dst: dst, hits: hits}
	go p.serve()
	return p
}

func (p *authCONNECTProxy) addr() string { return p.l.Addr().String() }

func (p *authCONNECTProxy) serve() {
	for {
		c, err := p.l.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *authCONNECTProxy) handle(c net.Conn) {
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
	if got := req.Header.Get("Proxy-Authorization"); got != p.wantAuth {
		_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
		return
	}
	p.hits.Add(1)
	if _, err := c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		return
	}
	upConn, err := net.Dial("tcp", p.dst)
	if err != nil {
		return
	}
	defer func() { _ = upConn.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upConn, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, upConn); done <- struct{}{} }()
	<-done
}
