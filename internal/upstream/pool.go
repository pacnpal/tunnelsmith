package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
)

// PoolEntry pairs an Upstream with the priority that determines its place
// in the dial order. Lower priority dials first, ties keep insertion order.
type PoolEntry struct {
	Up       Upstream
	Priority int
}

// Pool is the static priority-ordered upstream list the listener dials
// through. It tries upstreams in priority order until one succeeds or
// the per-request retry cap is reached. Each attempt emits a structured
// log line with upstream_id, host, outcome, and latency_ms; failed dials
// also include the error and the kind ("refused" / "timeout" / "other").
//
// Phase 3 has no notion of per-host scoring or cooldowns; that lands in
// Phase 4 on top of this same iteration order. Pool is the HAProxy-equivalent
// baseline: priority list with retry on hard failure.
type Pool struct {
	entries  []PoolEntry
	retryCap int
	logger   *slog.Logger

	clock func() time.Time
}

// NewPool builds a pool from the given entries. entries are sorted by
// priority ascending using a stable sort, so original order breaks ties.
// retryCap is the maximum number of dial attempts per request and must be
// at least 1; values from config.FailureConfig.MaxRetriesPerRequest already
// pass that bar via the package's defaulting and validation.
func NewPool(entries []PoolEntry, retryCap int, logger *slog.Logger) (*Pool, error) {
	if len(entries) == 0 {
		return nil, errors.New("pool: at least one upstream required")
	}
	if retryCap < 1 {
		return nil, fmt.Errorf("pool: retry cap must be >= 1, got %d", retryCap)
	}
	if logger == nil {
		logger = slog.Default()
	}
	for i, e := range entries {
		if e.Up == nil {
			return nil, fmt.Errorf("pool: entry[%d] has nil upstream", i)
		}
	}
	sorted := append([]PoolEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority < sorted[j].Priority
	})
	return &Pool{
		entries:  sorted,
		retryCap: retryCap,
		logger:   logger,
		clock:    time.Now,
	}, nil
}

// Len reports the number of upstreams in the pool.
func (p *Pool) Len() int { return len(p.entries) }

// IDs returns the ordered list of upstream ids in the pool. Useful for
// startup logs and tests that want to assert iteration order.
func (p *Pool) IDs() []string {
	out := make([]string, len(p.entries))
	for i, e := range p.entries {
		out[i] = e.Up.ID()
	}
	return out
}

// DialFor opens a connection to addr through the highest-priority upstream
// whose Dial returns a usable conn. On every failure DialFor advances to the
// next upstream and tries again, up to retryCap total attempts. On full
// failure the returned error joins each per-attempt error so callers can
// surface "all upstreams refused / timed out" diagnostics.
//
// The signature follows upstream.Upstream.Dial so callers can swap the two
// without ceremony. The second return value is the id of the upstream that
// served the request, which Phase 4's scoreboard will use as the key for
// per-(host, upstream) accounting.
func (p *Pool) DialFor(ctx context.Context, network, addr string) (net.Conn, string, error) {
	host := hostOnly(addr)
	limit := p.retryCap
	if limit > len(p.entries) {
		limit = len(p.entries)
	}

	var attemptErrs []error
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			if len(attemptErrs) == 0 {
				// Context already done before the first dial. Don't
				// pretend an attempt happened: surface the cancellation
				// directly so operator logs and the returned error
				// reflect reality.
				return nil, "", fmt.Errorf("pool: context canceled before any upstream was tried: %w", err)
			}
			// Cancellation arrived between attempts. Note it on the
			// aggregated trail and stop iterating; counters below
			// reflect dials that actually fired.
			attemptErrs = append(attemptErrs, fmt.Errorf("context canceled after %d attempt(s): %w", len(attemptErrs), err))
			break
		}
		entry := p.entries[i]
		start := p.clock()
		conn, err := entry.Up.Dial(ctx, network, addr)
		latencyMS := p.clock().Sub(start).Milliseconds()
		if err == nil {
			p.logger.Info("upstream dial",
				"upstream_id", entry.Up.ID(),
				"host", host,
				"outcome", "success",
				"latency_ms", latencyMS,
				"attempt", i+1,
			)
			return conn, entry.Up.ID(), nil
		}
		p.logger.Warn("upstream dial",
			"upstream_id", entry.Up.ID(),
			"host", host,
			"outcome", "failure",
			"latency_ms", latencyMS,
			"attempt", i+1,
			"kind", classifyKind(err),
			"err", err,
		)
		attemptErrs = append(attemptErrs, fmt.Errorf("%s: %w", entry.Up.ID(), err))
	}

	return nil, "", fmt.Errorf(
		"pool: all upstreams failed after %d attempt(s) (cap=%d, pool=%d): %w",
		len(attemptErrs), p.retryCap, len(p.entries), errors.Join(attemptErrs...),
	)
}

// classifyKind maps a dial error to the short tag the failure-log line
// uses ("refused" / "timeout" / "other"). Phase 4 will key its scoreboard
// off the same classification.
func classifyKind(err error) string {
	switch {
	case failure.IsConnectionRefused(err):
		return "refused"
	case failure.IsTimeout(err):
		return "timeout"
	default:
		return "other"
	}
}

// hostOnly returns the host portion of a host:port pair. Falls back to the
// whole input if SplitHostPort fails so logs still carry something useful.
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
