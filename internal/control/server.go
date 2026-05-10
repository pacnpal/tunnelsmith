// Package control owns the Phase 11 cooperative-reporting endpoint:
// a small HTTP listener that lets app integrations submit per-request
// outcomes (ok / soft geo-block / app-detected rate-limit / etc.) so
// the scoreboard learns from HTTPS traffic the proxy cannot inspect.
//
// Why this exists: CONNECT and SOCKS5 carry TLS the proxy cannot read.
// Apps that already terminate TLS (because they are the legitimate
// endpoint) submit reports via POST /v1/report, and Tunnelsmith feeds
// them into Scoreboard.RecordSuccess / Scoreboard.RecordFailure exactly
// like the listener-detected status codes from Phase 5.
//
// The endpoint deliberately has no auth. The security boundary is the
// host network or Docker bridge, same as the UI listener; the operator
// binds Bind to loopback or a private subnet that only trusted clients
// can reach. docs/cooperative-reporting.md spells this out.
//
// The wire protocol:
//
//	POST /v1/report HTTP/1.1
//	Content-Type: application/json
//
//	{
//	  "host":        "example.com:443",
//	  "upstream":    "mullvad-se-got",
//	  "outcome":     "geo_block",
//	  "http_status": 200
//	}
//
//	→ 204 No Content on success.
//	→ 400 Bad Request on malformed JSON, missing fields, or unknown outcome.
//	→ 404 Not Found when "upstream" is not in the pool.
//	→ 405 Method Not Allowed for any non-POST method.
//	→ 413 Payload Too Large when the body exceeds 4 KiB.
//	→ 503 Service Unavailable when the scoreboard backend is unavailable.
//
// docs/cooperative-reporting.md is the public reference.
package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server wraps an http.Server that serves the control endpoints. Safe to
// call Serve and Shutdown concurrently.
type Server struct {
	addr   string
	logger *slog.Logger
	srv    *http.Server
	tokens *tokenSet // always non-nil; an empty token set encodes the no-auth default

	ready    chan struct{}
	listener net.Listener
	bindErr  error
}

// ServerOptions threads the Phase 12 auth knobs into NewServer without
// growing the positional signature past 4 params (the original Phase 11
// shape). Empty Tokens + GateHealthz=false reproduces Phase 11 behavior
// exactly; the unauthenticated wire shape stays the default.
type ServerOptions struct {
	// Tokens is the initial auth token set. Empty/nil = no auth.
	Tokens []string
	// GateHealthz pulls /healthz under the auth gate when Tokens is
	// also non-empty. Default false keeps liveness probes ungated.
	GateHealthz bool
}

// NewServer builds a control HTTP server that serves on addr. backend is
// the scoreboard surface the report handler calls into; tests pass a fake
// to drive every endpoint without spinning up a real scoreboard. metrics
// may be nil; when set, the handlers emit Phase 11 counters through it.
// opts may be a zero value, which keeps the Phase 11 no-auth default.
func NewServer(addr string, backend Backend, metrics MetricsSink, opts ServerOptions, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "control")
	tokens := NewTokenSet(opts.Tokens)
	mux := http.NewServeMux()
	mountHandlers(mux, backend, metrics, tokens, opts.GateHealthz, logger)
	return &Server{
		addr:   addr,
		logger: logger,
		tokens: tokens,
		srv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
		ready: make(chan struct{}),
	}
}

// ReplaceTokens atomically swaps the live auth token set, used by the
// SIGHUP reloader so an operator can rotate tokens without bouncing the
// process. Safe to call concurrently with in-flight requests; readers
// load the pointer snapshot once per request so an in-flight check
// keeps its decision even if the swap happens mid-handler.
func (s *Server) ReplaceTokens(tokens []string) {
	if s == nil || s.tokens == nil {
		return
	}
	s.tokens.Replace(tokens)
	s.logger.Info("control auth tokens reloaded", "count", len(tokens))
}

// Ready returns a channel that closes once Serve has either bound the
// listener or failed to bind. Tests use this to wait for Addr() before
// dialing.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Addr returns the resolved listening address. Returns nil before the
// listener has bound; callers should wait on Ready() first.
func (s *Server) Addr() net.Addr {
	select {
	case <-s.ready:
	default:
		return nil
	}
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Serve binds the listener and blocks until Shutdown is called.
func (s *Server) Serve(_ context.Context) error {
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.bindErr = fmt.Errorf("control listen %s: %w", s.addr, err)
		close(s.ready)
		return s.bindErr
	}
	s.listener = l
	close(s.ready)
	s.logger.Info("listening", "addr", l.Addr().String())
	if err := s.srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("control serve: %w", err)
	}
	return nil
}

// Shutdown stops the control server.
func (s *Server) Shutdown(ctx context.Context) error {
	select {
	case <-s.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.srv.Shutdown(ctx)
}
