// Command tunnelsmith is the per-destination egress router. The HTTP and
// SOCKS5 listeners dial through a per-(host, upstream) scoreboard that
// wraps a static priority pool: the pool keeps the configured upstream
// list, the scoreboard layers per-host scoring, cooldowns, time decay,
// cascade-failure handling, and recovery probing on top.
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
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
	"github.com/pacnpal/tunnelsmith/internal/upstream/mullvad"
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	dialTimeout := time.Duration(cfg.Failure.TimeoutMS) * time.Millisecond
	expandedPools, err := expandUpstreamPools(ctx, cfg.UpstreamPools, logger)
	if err != nil {
		return fmt.Errorf("expand upstream pools: %w", err)
	}
	allUpstreams := make([]config.UpstreamConfig, 0, len(cfg.Upstreams)+len(expandedPools))
	allUpstreams = append(allUpstreams, cfg.Upstreams...)
	allUpstreams = append(allUpstreams, expandedPools...)
	if len(allUpstreams) == 0 {
		return errors.New("no upstreams available after expanding [[upstream_pool]] (check provider connectivity and country filters)")
	}
	entries := make([]upstream.PoolEntry, 0, len(allUpstreams))
	for _, uc := range allUpstreams {
		up, err := upstream.New(uc, dialTimeout)
		if err != nil {
			return fmt.Errorf("build upstream %q: %w", uc.ID, err)
		}
		entries = append(entries, upstream.PoolEntry{Up: up, Priority: uc.PriorityValue()})
	}
	pool, err := upstream.NewPool(entries, cfg.Failure.MaxRetriesPerRequest, logger)
	if err != nil {
		return fmt.Errorf("build upstream pool: %w", err)
	}
	logger.Info("upstream pool built",
		"upstreams", pool.IDs(),
		"retry_cap", cfg.Failure.MaxRetriesPerRequest,
		"dial_timeout_ms", cfg.Failure.TimeoutMS,
	)

	sb, err := scoreboard.New(pool, scoreboard.FromConfig(cfg.Failure.Scoring, cfg.Failure.Status),
		scoreboard.WithLogger(logger),
	)
	if err != nil {
		return fmt.Errorf("build scoreboard: %w", err)
	}
	logger.Info("scoreboard built",
		"probe_chance", cfg.Failure.Scoring.ProbeChance,
		"decay_interval_ms", cfg.Failure.Scoring.DecayInterval.Duration().Milliseconds(),
		"cascade_ttl_ms", cfg.Failure.Scoring.CascadeTTL.Duration().Milliseconds(),
		"debounce_window_ms", cfg.Failure.Scoring.DebounceWindow.Duration().Milliseconds(),
	)

	sb.Start(ctx)
	defer sb.Stop()

	detector := failure.NewStatusDetector(cfg.Failure.Status)
	httpSrv, err := listener.NewHTTP(cfg.Listener.HTTP, sb, detector, cfg.Failure.MaxRetriesPerRequest, logger)
	if err != nil {
		return fmt.Errorf("build http listener: %w", err)
	}
	socksSrv, err := listener.NewSOCKS(cfg.Listener.SOCKS, sb, logger)
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

// expandUpstreamPools turns each [[upstream_pool]] block into a slice of
// synthetic upstream entries by calling the relevant provider's expander
// (only "mullvad" is implemented for Phase 6). Failures during expansion
// are fatal at startup so the operator notices a broken upstream_pool
// before traffic is sent. Phase 7 will move the runtime refresh into the
// signal-handler hot-reload path.
func expandUpstreamPools(ctx context.Context, blocks []config.UpstreamPoolConfig, logger *slog.Logger) ([]config.UpstreamConfig, error) {
	var out []config.UpstreamConfig
	for i, block := range blocks {
		switch block.Provider {
		case config.UpstreamPoolMullvad:
			expanded, err := expandMullvadPool(ctx, block, logger)
			if err != nil {
				return nil, fmt.Errorf("upstream_pool[%d] (id_prefix=%q): %w", i, block.IDPrefix, err)
			}
			logger.Info("upstream_pool expanded",
				"id_prefix", block.IDPrefix,
				"provider", block.Provider,
				"countries", block.Countries,
				"upstreams", len(expanded),
			)
			out = append(out, expanded...)
		default:
			return nil, fmt.Errorf("upstream_pool[%d]: provider %q is not implemented", i, block.Provider)
		}
	}
	return out, nil
}

func expandMullvadPool(ctx context.Context, block config.UpstreamPoolConfig, logger *slog.Logger) ([]config.UpstreamConfig, error) {
	client := mullvad.NewClient()
	if block.CachePath != "" {
		client.Cache = &mullvad.Cache{Path: block.CachePath}
	}
	exp, err := mullvad.NewExpander(mullvad.ExpanderConfig{
		IDPrefix:        block.IDPrefix,
		Priority:        block.PriorityValue(),
		Countries:       block.Countries,
		IncludeInactive: block.IncludeInactive,
		Refresh:         block.RefreshDuration(),
	}, client, logger.With("component", "mullvad-expander", "id_prefix", block.IDPrefix))
	if err != nil {
		return nil, err
	}
	return exp.Snapshot(ctx)
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
