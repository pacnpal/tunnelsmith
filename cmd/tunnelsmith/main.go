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
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/metrics"
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

	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)

	dialTimeout := time.Duration(cfg.Failure.TimeoutMS) * time.Millisecond
	expandedPools, expanders, err := expandUpstreamPools(ctx, cfg.UpstreamPools, logger)
	if err != nil {
		return fmt.Errorf("expand upstream pools: %w", err)
	}
	allUpstreams := make([]config.UpstreamConfig, 0, len(cfg.Upstreams)+len(expandedPools))
	allUpstreams = append(allUpstreams, cfg.Upstreams...)
	allUpstreams = append(allUpstreams, expandedPools...)
	if len(allUpstreams) == 0 {
		return errors.New("no upstreams available after expanding [[upstream_pool]] (check provider connectivity and country filters)")
	}
	if err := assertUniqueUpstreamIDs(allUpstreams); err != nil {
		return err
	}
	if err := assertRulePreferIDs(cfg.Rules, allUpstreams); err != nil {
		return err
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

	metricsRegistry := metrics.New()
	metricsRegistry.SetUpstreamPoolSize(pool.Len())

	sb, err := scoreboard.New(pool, scoreboard.FromConfig(cfg.Failure.Scoring, cfg.Failure.Status),
		scoreboard.WithLogger(logger),
		scoreboard.WithMetrics(metricsRegistry),
	)
	if err != nil {
		return fmt.Errorf("build scoreboard: %w", err)
	}
	if cfg.Cache.PersistPath != "" {
		if err := sb.LoadSnapshot(cfg.Cache.PersistPath); err != nil {
			logger.Warn("scoreboard snapshot load failed, starting from empty state",
				"path", cfg.Cache.PersistPath,
				"err", err,
			)
		} else {
			logger.Info("scoreboard snapshot loaded",
				"path", cfg.Cache.PersistPath,
				"entries", sb.EntriesCount(),
			)
		}
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
	httpSrv, err := listener.NewHTTP(cfg.Listener.HTTP, sb, detector, cfg.Failure.MaxRetriesPerRequest, logger,
		listener.WithHTTPMetrics(metricsRegistry),
	)
	if err != nil {
		return fmt.Errorf("build http listener: %w", err)
	}
	socksSrv, err := listener.NewSOCKS(cfg.Listener.SOCKS, sb, logger,
		listener.WithSOCKSMetrics(metricsRegistry),
	)
	if err != nil {
		return fmt.Errorf("build socks listener: %w", err)
	}
	var metricsSrv *metrics.Server
	if cfg.Metrics.Bind != "" {
		metricsSrv = metrics.NewServer(cfg.Metrics.Bind, metricsRegistry, logger)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpSrv.Serve(gctx) })
	g.Go(func() error { return socksSrv.Serve(gctx) })
	if metricsSrv != nil {
		g.Go(func() error { return metricsSrv.Serve(gctx) })
	}
	if cfg.Cache.PersistPath != "" {
		persistLoop := scoreboard.NewPersistenceLoop(sb, scoreboard.PersistenceConfig{
			Path:     cfg.Cache.PersistPath,
			Interval: cfg.Cache.PersistInterval.Duration(),
		}, logger, metricsRegistry)
		g.Go(func() error { return persistLoop.Run(gctx) })
	}
	var gaugeRefresh *scoreboardGaugeRefresher
	if metricsSrv != nil {
		// Refresh the scoreboard-shape gauges on every metrics scrape
		// would be more accurate, but Prometheus client_golang does not
		// expose a "before-collect" hook for arbitrary gauges. A light
		// background ticker mirrors the scoreboard state into the gauge
		// vectors at a reasonable cadence.
		gaugeRefresh = newScoreboardGaugeRefresher(sb, metricsRegistry, pool.IDs())
		g.Go(func() error { return gaugeRefresh.run(gctx) })
	}

	// SIGHUP hot-reload: re-read the config file, validate, then apply
	// the subset that does not require a restart. Listener bindings,
	// the decay interval, [[upstream_pool]] refresh interval, and the
	// persistence path stay frozen at startup.
	reloader := &reloader{
		path:           *configPath,
		logger:         logger,
		sb:             sb,
		httpSrv:        httpSrv,
		registry:       metricsRegistry,
		gauges:         gaugeRefresh,
		runningHasPool: len(cfg.UpstreamPools) > 0,
	}
	g.Go(func() error {
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-hupCh:
				reloader.reload(gctx)
			}
		}
	})
	for _, run := range expanders {
		run := run
		g.Go(func() error { return run(gctx) })
	}

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
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics shutdown error", "err", err)
		}
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

// assertRulePreferIDs rejects [[rule]] entries whose prefer list names an
// upstream id that does not exist in the merged upstream set. The set is
// only known after [[upstream_pool]] expansion at startup, so this check
// runs here rather than in config.Validate. config.Validate still does the
// equivalent check itself when no pool blocks are configured (so a config
// with only static [[upstream]] entries fails fast at parse time).
func assertRulePreferIDs(rules []config.RuleConfig, upstreams []config.UpstreamConfig) error {
	ids := make(map[string]struct{}, len(upstreams))
	for _, u := range upstreams {
		ids[u.ID] = struct{}{}
	}
	var errs []error
	for i, r := range rules {
		for _, id := range r.Prefer {
			if _, ok := ids[id]; !ok {
				errs = append(errs, fmt.Errorf("rule[%d] (host_glob=%q): prefer references unknown upstream id %q (after [[upstream_pool]] expansion)", i, r.HostGlob, id))
			}
		}
	}
	return errors.Join(errs...)
}

// assertUniqueUpstreamIDs rejects a merged upstream list that contains
// duplicate ids. The scoreboard and per-host bookkeeping key off
// upstream_id, so two entries sharing one id would collapse into a single
// logical upstream and produce incorrect routing and scoring. The error
// message names every colliding id so the operator can point at the right
// pool block or [[upstream]] entry.
func assertUniqueUpstreamIDs(upstreams []config.UpstreamConfig) error {
	seen := make(map[string]int, len(upstreams))
	var duplicates []string
	for i, u := range upstreams {
		if prev, ok := seen[u.ID]; ok {
			duplicates = append(duplicates, fmt.Sprintf("%q (entries %d and %d)", u.ID, prev, i))
		} else {
			seen[u.ID] = i
		}
	}
	if len(duplicates) > 0 {
		return fmt.Errorf("duplicate upstream ids after expanding [[upstream_pool]]: %s", strings.Join(duplicates, ", "))
	}
	return nil
}

// expandUpstreamPools turns each [[upstream_pool]] block into a slice of
// synthetic upstream entries by calling the relevant provider's expander.
// Only "mullvad" is implemented for Phase 6.
//
// Returns two parallel sets of values: the resolved upstream list (used
// once at startup to build the priority pool), and a slice of refresh
// callbacks the caller is expected to fire from the signal-context
// errgroup. Each callback drives the expander's periodic refresh ticker,
// which currently logs the diff between snapshots; Phase 7's hot-reload
// path will swap the live pool in on each tick.
//
// Failures during the initial expansion are fatal at startup so the
// operator notices a broken upstream_pool before traffic is sent.
func expandUpstreamPools(ctx context.Context, blocks []config.UpstreamPoolConfig, logger *slog.Logger) ([]config.UpstreamConfig, []func(context.Context) error, error) {
	var out []config.UpstreamConfig
	var runs []func(context.Context) error
	for i, block := range blocks {
		switch block.Provider {
		case config.UpstreamPoolMullvad:
			expanded, run, err := expandMullvadPool(ctx, block, logger)
			if err != nil {
				return nil, nil, fmt.Errorf("upstream_pool[%d] (id_prefix=%q): %w", i, block.IDPrefix, err)
			}
			logger.Info("upstream_pool expanded",
				"id_prefix", block.IDPrefix,
				"provider", block.Provider,
				"countries", block.Countries,
				"upstreams", len(expanded),
			)
			out = append(out, expanded...)
			runs = append(runs, run)
		default:
			return nil, nil, fmt.Errorf("upstream_pool[%d]: provider %q is not implemented", i, block.Provider)
		}
	}
	return out, runs, nil
}

func expandMullvadPool(ctx context.Context, block config.UpstreamPoolConfig, logger *slog.Logger) ([]config.UpstreamConfig, func(context.Context) error, error) {
	expLogger := logger.With("component", "mullvad-expander", "id_prefix", block.IDPrefix)
	client := mullvad.NewClient()
	client.Logger = expLogger
	if block.CachePath != "" {
		client.Cache = &mullvad.Cache{Path: block.CachePath}
	}
	exp, err := mullvad.NewExpander(mullvad.ExpanderConfig{
		IDPrefix:        block.IDPrefix,
		Priority:        block.PriorityValue(),
		Countries:       block.Countries,
		IncludeInactive: block.IncludeInactive,
		Refresh:         block.RefreshDuration(),
	}, client, expLogger)
	if err != nil {
		return nil, nil, err
	}
	initial, err := exp.Snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Phase 6 only logs diffs; Phase 7 will rewire this callback to
	// hot-swap the live pool on every tick.
	run := func(ctx context.Context) error {
		return exp.RunRefresh(ctx, initial, func(prev, next []config.UpstreamConfig) {
			added, removed := diffUpstreams(prev, next)
			if len(added) == 0 && len(removed) == 0 {
				expLogger.Debug("upstream_pool refresh: no change", "upstreams", len(next))
				return
			}
			// INFO carries counts plus a small sample so a normal log
			// line stays small even when Mullvad rolls out or removes
			// dozens of relays at once. Operators who need the full
			// diff can drop the level to DEBUG.
			expLogger.Info("upstream_pool refresh",
				"upstreams", len(next),
				"added_count", len(added),
				"removed_count", len(removed),
				"added_sample", truncateIDs(added, refreshLogSampleSize),
				"removed_sample", truncateIDs(removed, refreshLogSampleSize),
			)
			expLogger.Debug("upstream_pool refresh full diff",
				"added", added,
				"removed", removed,
			)
		})
	}
	return initial, run, nil
}

// refreshLogSampleSize bounds the number of upstream ids included in the
// INFO refresh log line. The full diff is still emitted at DEBUG.
const refreshLogSampleSize = 5

// truncateIDs returns at most n entries from ids unchanged, plus an
// ellipsis sentinel listing how many were elided so the log line is
// self-describing without leaking the full slice.
func truncateIDs(ids []string, n int) []string {
	if len(ids) <= n {
		return ids
	}
	out := make([]string, 0, n+1)
	out = append(out, ids[:n]...)
	out = append(out, fmt.Sprintf("... and %d more", len(ids)-n))
	return out
}

// diffUpstreams returns the ids present in next but not in prev, and the
// ids present in prev but not in next. Order is stable (input order).
func diffUpstreams(prev, next []config.UpstreamConfig) (added, removed []string) {
	prevSet := make(map[string]struct{}, len(prev))
	for _, u := range prev {
		prevSet[u.ID] = struct{}{}
	}
	nextSet := make(map[string]struct{}, len(next))
	for _, u := range next {
		nextSet[u.ID] = struct{}{}
	}
	for _, u := range next {
		if _, ok := prevSet[u.ID]; !ok {
			added = append(added, u.ID)
		}
	}
	for _, u := range prev {
		if _, ok := nextSet[u.ID]; !ok {
			removed = append(removed, u.ID)
		}
	}
	return added, removed
}

// reloader drives the SIGHUP hot-reload path. It re-reads the config file,
// validates it, and applies the subset of fields the running binary can
// change in place. Listener bindings, decay interval, persist path, and
// the [[upstream_pool]] refresh schedule are frozen at startup.
//
// runningHasPool says whether the running instance was built with at
// least one [[upstream_pool]] block. When true the reloader leaves the
// upstream pool alone (pool expansion is restart-frozen for v1) and
// only swaps scoring tunings, the status detector, and the retry cap;
// the alternative would silently drop pool-expanded upstreams from the
// live pool on every SIGHUP, which is worse than not reloading them.
type reloader struct {
	path           string
	logger         *slog.Logger
	sb             *scoreboard.Scoreboard
	httpSrv        *listener.HTTPServer
	registry       *metrics.Registry
	gauges         *scoreboardGaugeRefresher
	runningHasPool bool
}

func (r *reloader) reload(ctx context.Context) {
	r.logger.Info("config reload requested", "path", r.path)
	newCfg, err := config.Load(r.path)
	if err != nil {
		r.logger.Warn("config reload failed (keeping current config)", "err", err)
		r.registry.ObserveConfigReload(metrics.ResultError)
		return
	}
	if len(newCfg.UnknownKeys) > 0 {
		r.logger.Warn("reloaded config has unknown keys; check for typos",
			"path", r.path,
			"keys", newCfg.UnknownKeys,
		)
	}

	// Always hot-reload scoring tunings and the status detector. They
	// are independent of the pool shape.
	r.sb.Reload(scoreboard.FromConfig(newCfg.Failure.Scoring, newCfg.Failure.Status))
	r.httpSrv.Reload(failure.NewStatusDetector(newCfg.Failure.Status), newCfg.Failure.MaxRetriesPerRequest)

	switch {
	case r.runningHasPool || len(newCfg.UpstreamPools) > 0:
		// The running config or the new config carries an
		// [[upstream_pool]] block. Pool expansion runs on its own
		// refresh ticker per ADR-006-equivalent reasoning: rebuilding
		// the priority pool here would either drop the pool-expanded
		// upstreams (if we used only newCfg.Upstreams) or duplicate
		// them (if we mixed snapshots). Skip the swap entirely and let
		// the user restart for pool-shape changes.
		r.logger.Warn("upstream pool not hot-reloaded; [[upstream_pool]] in play, restart to apply pool shape changes",
			"running_has_pool", r.runningHasPool,
			"new_has_pool", len(newCfg.UpstreamPools) > 0,
		)
	default:
		if err := r.swapStaticPool(newCfg); err != nil {
			r.logger.Warn("config reload failed (pool swap)", "err", err)
			r.registry.ObserveConfigReload(metrics.ResultError)
			return
		}
	}

	r.registry.ObserveConfigReload(metrics.ResultSuccess)
	r.logger.Info("config reloaded",
		"path", r.path,
		"static_upstreams", len(newCfg.Upstreams),
		"pools", len(newCfg.UpstreamPools),
		"retry_cap", newCfg.Failure.MaxRetriesPerRequest,
	)
	_ = ctx // reserved for future cancel-aware reloads
}

// swapStaticPool builds a fresh priority pool from newCfg.Upstreams and
// installs it on the scoreboard plus drops every cached HTTP transport.
// Cached transports pin the previous Upstream object via their
// DialContext closure, so a same-id reload that changes addr/kind would
// otherwise route through the stale destination; dropping the whole
// cache is simpler than tracking per-upstream identity.
func (r *reloader) swapStaticPool(newCfg *config.Config) error {
	dialTimeout := time.Duration(newCfg.Failure.TimeoutMS) * time.Millisecond
	entries := make([]upstream.PoolEntry, 0, len(newCfg.Upstreams))
	for _, uc := range newCfg.Upstreams {
		up, buildErr := upstream.New(uc, dialTimeout)
		if buildErr != nil {
			return fmt.Errorf("build upstream %q: %w", uc.ID, buildErr)
		}
		entries = append(entries, upstream.PoolEntry{Up: up, Priority: uc.PriorityValue()})
	}
	if len(entries) == 0 {
		return errors.New("no static upstreams in reloaded config")
	}
	newPool, err := upstream.NewPool(entries, newCfg.Failure.MaxRetriesPerRequest, r.logger)
	if err != nil {
		return fmt.Errorf("build pool: %w", err)
	}
	if err := r.sb.ReplacePool(newPool); err != nil {
		return fmt.Errorf("scoreboard ReplacePool: %w", err)
	}

	newIDs := newPool.IDs()
	// Pass an empty keep set so every cached transport is dropped. The
	// new pool's Upstream objects need their own freshly-pinned
	// transports; a same-id transport from the old pool would still
	// dial through the previous Upstream's closure.
	r.httpSrv.CloseTransportsExcept(map[string]struct{}{})

	if r.gauges != nil {
		r.gauges.setPoolIDs(newIDs)
	}

	r.registry.SetUpstreamPoolSize(newPool.Len())
	return nil
}

// scoreboardGaugeRefresher periodically copies the scoreboard's
// shape-only state (total entries, per-upstream cooled hosts, cascade
// active count) into the metrics registry. The refresh interval is
// independent of the persistence loop so the metrics gauges stay
// reasonably fresh even when persistence is disabled.
type scoreboardGaugeRefresher struct {
	sb       *scoreboard.Scoreboard
	registry *metrics.Registry

	mu       sync.RWMutex
	poolIDs  []string
	interval time.Duration
}

func newScoreboardGaugeRefresher(sb *scoreboard.Scoreboard, reg *metrics.Registry, poolIDs []string) *scoreboardGaugeRefresher {
	return &scoreboardGaugeRefresher{sb: sb, registry: reg, poolIDs: poolIDs, interval: 5 * time.Second}
}

// setPoolIDs swaps the upstream id list the gauges reset against. Called
// from the SIGHUP hot-reload path after the upstream pool changes so a
// scrape after the reload reports the right set of upstream_id labels.
func (r *scoreboardGaugeRefresher) setPoolIDs(ids []string) {
	r.mu.Lock()
	r.poolIDs = ids
	r.mu.Unlock()
}

func (r *scoreboardGaugeRefresher) run(ctx context.Context) error {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	r.tick()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.tick()
		}
	}
}

func (r *scoreboardGaugeRefresher) tick() {
	r.mu.RLock()
	ids := r.poolIDs
	r.mu.RUnlock()
	r.registry.SetScoreboardSnapshot(
		r.sb.EntriesCount(),
		r.sb.CooledHostsByUpstream(),
		r.sb.CascadeActiveCount(),
		ids,
	)
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
