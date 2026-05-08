// Command tunnelsmith is the per-destination egress router. Phase 2 wires
// up the HTTP and SOCKS5 listeners against a single hardcoded upstream
// (the first one in config). Phase 3 introduces a priority pool, Phase 4
// introduces the per-host scoreboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const (
	defaultConfigPath = "/etc/tunnelsmith/config.toml"
	shutdownTimeout   = 30 * time.Second
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "tunnelsmith:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("tunnelsmith", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		configPath  = fs.String("config", defaultConfigPath, "path to the TOML config file")
		printConfig = fs.Bool("print-config", false, "load the config, apply defaults, print the resolved TOML to stdout, and exit")
		printVer    = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *printVer {
		if _, err := fmt.Fprintf(stdout, "tunnelsmith %s (commit %s, built %s)\n", version, commit, date); err != nil {
			return fmt.Errorf("write version: %w", err)
		}
		return nil
	}

	logger := newLogger(stderr)
	logger.Info("starting", "version", version, "commit", commit, "built", date)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if len(cfg.UnknownKeys) > 0 {
		logger.Warn("config has unknown keys; check for typos",
			"path", *configPath,
			"keys", cfg.UnknownKeys,
		)
	}
	logger.Info("config loaded",
		"path", *configPath,
		"upstreams", len(cfg.Upstreams),
		"rules", len(cfg.Rules),
		"listener_http", cfg.Listener.HTTP,
		"listener_socks", cfg.Listener.SOCKS,
	)

	if *printConfig {
		out, err := cfg.Marshal()
		if err != nil {
			return fmt.Errorf("marshal config: %w", err)
		}
		if _, err := stdout.Write(out); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
		return nil
	}

	// Phase 2: hardcode the first upstream. Phase 3 introduces the pool.
	first := cfg.Upstreams[0]
	dialTimeout := time.Duration(cfg.Failure.TimeoutMS) * time.Millisecond
	up, err := upstream.New(first, dialTimeout)
	if err != nil {
		return fmt.Errorf("build upstream %q: %w", first.ID, err)
	}
	logger.Info("upstream selected (phase 2: first only)",
		"upstream_id", up.ID(),
		"upstream_kind", string(up.Kind()),
		"dial_timeout_ms", cfg.Failure.TimeoutMS,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	httpSrv := listener.NewHTTP(cfg.Listener.HTTP, up, logger)
	socksSrv, err := listener.NewSOCKS(cfg.Listener.SOCKS, up, logger)
	if err != nil {
		return fmt.Errorf("build socks listener: %w", err)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpSrv.Serve(gctx) })
	g.Go(func() error { return socksSrv.Serve(gctx) })

	// Wait for either ctx cancellation (signal) or a listener error.
	<-gctx.Done()
	logger.Info("shutdown initiated", "reason", contextReason(gctx))

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", "err", err)
	}
	if err := socksSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("socks shutdown error", "err", err)
	}

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("listener exited: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}

// contextReason renders the cause of a cancelled context as a string the
// log line can hand the operator without leaking internals.
func contextReason(ctx context.Context) string {
	if err := ctx.Err(); err != nil {
		return err.Error()
	}
	return "unknown"
}

// newLogger builds the structured JSON logger. Level is read from the
// TUNNELSMITH_LOG_LEVEL env var (debug, info, warn, error). Default: info.
func newLogger(w *os.File) *slog.Logger {
	level := slog.LevelInfo
	if raw := strings.ToLower(os.Getenv("TUNNELSMITH_LOG_LEVEL")); raw != "" {
		switch raw {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn", "warning":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
