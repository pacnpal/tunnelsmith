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
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/control"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/listener"
	"github.com/pacnpal/tunnelsmith/internal/metrics"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/ui"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
	"github.com/pacnpal/tunnelsmith/internal/upstream/provider"
	_ "github.com/pacnpal/tunnelsmith/internal/upstream/providers" // wires every supported [[upstream_pool]] provider into the registry and installs config.SetProviderValidator
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
	expandedPools, poolBlocks, err := expandUpstreamPools(ctx, cfg.UpstreamPools, logger)
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
	if len(cfg.Failure.BodyRegex) > 0 {
		// Phase 8 moved body-regex detection to per-rule BodyRegex.
		// The top-level field is parsed for forward compatibility but
		// does nothing at runtime. Surface a single startup warning so
		// operators know to move their patterns into a [[rule]] block.
		logger.Warn("failure.body_regex is deprecated; move patterns into [[rule]].body_regex (Phase 8)",
			"top_level_patterns", len(cfg.Failure.BodyRegex),
		)
	}
	rules, err := upstream.NewRuleSet(cfg.Rules)
	if err != nil {
		return fmt.Errorf("compile [[rule]] entries: %w", err)
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

	sb, err := scoreboard.New(pool, scoreboard.FromConfig(cfg.Failure.Scoring, cfg.Failure.Status, cfg.Failure.ConnectionRefused),
		scoreboard.WithLogger(logger),
		scoreboard.WithMetrics(metricsRegistry),
		scoreboard.WithRules(rules),
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
		listener.WithHTTPRules(rules),
		listener.WithHTTPBodyBufferKB(cfg.Failure.BodyBufferKB),
		listener.WithHTTPConnectionRefused(cfg.Failure.ConnectionRefused),
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
	var uiSrv *ui.Server
	if cfg.UI.Bind != "" {
		uiSrv = ui.NewServer(cfg.UI.Bind, sb, logger)
	}
	var controlSrv *control.Server
	if cfg.Control.Bind != "" {
		// Phase 12: build the initial token set from inline + file. A
		// missing auth_tokens_file at startup is warned and treated as
		// empty (ADR-007) so a typo'd path does not break boot; SIGHUP
		// can correct it later. Inline tokens have already passed
		// config.Validate.
		initialTokens, missingFile, err := buildControlTokens(cfg.Control)
		if err != nil {
			return fmt.Errorf("control auth tokens: %w", err)
		}
		if missingFile {
			logger.Warn("control.auth_tokens_file missing at startup; file portion treated as empty (SIGHUP will retry)",
				"path", cfg.Control.AuthTokensFile)
		}
		if len(initialTokens) > 0 {
			logger.Info("control auth enabled at startup",
				"tokens", len(initialTokens),
				"gate_healthz", cfg.Control.GateHealthz, // wired at NewServer; restart-only
			)
		}
		controlSrv = control.NewServer(cfg.Control.Bind, sb, metricsRegistry, control.ServerOptions{
			Tokens:      initialTokens,
			GateHealthz: cfg.Control.GateHealthz,
			Providers:   buildProviderRegistry(poolBlocks),
		}, logger)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpSrv.Serve(gctx) })
	g.Go(func() error { return socksSrv.Serve(gctx) })
	if metricsSrv != nil {
		g.Go(func() error { return metricsSrv.Serve(gctx) })
	}
	if uiSrv != nil {
		g.Go(func() error { return uiSrv.Serve(gctx) })
	}
	if controlSrv != nil {
		g.Go(func() error { return controlSrv.Serve(gctx) })
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
		controlSrv:     controlSrv,
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
	// Build the pool composer now that sb / httpSrv / gauges / registry
	// all exist, then start the refresh-tick runners with composer
	// attached so every successful diff hot-swaps the running pool. The
	// composer captures the startup static [[upstream]] slice and each
	// block's initial expansion, so the merged view at swap time is
	// deterministic across blocks.
	composer := newPoolComposer(
		cfg.Upstreams,
		poolBlocks,
		sb,
		httpSrv,
		gaugeRefresh,
		metricsRegistry,
		logger,
		cfg.Failure.MaxRetriesPerRequest,
		dialTimeout,
	)
	for _, pb := range poolBlocks {
		pb := pb
		g.Go(func() error { return pb.runRefresh(gctx, composer) })
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
	if uiSrv != nil {
		if err := uiSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("ui shutdown error", "err", err)
		}
	}
	if controlSrv != nil {
		if err := controlSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("control shutdown error", "err", err)
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

// buildControlTokens merges the inline [control].auth_tokens list with
// tokens loaded from [control].auth_tokens_file (Phase 12). The
// returned missingFile flag is true when the configured file was set
// but did not exist on disk — callers handle that case differently and
// log their own context-appropriate message (startup warns and treats
// the file portion as empty per ADR-007; the SIGHUP reload path
// preserves the current live token set without rotating so a
// logrotate-style brief disappearance does not silently disable auth).
// The function itself does not log: each call site owns the policy and
// the operator-facing message that fits its phase.
// The returned slice is the dedup'd union of inline + file, inline-first.
func buildControlTokens(cfg config.ControlConfig) (tokens []string, missingFile bool, err error) {
	var fromFile []string
	if cfg.AuthTokensFile != "" {
		loaded, lerr := control.LoadTokensFile(cfg.AuthTokensFile)
		switch {
		case lerr == nil:
			fromFile = loaded
		case errors.Is(lerr, os.ErrNotExist):
			missingFile = true
		default:
			return nil, false, lerr
		}
	}
	tokens = control.MergeTokens(cfg.AuthTokens, fromFile)
	return tokens, missingFile, nil
}

// poolComposer rebuilds the running priority pool whenever an
// [[upstream_pool]] expander's refresh tick produces a non-empty diff
// (Phase 11.1). It captures the static [[upstream]] slice and the
// per-block expansion state at startup, holds a reference to the
// scoreboard / HTTP listener / gauge refresher / metrics registry, and
// applies the swap atomically through Scoreboard.ReplacePool. The
// SIGHUP reload path stays restart-only for pool-shape changes; the
// composer only handles refresh-tick churn.
type poolComposer struct {
	mu sync.Mutex
	// static is the [[upstream]] slice as captured at startup. It does
	// not change across the lifetime of the binary; SIGHUP changes to
	// [[upstream]] when an [[upstream_pool]] block is present require a
	// restart by design (see reloader.reload).
	static []config.UpstreamConfig
	// blocks tracks each [[upstream_pool]] block's current expansion in
	// declaration order so the merged slice is deterministic.
	blocks []composerBlock

	sb       *scoreboard.Scoreboard
	httpSrv  *listener.HTTPServer
	gauges   *scoreboardGaugeRefresher
	registry *metrics.Registry
	logger   *slog.Logger

	// retryCap and dialTimeout are captured at startup so a refresh-tick
	// swap never silently changes the values an operator configured.
	retryCap    int
	dialTimeout time.Duration
}

// composerBlock holds the live expansion for one [[upstream_pool]]
// block. Updated on every successful refresh tick via poolComposer.Update.
type composerBlock struct {
	idPrefix string
	current  []config.UpstreamConfig
}

// newPoolComposer wires every dependency the swap path needs and seeds
// each block's current expansion with what the startup pool was built
// from. Call after sb / httpSrv / gauges / registry exist so the first
// refresh tick has a complete dependency graph.
func newPoolComposer(
	staticUpstreams []config.UpstreamConfig,
	blocks []*poolBlock,
	sb *scoreboard.Scoreboard,
	httpSrv *listener.HTTPServer,
	gauges *scoreboardGaugeRefresher,
	registry *metrics.Registry,
	logger *slog.Logger,
	retryCap int,
	dialTimeout time.Duration,
) *poolComposer {
	cb := make([]composerBlock, 0, len(blocks))
	for _, b := range blocks {
		// slices.Clone preserves the nil-vs-empty distinction so a
		// stably-empty Mullvad snapshot ([]T{}, non-nil) doesn't
		// collapse into nil here and then mismatch every refresh tick
		// under reflect.DeepEqual.
		cb = append(cb, composerBlock{
			idPrefix: b.idPrefix,
			current:  slices.Clone(b.initial),
		})
	}
	return &poolComposer{
		static:      slices.Clone(staticUpstreams),
		blocks:      cb,
		sb:          sb,
		httpSrv:     httpSrv,
		gauges:      gauges,
		registry:    registry,
		logger:      logger,
		retryCap:    retryCap,
		dialTimeout: dialTimeout,
	}
}

// Update is invoked by a poolBlock's refresh callback on every fresh
// snapshot. It compares next to the composer's last successfully-
// installed expansion for the block; when they match it returns
// (applied=false, nil) without rebuilding the pool. Otherwise it
// rebuilds the merged priority pool from static + every block's
// latest applied expansion (substituting next for the matching
// block), installs the new pool on the scoreboard, commits next into
// the cache, and returns (applied=true, nil).
//
// Failures (build error in upstream.New, NewPool, or ReplacePool)
// leave both the running pool and the composer's cache untouched,
// increment pool_hotswap_total{result="error"}, and return
// (applied=false, err). Returning err — combined with the cache not
// advancing — means a subsequent Update with the same next will
// re-attempt the swap on the next refresh tick rather than going
// silent if the Mullvad expander's prev/next happens to be stable
// post-failure.
//
// The lock is held for the entire build-and-swap so two concurrent
// Updates from different blocks serialize end-to-end. Refresh ticks
// fire on hour-scale schedules; lock granularity is a non-issue here
// and the simpler ordering is worth more than the contention saved.
func (c *poolComposer) Update(idPrefix string, next []config.UpstreamConfig) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := -1
	for i := range c.blocks {
		if c.blocks[i].idPrefix == idPrefix {
			idx = i
			break
		}
	}
	if idx == -1 {
		c.registry.ObservePoolHotSwap(metrics.ResultError)
		return false, fmt.Errorf("poolComposer: no block registered for id_prefix %q", idPrefix)
	}

	// No-op short-circuit: if next equals the last successfully-
	// installed expansion for this block, the running pool is already
	// what next would build. Skip without touching the metric — only
	// real swap attempts (success or failure) tick pool_hotswap_total.
	if reflect.DeepEqual(c.blocks[idx].current, next) {
		return false, nil
	}

	// Build the merged view substituting next for the matching block
	// without mutating cached state. If anything below this point fails
	// the cache stays at the last-installed snapshot, so a later Update
	// for next on the same or another block will re-attempt the swap.
	totalLen := len(c.static) + len(next)
	for i, b := range c.blocks {
		if i == idx {
			continue
		}
		totalLen += len(b.current)
	}
	merged := make([]config.UpstreamConfig, 0, totalLen)
	merged = append(merged, c.static...)
	for i, b := range c.blocks {
		if i == idx {
			merged = append(merged, next...)
		} else {
			merged = append(merged, b.current...)
		}
	}

	// Re-assert the uniqueness invariant that startup enforces. If a
	// later relay snapshot ever produces an id that collides with a
	// static [[upstream]] or another block's expansion, the scoreboard
	// would key multiple upstreams under the same upstream_id and
	// silently scramble routing/scoring. Treat duplicates as a swap
	// error and leave the running pool untouched.
	if err := assertUniqueUpstreamIDs(merged); err != nil {
		c.registry.ObservePoolHotSwap(metrics.ResultError)
		return false, err
	}

	entries := make([]upstream.PoolEntry, 0, len(merged))
	for _, uc := range merged {
		up, err := upstream.New(uc, c.dialTimeout)
		if err != nil {
			c.registry.ObservePoolHotSwap(metrics.ResultError)
			return false, fmt.Errorf("build upstream %q: %w", uc.ID, err)
		}
		entries = append(entries, upstream.PoolEntry{Up: up, Priority: uc.PriorityValue()})
	}
	newPool, err := upstream.NewPool(entries, c.retryCap, c.logger)
	if err != nil {
		c.registry.ObservePoolHotSwap(metrics.ResultError)
		return false, fmt.Errorf("build pool: %w", err)
	}
	if err := c.sb.ReplacePool(newPool); err != nil {
		c.registry.ObservePoolHotSwap(metrics.ResultError)
		return false, fmt.Errorf("scoreboard ReplacePool: %w", err)
	}

	// Swap succeeded: commit the new expansion to the cache so the next
	// Update merges from the actually-installed view. slices.Clone (not
	// append([]T(nil), …)) so an empty-but-non-nil next stays empty-but-
	// non-nil — otherwise the DeepEqual short-circuit above would fail
	// on every subsequent tick when the snapshot is stably empty.
	c.blocks[idx].current = slices.Clone(next)

	// Drop cached HTTP transports so a new pool's Upstream objects
	// build fresh DialContext closures rather than routing through the
	// previous Upstream's pinned dialer. swapStaticPool does the same.
	if c.httpSrv != nil {
		c.httpSrv.CloseTransportsExcept(map[string]struct{}{})
	}
	if c.gauges != nil {
		c.gauges.setPoolIDs(newPool.IDs())
	}
	c.registry.SetUpstreamPoolSize(newPool.Len())
	c.registry.ObservePoolHotSwap(metrics.ResultSuccess)
	return true, nil
}

// poolBlock carries the live state for one [[upstream_pool]] block so a
// poolComposer can drive its refresh ticker and hot-swap the running
// priority pool when the expansion changes. The expander is kept for
// its RunRefresh loop; the initial expansion seeds the diff comparison
// and is also returned to the caller for the startup pool. The api
// pointer is non-nil only when the provider exposed one (Mullvad has
// no vendor API, so its api is nil).
type poolBlock struct {
	idPrefix string
	provider string
	exp      provider.Expander
	api      provider.API // nil when the provider returned ErrAPINotSupported
	initial  []config.UpstreamConfig
	logger   *slog.Logger
}

// expandUpstreamPools turns each [[upstream_pool]] block into a slice of
// synthetic upstream entries by calling the configured provider's
// expander. The provider is resolved through the registry built by
// internal/upstream/providers's init() functions; an unknown provider
// is rejected here even though config.Validate already covers the case.
//
// Returns the resolved initial upstream list (used once at startup to
// build the priority pool) and a parallel slice of *poolBlock values
// the caller wires into a poolComposer to drive refresh-tick hot-swaps.
// Order in both return values matches the declaration order of
// [[upstream_pool]] in the config.
//
// Failures during the initial expansion are fatal at startup so the
// operator notices a broken upstream_pool before traffic is sent.
func expandUpstreamPools(ctx context.Context, blocks []config.UpstreamPoolConfig, logger *slog.Logger) ([]config.UpstreamConfig, []*poolBlock, error) {
	var out []config.UpstreamConfig
	var poolBlocks []*poolBlock
	for i, block := range blocks {
		pb, err := expandPool(ctx, block, logger)
		if err != nil {
			return nil, nil, fmt.Errorf("upstream_pool[%d] (id_prefix=%q): %w", i, block.IDPrefix, err)
		}
		logger.Info("upstream_pool expanded",
			"id_prefix", block.IDPrefix,
			"provider", block.Provider,
			"upstreams", len(pb.initial),
			"has_api", pb.api != nil,
		)
		out = append(out, pb.initial...)
		poolBlocks = append(poolBlocks, pb)
	}
	return out, poolBlocks, nil
}

// buildProviderRegistry turns the startup poolBlocks into the binding
// slice the control listener serves under /v1/providers. Blocks whose
// provider has no API still appear (with HasAPI=false) so an operator
// running GET /v1/providers can confirm the block is wired even though
// refresh would return 501.
func buildProviderRegistry(blocks []*poolBlock) *control.ProviderRegistry {
	bindings := make([]control.ProviderAPIBinding, 0, len(blocks))
	for _, b := range blocks {
		bindings = append(bindings, control.ProviderAPIBinding{
			IDPrefix: b.idPrefix,
			Provider: b.provider,
			API:      b.api,
		})
	}
	return control.NewProviderRegistry(bindings)
}

func expandPool(ctx context.Context, block config.UpstreamPoolConfig, logger *slog.Logger) (*poolBlock, error) {
	prov, ok := provider.Default().Lookup(string(block.Provider))
	if !ok {
		return nil, fmt.Errorf("provider %q is not registered (built-in providers: %v)", block.Provider, provider.Default().Names())
	}
	expLogger := logger.With("provider", block.Provider, "id_prefix", block.IDPrefix)
	exp, err := prov.BuildExpander(block, expLogger)
	if err != nil {
		return nil, fmt.Errorf("build expander: %w", err)
	}
	api, apiErr := prov.BuildAPI(block, expLogger)
	if apiErr != nil && !errors.Is(apiErr, provider.ErrAPINotSupported) {
		return nil, fmt.Errorf("build api: %w", apiErr)
	}
	initial, err := exp.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return &poolBlock{
		idPrefix: block.IDPrefix,
		provider: string(block.Provider),
		exp:      exp,
		api:      api,
		initial:  initial,
		logger:   expLogger,
	}, nil
}

// runRefresh drives one block's refresh ticker. The callback logs the
// snapshot-level diff (prev vs. next, the existing INFO/DEBUG
// behaviour) and, when composer is non-nil, hands every snapshot to
// composer.Update. The composer is the authority on whether a swap is
// needed: if next equals the last-applied expansion, Update returns
// (applied=false, nil) and we stay quiet. If a previous swap failed,
// the composer's cache is still at the prior good snapshot, so the
// next call to Update with the same next re-attempts the swap — the
// retry does not depend on the Mullvad expander's prev advancing.
// composer may be nil during construction or for tests that only want
// to observe the log path.
func (b *poolBlock) runRefresh(ctx context.Context, composer *poolComposer) error {
	return b.exp.RunRefresh(ctx, b.initial, func(prev, next []config.UpstreamConfig) {
		added, removed := diffUpstreams(prev, next)
		if len(added) == 0 && len(removed) == 0 {
			b.logger.Debug("upstream_pool refresh: snapshot unchanged",
				"upstreams", len(next))
		} else {
			// INFO carries counts plus a small sample so a normal log
			// line stays small even when Mullvad rolls out or removes
			// dozens of relays at once. Operators who need the full
			// diff can drop the level to DEBUG.
			b.logger.Info("upstream_pool refresh",
				"upstreams", len(next),
				"added_count", len(added),
				"removed_count", len(removed),
				"added_sample", truncateIDs(added, refreshLogSampleSize),
				"removed_sample", truncateIDs(removed, refreshLogSampleSize),
			)
			b.logger.Debug("upstream_pool refresh full diff",
				"added", added,
				"removed", removed,
			)
		}
		if composer == nil {
			return
		}
		applied, err := composer.Update(b.idPrefix, next)
		if err != nil {
			b.logger.Warn("upstream_pool hot-swap failed; running pool unchanged",
				"err", err,
			)
			return
		}
		if applied {
			b.logger.Info("upstream_pool hot-swap applied",
				"upstreams", len(next),
			)
		}
	})
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
	controlSrv     *control.Server // nil when control.bind disabled
	registry       *metrics.Registry
	gauges         *scoreboardGaugeRefresher
	runningHasPool bool
}

// projectedUpstreamIDs returns the upstream id set the reload pass
// will end up with, used to validate rule.Prefer entries before any
// partial state is installed. Pool-shape changes are restart-frozen
// for v1, so when either the running config or the new config carries
// an [[upstream_pool]] block, the projection is the live pool's ids.
// Without pool blocks, the projection is newCfg.Upstreams (which is
// what swapStaticPool will install).
func (r *reloader) projectedUpstreamIDs(newCfg *config.Config) map[string]struct{} {
	if r.runningHasPool || len(newCfg.UpstreamPools) > 0 {
		ids := r.sb.PoolIDs()
		out := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			out[id] = struct{}{}
		}
		return out
	}
	out := make(map[string]struct{}, len(newCfg.Upstreams))
	for _, u := range newCfg.Upstreams {
		out[u.ID] = struct{}{}
	}
	return out
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

	// Phase 8: pre-validate the [[rule]] block so a malformed pattern
	// or unknown prefer id aborts the reload BEFORE any partial state
	// is installed. Pool changes happen later, so prefer-id validation
	// uses the projected pool: when the running config has no pool
	// blocks and the new config also has none, the projected ids come
	// from newCfg.Upstreams; otherwise validate against the live pool
	// (pool-shape changes are restart-frozen for v1).
	newRules, ruleErr := upstream.NewRuleSet(newCfg.Rules)
	if ruleErr != nil {
		r.logger.Warn("config reload failed (rule compile)", "err", ruleErr)
		r.registry.ObserveConfigReload(metrics.ResultError)
		return
	}
	projectedIDs := r.projectedUpstreamIDs(newCfg)
	if err := newRules.CheckPreferIDs(projectedIDs); err != nil {
		r.logger.Warn("config reload failed (rule prefer ids unknown)", "err", err)
		r.registry.ObserveConfigReload(metrics.ResultError)
		return
	}

	// Always hot-reload scoring tunings and the status detector. They
	// are independent of the pool shape.
	r.sb.Reload(scoreboard.FromConfig(newCfg.Failure.Scoring, newCfg.Failure.Status, newCfg.Failure.ConnectionRefused))
	r.httpSrv.Reload(failure.NewStatusDetector(newCfg.Failure.Status), newCfg.Failure.MaxRetriesPerRequest)
	r.httpSrv.ReloadConnectionRefused(newCfg.Failure.ConnectionRefused)

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

	// Pool swap is done. Install the new rule set on both the
	// scoreboard and the listener, plus update the body-buffer cap.
	r.sb.ReplaceRules(newRules)
	r.httpSrv.ReloadRules(newRules)
	r.httpSrv.ReloadBodyBufferKB(newCfg.Failure.BodyBufferKB)

	// Phase 12: rotate control auth tokens. Missing auth_tokens_file on
	// SIGHUP keeps the current token set rather than dropping to empty
	// (which would silently disable auth on a typo'd path or a brief
	// logrotate-style disappearance). Hard read errors also keep the
	// current set. Only a clean load — or a fully cleared config —
	// advances the runtime state. gate_healthz is intentionally not
	// logged here: it is wired at NewServer time and is restart-only;
	// reporting it during a reload would mislead operators about what
	// the running listener is actually using.
	if r.controlSrv != nil {
		newTokens, missingFile, err := buildControlTokens(newCfg.Control)
		switch {
		case err != nil:
			r.logger.Warn("control auth tokens not rotated (file read error); keeping current set",
				"err", err)
		case missingFile:
			r.logger.Warn("control auth tokens not rotated (auth_tokens_file missing); keeping current set",
				"path", newCfg.Control.AuthTokensFile)
		default:
			// Server.ReplaceTokens emits the post-swap Info log itself,
			// so the reloader does not also log here — otherwise every
			// SIGHUP would produce a duplicate "auth tokens" line.
			r.controlSrv.ReplaceTokens(newTokens)
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
