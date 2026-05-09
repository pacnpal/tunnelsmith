// Package ui owns the Phase 9 web UI: a small embedded HTML page that
// renders the scoreboard, the upstream pool, and the active Force pins,
// plus four JSON action endpoints (forget, force, reset, plus the
// scoreboard read).
//
// The UI deliberately has no auth. The security boundary is the host
// network or the Docker bridge: the operator binds the listener to a
// loopback address or to a private subnet that only trusted clients
// can reach. docs/ui.md spells this out so an operator does not
// accidentally expose the action endpoints to the world.
package ui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server wraps an http.Server that exposes the UI at /, the JSON
// actions at /api/*, and a /healthz probe. It is safe to call Serve
// and Shutdown concurrently.
type Server struct {
	addr   string
	logger *slog.Logger
	srv    *http.Server

	ready    chan struct{}
	listener net.Listener
	bindErr  error
}

// NewServer builds a UI HTTP server that serves on addr. backend supplies
// the read and mutating operations the handlers call into; tests pass a
// fake to drive every endpoint without spinning up a real scoreboard.
func NewServer(addr string, backend Backend, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "ui")
	mux := http.NewServeMux()
	mountHandlers(mux, backend, logger)
	return &Server{
		addr:   addr,
		logger: logger,
		srv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
		ready: make(chan struct{}),
	}
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
		s.bindErr = fmt.Errorf("ui listen %s: %w", s.addr, err)
		close(s.ready)
		return s.bindErr
	}
	s.listener = l
	close(s.ready)
	s.logger.Info("listening", "addr", l.Addr().String())
	if err := s.srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("ui serve: %w", err)
	}
	return nil
}

// Shutdown stops the UI server.
func (s *Server) Shutdown(ctx context.Context) error {
	select {
	case <-s.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.srv.Shutdown(ctx)
}
