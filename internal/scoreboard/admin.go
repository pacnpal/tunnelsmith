package scoreboard

import (
	"errors"
	"sort"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// Admin operations the Phase 9 web UI calls into. These three methods plus
// Snapshot are the entire surface the UI handlers need; everything else on
// the Scoreboard stays internal to the request path.

// ErrUnknownUpstream is returned by Force when the upstream id is not in
// the live pool. Force never auto-creates upstreams; the operator must
// pin to one of the configured ids.
var ErrUnknownUpstream = errors.New("scoreboard: unknown upstream id")

// ForceEntry is one active Force pin. Returned by ForcedFor and used by
// Pick to restrict candidates to a single upstream while the pin is
// active.
type ForceEntry struct {
	UpstreamID string
	Until      time.Time
}

// Forget drops every per-(host, upstream) entry for host plus any active
// cascade for it. Debounce keys for the host are also cleared so a
// subsequent failure starts a fresh window. The Force pin (if any) is
// left in place: an operator who pinned an upstream and then asked to
// "forget" the host typically wants the score reset, not the pin
// dropped. Use ClearForce to drop the pin.
//
// Returns true if any state was removed; false means the host had no
// scoreboard footprint and the call was a no-op.
func (s *Scoreboard) Forget(host string) bool {
	if host == "" {
		return false
	}
	s.mu.Lock()
	_, hadEntries := s.entries[host]
	delete(s.entries, host)
	_, hadCascade := s.cascade[host]
	delete(s.cascade, host)
	s.mu.Unlock()

	hadDebounce := false
	s.debounceMu.Lock()
	for k := range s.debounce {
		if k.host == host {
			delete(s.debounce, k)
			hadDebounce = true
		}
	}
	s.debounceMu.Unlock()

	removed := hadEntries || hadCascade || hadDebounce
	if removed {
		s.logger.Info("scoreboard forget", "host", host)
	}
	return removed
}

// Force pins host to upstreamID until the given expiry. While the pin is
// active and the upstream is not in the per-call tried set, Pick returns
// it ahead of normal scoring and ignores the upstream's own cooldown
// (the operator is overriding the learning loop on purpose). Force does
// not clear cascade: if the host is in cascade, Pick still short-
// circuits with ErrCascadeCooling. Operators who want to retry a cooled
// host should ForgetHost first, then Force.
//
// Force returns ErrUnknownUpstream if upstreamID is not currently in
// the live pool. until in the past clears any existing pin for host;
// the clear path runs before pool-membership validation so an operator
// can drop a stale pin even after a hot reload removes the upstream
// the pin originally referenced.
func (s *Scoreboard) Force(host, upstreamID string, until time.Time) error {
	if host == "" {
		return errors.New("scoreboard: Force requires a non-empty host")
	}
	if upstreamID == "" {
		return errors.New("scoreboard: Force requires a non-empty upstream id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if !until.After(now) {
		// Clear path: drop any active pin for host regardless of
		// whether upstreamID is still in the live pool. If there was no
		// pin to clear this is a no-op.
		if _, ok := s.forces[host]; ok {
			delete(s.forces, host)
			s.logger.Info("scoreboard force cleared", "host", host, "reason", "until_in_past")
		}
		return nil
	}
	known := false
	for _, c := range s.poolEntries {
		if c.Up.ID() == upstreamID {
			known = true
			break
		}
	}
	if !known {
		return ErrUnknownUpstream
	}
	if s.forces == nil {
		s.forces = make(map[string]ForceEntry)
	}
	s.forces[host] = ForceEntry{UpstreamID: upstreamID, Until: until}
	s.logger.Info("scoreboard force",
		"host", host,
		"upstream_id", upstreamID,
		"until", until.UTC().Format(time.RFC3339),
	)
	return nil
}

// ClearForce drops any active Force pin for host. Returns true if a pin
// was active; false means there was nothing to clear.
func (s *Scoreboard) ClearForce(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.forces[host]; !ok {
		return false
	}
	delete(s.forces, host)
	s.logger.Info("scoreboard force cleared", "host", host, "reason", "explicit")
	return true
}

// ForcedFor returns the live force pin for host, or zero-value plus
// false if none is active. An expired pin is treated as absent and
// pruned in place so Pick is not the only path that evicts dead state.
func (s *Scoreboard) ForcedFor(host string) (ForceEntry, bool) {
	now := s.clock()
	s.mu.RLock()
	entry, ok := s.forces[host]
	s.mu.RUnlock()
	if !ok {
		return ForceEntry{}, false
	}
	if !entry.Until.After(now) {
		s.mu.Lock()
		// Re-check under the write lock so a concurrent Force that
		// installed a fresh pin between the RUnlock and Lock is not
		// clobbered.
		if cur, stillThere := s.forces[host]; stillThere && !cur.Until.After(now) {
			delete(s.forces, host)
		}
		s.mu.Unlock()
		return ForceEntry{}, false
	}
	return entry, true
}

// ForceSnapshot returns every active Force pin sorted by host. Expired
// pins are excluded from the result and evicted from s.forces in the
// same call so a host that was pinned and then never picked again does
// not leak the entry forever.
func (s *Scoreboard) ForceSnapshot() []ForceSnapshotEntry {
	now := s.clock()
	s.mu.RLock()
	out := make([]ForceSnapshotEntry, 0, len(s.forces))
	expired := make([]string, 0)
	for host, f := range s.forces {
		if !f.Until.After(now) {
			expired = append(expired, host)
			continue
		}
		out = append(out, ForceSnapshotEntry{
			Host:       host,
			UpstreamID: f.UpstreamID,
			Until:      f.Until,
		})
	}
	s.mu.RUnlock()
	if len(expired) > 0 {
		s.mu.Lock()
		for _, host := range expired {
			// Re-check under the write lock so a concurrent Force that
			// installed a fresh pin between the RUnlock and Lock is not
			// clobbered.
			if cur, ok := s.forces[host]; ok && !cur.Until.After(now) {
				delete(s.forces, host)
			}
		}
		s.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// ForceSnapshotEntry is a read-only view of one Force pin. Returned by
// ForceSnapshot for the UI's force-table render.
type ForceSnapshotEntry struct {
	Host       string
	UpstreamID string
	Until      time.Time
}

// pickForced is Pick's pre-scan for an active Force pin on host. Returns
// (upstream, true) when the pin is live and the pinned upstream is in
// the live pool and not in tried. Returns (nil, false) otherwise so
// Pick falls through to its usual scoring path. The expired-pin lazy
// eviction lives here too: an active pin whose Until is now in the
// past is dropped and the call returns (nil, false).
//
// Holds RLock for the lookup, then upgrades to Lock only when an
// expired pin needs eviction. The common case (no pin or live pin) is
// read-only.
func (s *Scoreboard) pickForced(host string, tried map[string]bool, now time.Time) (upstream.Upstream, bool) {
	s.mu.RLock()
	entry, ok := s.forces[host]
	if !ok {
		s.mu.RUnlock()
		return nil, false
	}
	if !entry.Until.After(now) {
		s.mu.RUnlock()
		s.mu.Lock()
		// Re-check under the write lock so a concurrent Force that
		// installed a fresh pin between the RUnlock and Lock is not
		// clobbered.
		if cur, stillThere := s.forces[host]; stillThere && !cur.Until.After(now) {
			delete(s.forces, host)
		}
		s.mu.Unlock()
		return nil, false
	}
	if tried[entry.UpstreamID] {
		// Pin is live but the in-flight retry already burned this
		// upstream. Fall through to normal scoring instead of looping.
		s.mu.RUnlock()
		return nil, false
	}
	for _, c := range s.poolEntries {
		if c.Up.ID() == entry.UpstreamID {
			s.mu.RUnlock()
			return c.Up, true
		}
	}
	// Pin references an upstream that is no longer in the pool (likely
	// dropped during a hot reload). Fall through; ClearForce can
	// evict it explicitly when the operator notices.
	s.mu.RUnlock()
	return nil, false
}

// Reset clears every entry, every cascade, every Force pin, and every
// debounce key. The pool stays untouched: the operator is throwing away
// learned state, not changing the upstreams themselves. After Reset the
// scoreboard behaves as if just constructed.
func (s *Scoreboard) Reset() {
	s.mu.Lock()
	s.entries = make(map[string]map[string]*entry)
	s.cascade = make(map[string]time.Time)
	s.forces = nil
	s.mu.Unlock()

	s.debounceMu.Lock()
	s.debounce = make(map[debounceKey]time.Time)
	s.debounceMu.Unlock()

	s.logger.Info("scoreboard reset")
}
