package scoreboard

import "time"

// Cascade-failure handling. When every upstream fails for a host on the same
// request, the scoreboard sets a host-keyed expiry timestamp. Subsequent
// Picks within the negative TTL short-circuit with ErrCascadeCooling so a
// stampede of clients does not burn through the upstream pool again.
//
// The proposal is explicit on the trade-off: cascade exists to avoid
// amplifying a real outage. After the negative TTL expires, the next request
// gets a fresh, full-pool attempt. A single RecordSuccess for the host
// clears cascade early; that lives in scoreboard.go alongside the rest of
// the success bookkeeping.
//
// State lives in Scoreboard.cascade and is guarded by Scoreboard.mu. The
// helpers below keep the access pattern in one file rather than scattering
// "delete(s.cascade, host)" across the rest of the package.

// cascadeActive reports whether host is currently in cascade at the given
// instant. Takes the read lock; safe to call concurrently with Pick.
func (s *Scoreboard) cascadeActive(host string, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	until, ok := s.cascade[host]
	return ok && until.After(now)
}

// TripCascade marks host as in cascade for cfg.CascadeTTL starting from the
// current clock. A CascadeTTL of zero or less makes this a no-op so callers
// can disable the feature entirely without an extra config branch.
//
// Exported so listeners that drive their own retry loop (the plain-HTTP
// path now layers status-code retries on top of the dial loop) can mark
// cascade after exhausting every upstream for one request. DialFor calls
// it on the dial-only path; both call sites converge on the same state.
func (s *Scoreboard) TripCascade(host string) {
	if s.cfg.CascadeTTL <= 0 {
		return
	}
	until := s.clock().Add(s.cfg.CascadeTTL)
	s.mu.Lock()
	s.cascade[host] = until
	s.mu.Unlock()
	if s.metrics != nil {
		s.metrics.ObserveCascadeTrip()
	}
	s.logger.Warn("cascade tripped",
		"host", host,
		"ttl_ms", s.cfg.CascadeTTL.Milliseconds(),
	)
}

// CascadeUntil returns the cascade-cooling expiry for host, or the zero
// time if the host is not currently in cascade. Exposed for tests and the
// future web UI; production code uses cascadeActive.
func (s *Scoreboard) CascadeUntil(host string) time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	until, ok := s.cascade[host]
	if !ok {
		return time.Time{}
	}
	if !until.After(s.clock()) {
		return time.Time{}
	}
	return until
}
