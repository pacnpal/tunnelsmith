package scoreboard

// Recovery probing. The default behavior is "always pick the top-ranked
// non-cooled candidate". That is correct in steady state but pins the
// scoreboard to one upstream forever if a previously-penalized upstream
// has recovered: it never gets retried because it is no longer the top
// pick. The probe knob fixes that.
//
// On every Pick, the scoreboard rolls a uniform [0,1) draw against
// cfg.ProbeChance. If the draw wins and there is more than one eligible
// candidate, Pick returns a random non-top eligible candidate instead of
// the top pick. The probed upstream's score climbs naturally on success
// or sinks further on failure, same penalty path as a normal pick.
//
// The plan calls for the random source to be injectable so tests can use
// a fixed seed and assert specific probe outcomes. WithRand handles that;
// the helpers below own the per-Pick draw and serialize access to the
// underlying *rand.Rand so concurrent Picks do not race it.

// probeRoll returns true when a uniform [0,1) draw falls below chance. The
// caller passes the snapshotted ProbeChance so a concurrent Reload cannot
// race the read; only the random source is serialized here, since
// math/rand.Rand is not goroutine-safe on its own.
func (s *Scoreboard) probeRoll(chance float64) bool {
	s.randMu.Lock()
	defer s.randMu.Unlock()
	return s.rand.Float64() < chance
}

// probePick returns a uniform random integer in [0, n). Caller guarantees
// n > 0; Pick checks that before calling.
func (s *Scoreboard) probePick(n int) int {
	s.randMu.Lock()
	defer s.randMu.Unlock()
	return s.rand.Intn(n)
}
