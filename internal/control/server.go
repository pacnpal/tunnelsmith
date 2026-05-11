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
// Auth is opt-in. Phase 11 shipped the endpoint with no credential
// check; the operator's only line of defence was binding Bind to
// loopback or a private subnet. Phase 12 adds optional bearer-token
// auth (ServerOptions.Tokens; GateHealthz pulls /healthz under the
// same gate when set). An empty token set keeps the Phase 11 wire
// shape byte-for-byte; a non-empty set requires Authorization: Bearer
// <token> on POST /v1/report (and on GET /healthz when GateHealthz is
// true). docs/cooperative-reporting.md and ADR-007 spell out the
// trust stance and the threat model around plaintext tokens on this
// listener (TLS termination on the control listener itself is a
// Phase 13 candidate).
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
	"crypto/tls"
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

	// tlsCertFile / tlsKeyFile are non-empty when the operator
	// configured [control].tls_cert_file + tls_key_file. config.Validate
	// already enforces both-or-neither, so the in-process invariant is
	// "either both empty (plaintext) or both set (TLS)". Captured at
	// construction so Serve can pick http.Server.ServeTLS over Serve
	// without re-reading config.
	tlsCertFile string
	tlsKeyFile  string

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
	// Providers is the optional registry of vendor-API bindings used by
	// GET /v1/providers and POST /v1/providers/{id_prefix}/refresh. nil
	// disables the routes entirely (Phase 11/12 wire shape stays
	// byte-for-byte identical for operators not using the feature).
	Providers *ProviderRegistry
	// TLSCertFile and TLSKeyFile, when both non-empty, switch the
	// listener to HTTPS via http.Server.ServeTLS. Both empty keeps
	// plaintext. config.Validate enforces the both-or-neither
	// invariant so Server doesn't have to re-check.
	TLSCertFile string
	TLSKeyFile  string
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
	if opts.Providers != nil {
		mountProvidersHandlers(mux, opts.Providers, tokens, logger)
	}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if opts.TLSCertFile != "" && opts.TLSKeyFile != "" {
		// Pin the minimum TLS version explicitly rather than
		// relying on whatever the stdlib default happens to be.
		// Go's default is TLS 1.2+ today, but the listener's
		// policy is operator-facing; encoding it here keeps the
		// contract immune to a future stdlib default flip.
		httpSrv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}
	return &Server{
		addr:        addr,
		logger:      logger,
		tokens:      tokens,
		tlsCertFile: opts.TLSCertFile,
		tlsKeyFile:  opts.TLSKeyFile,
		srv:         httpSrv,
		ready:       make(chan struct{}),
	}
}

// ReplaceTokens atomically swaps the live auth token set, used by the
// SIGHUP reloader so an operator can rotate tokens without bouncing
// the process. Safe to call concurrently with in-flight requests:
// each handler calls TokenSource.Snapshot() exactly once at the start
// of the request and runs every auth question (Enabled, Allow) off
// that captured view, so an in-flight check keeps a stable decision
// even if the underlying atomic pointer is rotated mid-handler.
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

// Serve binds the listener and blocks until Shutdown is called. When
// TLSCertFile and TLSKeyFile were set in ServerOptions the listener
// terminates TLS via http.Server.ServeTLS using the configured PEM
// files; otherwise plain HTTP. config.Validate enforces the
// both-or-neither invariant on the cert/key pair, but NewServer is
// also callable from tests and any future internal package that
// bypasses config.Validate. Fail fast here so a half-configured
// ServerOptions can never silently downgrade an intended-TLS
// listener to plaintext.
func (s *Server) Serve(_ context.Context) error {
	if (s.tlsCertFile == "") != (s.tlsKeyFile == "") {
		err := fmt.Errorf("control serve: tls_cert_file and tls_key_file must be both set or both empty (got cert=%q, key=%q)",
			s.tlsCertFile, s.tlsKeyFile)
		s.bindErr = err
		close(s.ready)
		return err
	}
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.bindErr = fmt.Errorf("control listen %s: %w", s.addr, err)
		close(s.ready)
		return s.bindErr
	}
	s.listener = l
	close(s.ready)
	s.logger.Info("listening",
		"addr", l.Addr().String(),
		"tls", s.TLSEnabled(),
	)
	var serveErr error
	if s.TLSEnabled() {
		serveErr = s.srv.ServeTLS(l, s.tlsCertFile, s.tlsKeyFile)
	} else {
		serveErr = s.srv.Serve(l)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("control serve: %w", serveErr)
	}
	return nil
}

// TLSEnabled reports whether this server is configured to terminate
// TLS. Used by Serve to pick ServeTLS vs Serve and by callers that
// want to log or expose the listener's transport mode.
func (s *Server) TLSEnabled() bool {
	return s != nil && s.tlsCertFile != "" && s.tlsKeyFile != ""
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
