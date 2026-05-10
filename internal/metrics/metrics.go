// Package metrics owns the Prometheus exposition surface added in Phase 7.
//
// The Registry type bundles a private *prometheus.Registry with the
// counters, histograms, and gauges Tunnelsmith emits. Construct one via
// New, hand it to the scoreboard and the listeners, and call ServeHTTP
// on the result of Handler() to expose /metrics.
//
// All metrics live under the tunnelsmith_ namespace. Cardinality is
// bounded on purpose: per-host gauges would blow up with thousands of
// destinations and 60+ Mullvad relays, so per-host detail belongs in the
// Phase 9 web UI instead. Per-upstream labels are safe because the
// upstream-id space is small and stable.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Outcome labels for the request_outcomes_total counter.
const (
	OutcomeSuccess   = "success"
	OutcomeCascade   = "cascade"
	OutcomeExhausted = "exhausted"
)

// DialOutcome labels for dial_attempts_total and dial_latency_seconds.
const (
	DialOutcomeSuccess = "success"
	DialOutcomeRefused = "refused"
	DialOutcomeTimeout = "timeout"
	DialOutcomeOther   = "other"
)

// Result labels for the persistence/reload counters.
const (
	ResultSuccess = "success"
	ResultError   = "error"
)

// Reason labels for reports_rejected_total. internal/control imports
// these constants so reject labels stay consistent across packages.
const (
	ReportRejectBadJSON              = "bad_json"
	ReportRejectMissingField         = "missing_field"
	ReportRejectUnknownOutcome       = "unknown_outcome"
	ReportRejectUnknownUpstream      = "unknown_upstream"
	ReportRejectScoreboardNotStarted = "scoreboard_unavailable"
)

// Registry holds Tunnelsmith's metric vectors and the Prometheus registry
// they live in. Methods are safe for concurrent use.
type Registry struct {
	reg *prometheus.Registry

	DialAttempts        *prometheus.CounterVec
	DialLatency         *prometheus.HistogramVec
	StatusFailures      *prometheus.CounterVec
	RequestOutcomes     *prometheus.CounterVec
	CascadeTrips        prometheus.Counter
	ProbePicks          prometheus.Counter
	PersistenceWrites   *prometheus.CounterVec
	ConfigReloads       *prometheus.CounterVec
	UpstreamPoolSize    prometheus.Gauge
	ScoreboardEntries   prometheus.Gauge
	UpstreamCooledHosts *prometheus.GaugeVec
	CascadeActiveHosts  prometheus.Gauge
	ReportsReceived     *prometheus.CounterVec
	ReportsRejected     *prometheus.CounterVec
}

// New constructs a Registry with all metric vectors registered in a fresh
// Prometheus registry. The returned Registry can be passed to scoreboard
// and listener constructors that take an optional metrics sink.
func New() *Registry {
	reg := prometheus.NewRegistry()

	r := &Registry{
		reg: reg,
		DialAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tunnelsmith",
			Name:      "dial_attempts_total",
			Help:      "Number of upstream dial attempts, emitted by the scoreboard's DialFor (CONNECT and SOCKS5 paths) and by the HTTP plain-HTTP forward path. A status-detected failure (429/403/451) still records a successful dial here because the connection succeeded; the failure dimension is captured by status_failures_total. Labelled by upstream and outcome.",
		}, []string{"upstream_id", "outcome"}),
		DialLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "tunnelsmith",
			Name:      "dial_latency_seconds",
			Help:      "Wall-clock latency of upstream dial attempts. On the scoreboard's DialFor it measures the TCP / SOCKS5 handshake; on the HTTP plain-HTTP forward path it measures the full RoundTrip (handshake plus request / response headers). Labelled by upstream and outcome.",
			Buckets:   prometheus.ExponentialBuckets(0.005, 2, 12),
		}, []string{"upstream_id", "outcome"}),
		StatusFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tunnelsmith",
			Name:      "status_failures_total",
			Help:      "Number of HTTP responses the listener treated as upstream failures, labelled by upstream and status code.",
		}, []string{"upstream_id", "code"}),
		RequestOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tunnelsmith",
			Name:      "request_outcomes_total",
			Help:      "Terminal outcome the listener returned to the client, labelled by outcome.",
		}, []string{"outcome"}),
		CascadeTrips: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "tunnelsmith",
			Name:      "cascade_trips_total",
			Help:      "Number of times a host transitioned into cascade-failure mode after every upstream failed.",
		}),
		ProbePicks: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "tunnelsmith",
			Name:      "probe_picks_total",
			Help:      "Number of recovery probes the scoreboard issued (a non-top eligible candidate was picked).",
		}),
		PersistenceWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tunnelsmith",
			Name:      "persistence_writes_total",
			Help:      "Result of scoreboard snapshot writes, labelled by result.",
		}, []string{"result"}),
		ConfigReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tunnelsmith",
			Name:      "config_reloads_total",
			Help:      "Result of SIGHUP-driven config reloads, labelled by result.",
		}, []string{"result"}),
		UpstreamPoolSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "tunnelsmith",
			Name:      "upstream_pool_size",
			Help:      "Number of upstreams currently in the pool.",
		}),
		ScoreboardEntries: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "tunnelsmith",
			// Prometheus convention reserves the _total suffix for
			// counters; this is a gauge, so it stays plain.
			Name: "scoreboard_entries",
			Help: "Number of (host, upstream) entries the scoreboard is tracking.",
		}),
		UpstreamCooledHosts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tunnelsmith",
			Name:      "upstream_cooled_hosts",
			Help:      "Number of hosts currently on cooldown for the labelled upstream.",
		}, []string{"upstream_id"}),
		CascadeActiveHosts: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "tunnelsmith",
			Name:      "cascade_active_hosts",
			Help:      "Number of hosts currently in cascade-failure cooldown.",
		}),
		ReportsReceived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tunnelsmith",
			Name:      "reports_received_total",
			Help:      "Number of cooperative outcome reports the control endpoint accepted, labelled by outcome and upstream.",
		}, []string{"outcome", "upstream_id"}),
		ReportsRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tunnelsmith",
			Name:      "reports_rejected_total",
			Help:      "Number of cooperative outcome reports the control endpoint rejected, labelled by reason (bad_json, missing_field, unknown_outcome, unknown_upstream, scoreboard_unavailable).",
		}, []string{"reason"}),
	}

	reg.MustRegister(
		r.DialAttempts,
		r.DialLatency,
		r.StatusFailures,
		r.RequestOutcomes,
		r.CascadeTrips,
		r.ProbePicks,
		r.PersistenceWrites,
		r.ConfigReloads,
		r.UpstreamPoolSize,
		r.ScoreboardEntries,
		r.UpstreamCooledHosts,
		r.CascadeActiveHosts,
		r.ReportsReceived,
		r.ReportsRejected,
	)

	return r
}

// Handler returns an http.Handler that exposes the Prometheus exposition
// endpoint for this Registry.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		Registry: r.reg,
	})
}

// PrometheusRegistry exposes the underlying *prometheus.Registry so tests
// can inspect or gather metrics directly.
func (r *Registry) PrometheusRegistry() *prometheus.Registry { return r.reg }

// ObserveDial records one dial attempt. outcome is one of the
// DialOutcome* constants. Latency is the wall-clock time the attempt took.
func (r *Registry) ObserveDial(upstreamID, outcome string, latency time.Duration) {
	if r == nil {
		return
	}
	r.DialAttempts.WithLabelValues(upstreamID, outcome).Inc()
	r.DialLatency.WithLabelValues(upstreamID, outcome).Observe(latency.Seconds())
}

// ObserveStatusFailure increments the status_failures_total counter for a
// listener-detected failure response. code is the upstream's HTTP status
// formatted as a decimal string (e.g. "429").
func (r *Registry) ObserveStatusFailure(upstreamID, code string) {
	if r == nil {
		return
	}
	r.StatusFailures.WithLabelValues(upstreamID, code).Inc()
}

// ObserveRequestOutcome records the terminal outcome of one client request.
func (r *Registry) ObserveRequestOutcome(outcome string) {
	if r == nil {
		return
	}
	r.RequestOutcomes.WithLabelValues(outcome).Inc()
}

// ObserveCascadeTrip increments the cascade-trips counter.
func (r *Registry) ObserveCascadeTrip() {
	if r == nil {
		return
	}
	r.CascadeTrips.Inc()
}

// ObserveProbePick increments the probe-picks counter. Called every time
// the scoreboard's probe roll picked a non-top eligible candidate.
func (r *Registry) ObserveProbePick() {
	if r == nil {
		return
	}
	r.ProbePicks.Inc()
}

// ObservePersistenceWrite increments persistence_writes_total. result is
// one of ResultSuccess or ResultError.
func (r *Registry) ObservePersistenceWrite(result string) {
	if r == nil {
		return
	}
	r.PersistenceWrites.WithLabelValues(result).Inc()
}

// ObserveConfigReload increments config_reloads_total. result is one of
// ResultSuccess or ResultError.
func (r *Registry) ObserveConfigReload(result string) {
	if r == nil {
		return
	}
	r.ConfigReloads.WithLabelValues(result).Inc()
}

// ObserveReportReceived increments reports_received_total for an accepted
// cooperative outcome report. Phase 11.
func (r *Registry) ObserveReportReceived(outcome, upstreamID string) {
	if r == nil {
		return
	}
	r.ReportsReceived.WithLabelValues(outcome, upstreamID).Inc()
}

// ObserveReportRejected increments reports_rejected_total for a report the
// control endpoint refused. reason is one of the ReportReject* constants.
func (r *Registry) ObserveReportRejected(reason string) {
	if r == nil {
		return
	}
	r.ReportsRejected.WithLabelValues(reason).Inc()
}

// SetUpstreamPoolSize updates the gauge tracking the active upstream pool
// size. Called from cmd/tunnelsmith on startup and after a hot-reload.
func (r *Registry) SetUpstreamPoolSize(n int) {
	if r == nil {
		return
	}
	r.UpstreamPoolSize.Set(float64(n))
}

// SetScoreboardSnapshot updates the scoreboard-shape gauges from a Snapshot
// pass: total entries, per-upstream cooled-host counts, and the count of
// hosts currently in cascade. Counts the caller passes here are derived
// from the same Snapshot the persistence writer uses, so /metrics and the
// snapshot file stay in lockstep.
//
// poolIDs is the full list of upstream ids known to the binary. The
// upstream_cooled_hosts gauge vec is reset before repopulating so an
// upstream that disappeared from the pool after a hot-reload no longer
// shows its stale label series.
func (r *Registry) SetScoreboardSnapshot(totalEntries int, cooledByUpstream map[string]int, cascadeHosts int, poolIDs []string) {
	if r == nil {
		return
	}
	r.ScoreboardEntries.Set(float64(totalEntries))
	// Reset drops every label series the gauge vec is currently tracking,
	// so the loop below repopulates only ids that exist in the live pool.
	// A scrape between Reset and the loop sees zero series; the refresh
	// cadence (5 seconds) makes that gap acceptable.
	r.UpstreamCooledHosts.Reset()
	for _, id := range poolIDs {
		count := cooledByUpstream[id]
		r.UpstreamCooledHosts.WithLabelValues(id).Set(float64(count))
	}
	r.CascadeActiveHosts.Set(float64(cascadeHosts))
}

// Server wraps an http.Server that serves the Prometheus exposition
// endpoint at /metrics. It is safe to call Serve and Shutdown concurrently.
type Server struct {
	addr   string
	logger *slog.Logger
	srv    *http.Server

	ready    chan struct{}
	listener net.Listener
	bindErr  error
}

// NewServer builds a metrics HTTP server that exposes registry at /metrics
// on addr. The caller is expected to drive Serve in its errgroup and call
// Shutdown during graceful shutdown.
func NewServer(addr string, registry *Registry, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "metrics")
	mux := http.NewServeMux()
	mux.Handle("/metrics", registry.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
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
// scraping.
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
		s.bindErr = fmt.Errorf("metrics listen %s: %w", s.addr, err)
		close(s.ready)
		return s.bindErr
	}
	s.listener = l
	close(s.ready)
	s.logger.Info("listening", "addr", l.Addr().String())
	if err := s.srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("metrics serve: %w", err)
	}
	return nil
}

// Shutdown stops the metrics server.
func (s *Server) Shutdown(ctx context.Context) error {
	select {
	case <-s.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.srv.Shutdown(ctx)
}
