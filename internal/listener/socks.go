package listener

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"sync"

	socks5 "github.com/armon/go-socks5"

	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// SOCKSServer is a SOCKS5 listener that hands every connection to a single
// upstream. The Phase 2 contract is "use the first configured upstream";
// later phases swap that out for the scoreboard-driven Pool.
type SOCKSServer struct {
	addr     string
	upstream upstream.Upstream
	logger   *slog.Logger

	server *socks5.Server

	// ready closes once Serve has finished binding the listener (or
	// finished failing to bind). Channel receive after close forms a
	// happens-before edge with the listener field write below, so other
	// goroutines reading listener after <-ready do so race free.
	ready    chan struct{}
	listener net.Listener
	bindErr  error

	wg     sync.WaitGroup
	doneMu sync.Mutex
	done   bool
}

// NewSOCKS builds a SOCKS5 listener that dials through up.
func NewSOCKS(addr string, up upstream.Upstream, logger *slog.Logger) (*SOCKSServer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	s := &SOCKSServer{
		addr:     addr,
		upstream: up,
		logger:   logger.With("listener", "socks5", "upstream_id", up.ID()),
		ready:    make(chan struct{}),
	}
	srv, err := socks5.New(&socks5.Config{
		// armon/go-socks5 logs to stdout by default. Route into a discard
		// log.Logger so its output does not pollute Tunnelsmith's slog
		// JSON stream. Real failures surface through ServeConn's return
		// value, which we log via slog below.
		Logger: log.New(io.Discard, "", 0),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return up.Dial(ctx, network, addr)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build socks5 server: %w", err)
	}
	s.server = srv
	return s, nil
}

// Ready returns a channel that closes once Serve has either bound the
// listener or failed to bind.
func (s *SOCKSServer) Ready() <-chan struct{} { return s.ready }

// Addr returns the resolved listen address. Returns nil before the
// listener has bound; wait on Ready() in tests.
func (s *SOCKSServer) Addr() net.Addr {
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

// Serve binds the listener and accepts connections until Shutdown is
// called. Each accepted conn is handled in its own goroutine, tracked so
// Shutdown can drain in flight.
func (s *SOCKSServer) Serve(ctx context.Context) error {
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.bindErr = fmt.Errorf("socks listen %s: %w", s.addr, err)
		close(s.ready)
		return s.bindErr
	}
	s.listener = l
	close(s.ready)
	s.logger.Info("listening", "addr", l.Addr().String())

	for {
		conn, err := l.Accept()
		if err != nil {
			if s.isShuttingDown() && errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("socks accept: %w", err)
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() { _ = c.Close() }()
			if err := s.server.ServeConn(c); err != nil {
				s.logger.Debug("socks5 conn ended", "err", err, "remote", c.RemoteAddr().String())
			}
		}(conn)
	}
}

// Shutdown closes the listener (so Serve returns) and waits for in-flight
// connections to finish, bounded by ctx. Safe to call before Serve binds:
// it waits on Ready() so the listener field read is race free.
func (s *SOCKSServer) Shutdown(ctx context.Context) error {
	s.markDone()
	select {
	case <-s.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	var closeErr error
	if s.listener != nil {
		closeErr = s.listener.Close()
		// net.ErrClosed on a second close is benign and not surfaced.
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return closeErr
	case <-ctx.Done():
		if closeErr == nil {
			return ctx.Err()
		}
		return closeErr
	}
}

func (s *SOCKSServer) markDone() {
	s.doneMu.Lock()
	s.done = true
	s.doneMu.Unlock()
}

func (s *SOCKSServer) isShuttingDown() bool {
	s.doneMu.Lock()
	defer s.doneMu.Unlock()
	return s.done
}
