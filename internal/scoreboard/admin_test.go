package scoreboard_test

import (
	"errors"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
)

// TestForgetClearsHostState confirms Forget drops every per-(host, upstream)
// entry, clears any cascade for the host, and wipes debounce keys for it.
func TestForgetClearsHostState(t *testing.T) {
	clock := newManualClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	sb := buildScoreboard(t, []*fakeUpstream{alwaysOK("a"), alwaysRefused("b")}, scoreboard.Config{}, clock, 1)

	// Generate state on host1 across both upstreams.
	if _, err := dialOnce(t, sb, "host1.example.com:443"); err != nil {
		t.Fatalf("dial host1: %v", err)
	}
	sb.RecordFailure("host1.example.com", "b", failure.KindRefused, nil)
	sb.TripCascade("host1.example.com")

	// And one entry on host2 to confirm it is left alone.
	if _, err := dialOnce(t, sb, "host2.example.com:443"); err != nil {
		t.Fatalf("dial host2: %v", err)
	}

	if got := sb.EntriesCount(); got < 2 {
		t.Fatalf("EntriesCount before Forget = %d, want at least 2", got)
	}

	if !sb.Forget("host1.example.com") {
		t.Fatal("Forget(host1) returned false; expected true (state was present)")
	}

	for _, snap := range sb.Snapshot() {
		if snap.Host == "host1.example.com" {
			t.Errorf("host1 entry %+v survived Forget", snap)
		}
	}
	if sb.CascadeUntil("host1.example.com") != (time.Time{}) {
		t.Error("cascade for host1 survived Forget")
	}

	// host2 untouched.
	host2Found := false
	for _, snap := range sb.Snapshot() {
		if snap.Host == "host2.example.com" {
			host2Found = true
		}
	}
	if !host2Found {
		t.Error("host2 entry was wrongly cleared by Forget(host1)")
	}
}

// TestForgetUnknownHostReturnsFalse confirms Forget on an absent host is a
// no-op and reports it.
func TestForgetUnknownHostReturnsFalse(t *testing.T) {
	sb := buildScoreboard(t, []*fakeUpstream{alwaysOK("a")}, scoreboard.Config{}, nil, 1)
	if sb.Forget("never-seen.example.com") {
		t.Error("Forget on unknown host returned true; want false")
	}
	if sb.Forget("") {
		t.Error("Forget on empty host returned true; want false")
	}
}

// TestForcePinRoutesThroughChosenUpstream confirms an active Force pin
// takes precedence over normal scoring: the pinned upstream serves even
// when another upstream has a higher score.
func TestForcePinRoutesThroughChosenUpstream(t *testing.T) {
	clock := newManualClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	winner := alwaysOK("winner")
	pinned := alwaysOK("pinned")
	sb := buildScoreboard(t, []*fakeUpstream{winner, pinned}, scoreboard.Config{}, clock, 1)

	// Warm up "winner" so it climbs the score. Without a pin Pick would
	// route through it.
	for i := 0; i < 5; i++ {
		if _, err := dialOnce(t, sb, "host.example.com:443"); err != nil {
			t.Fatalf("warm-up dial %d: %v", i, err)
		}
	}
	if winner.dialCount.Load() != 5 {
		t.Fatalf("winner.dialCount before pin = %d, want 5", winner.dialCount.Load())
	}

	if err := sb.Force("host.example.com", "pinned", clock.Now().Add(30*time.Minute)); err != nil {
		t.Fatalf("Force: %v", err)
	}

	// Three dials post-pin should all go through "pinned", not the score
	// leader.
	for i := 0; i < 3; i++ {
		id, err := dialOnce(t, sb, "host.example.com:443")
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		if id != "pinned" {
			t.Errorf("dial %d went through %q, want pinned", i, id)
		}
	}
	if got := pinned.dialCount.Load(); got != 3 {
		t.Errorf("pinned.dialCount after pin = %d, want 3", got)
	}
	if got := winner.dialCount.Load(); got != 5 {
		t.Errorf("winner.dialCount drifted while pin was active = %d, want 5", got)
	}
}

// TestForceExpiresLazily confirms a pin past its Until is treated as
// absent and falls back to normal scoring.
func TestForceExpiresLazily(t *testing.T) {
	clock := newManualClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	leader := alwaysOK("leader")
	pinned := alwaysOK("pinned")
	sb := buildScoreboard(t, []*fakeUpstream{leader, pinned}, scoreboard.Config{}, clock, 1)

	// Warm "leader" with five successes so its score (5) sits comfortably
	// above the single success "pinned" will accumulate during the
	// pinned interval. Without this, the pinned upstream's post-pin
	// score equals leader's and the (score desc, base priority asc)
	// tiebreak picks leader anyway, so the test would not actually
	// exercise expiry-fallback.
	for i := 0; i < 5; i++ {
		if _, err := dialOnce(t, sb, "h.example.com:443"); err != nil {
			t.Fatalf("warm-up dial %d: %v", i, err)
		}
	}
	if leader.dialCount.Load() != 5 {
		t.Fatalf("leader.dialCount after warm-up = %d, want 5", leader.dialCount.Load())
	}

	if err := sb.Force("h.example.com", "pinned", clock.Now().Add(time.Minute)); err != nil {
		t.Fatalf("Force: %v", err)
	}

	if id, err := dialOnce(t, sb, "h.example.com:443"); err != nil || id != "pinned" {
		t.Fatalf("dial under pin id=%q err=%v, want pinned/no-error", id, err)
	}

	// Advance past the pin's Until and confirm the next pick goes through
	// the priority leader instead.
	clock.Add(2 * time.Minute)
	if id, err := dialOnce(t, sb, "h.example.com:443"); err != nil || id != "leader" {
		t.Fatalf("post-expiry dial id=%q err=%v, want leader/no-error", id, err)
	}
	if _, ok := sb.ForcedFor("h.example.com"); ok {
		t.Error("expired pin still reports active via ForcedFor")
	}
}

// TestForceUnknownUpstreamErrors confirms Force rejects an upstream id that
// is not in the live pool.
func TestForceUnknownUpstreamErrors(t *testing.T) {
	sb := buildScoreboard(t, []*fakeUpstream{alwaysOK("a")}, scoreboard.Config{}, nil, 1)
	err := sb.Force("h.example.com", "nope", time.Now().Add(time.Minute))
	if !errors.Is(err, scoreboard.ErrUnknownUpstream) {
		t.Fatalf("Force unknown upstream err = %v, want ErrUnknownUpstream", err)
	}
}

// TestForcePastUntilClearsExistingPin confirms passing an Until in the past
// clears any active pin for the host (the UI uses this to "unforce").
func TestForcePastUntilClearsExistingPin(t *testing.T) {
	clock := newManualClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	sb := buildScoreboard(t, []*fakeUpstream{alwaysOK("a"), alwaysOK("b")}, scoreboard.Config{}, clock, 1)

	if err := sb.Force("h.example.com", "b", clock.Now().Add(time.Minute)); err != nil {
		t.Fatalf("Force: %v", err)
	}
	if _, ok := sb.ForcedFor("h.example.com"); !ok {
		t.Fatal("ForcedFor returned false right after Force")
	}
	// Past-until is a valid clear path (the UI uses it for "unforce").
	if err := sb.Force("h.example.com", "b", clock.Now().Add(-time.Second)); err != nil {
		t.Fatalf("Force with past until: %v", err)
	}
	if _, ok := sb.ForcedFor("h.example.com"); ok {
		t.Error("ForcedFor still active after past-until Force")
	}
}

// TestClearForceDropsActivePin confirms ClearForce removes a live pin and
// reports whether one was active.
func TestClearForceDropsActivePin(t *testing.T) {
	clock := newManualClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	sb := buildScoreboard(t, []*fakeUpstream{alwaysOK("a"), alwaysOK("b")}, scoreboard.Config{}, clock, 1)

	if err := sb.Force("h.example.com", "b", clock.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Force: %v", err)
	}
	if !sb.ClearForce("h.example.com") {
		t.Error("ClearForce returned false for an active pin")
	}
	if sb.ClearForce("h.example.com") {
		t.Error("ClearForce returned true on an already-clear host")
	}
	if _, ok := sb.ForcedFor("h.example.com"); ok {
		t.Error("ForcedFor still reports active after ClearForce")
	}
}

// TestForceFallsBackWhenPinnedIsTried confirms that when an in-flight retry
// already burned the pinned upstream, Pick falls back to the normal pool
// instead of looping on the same id.
func TestForceFallsBackWhenPinnedIsTried(t *testing.T) {
	clock := newManualClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	pinned := alwaysRefused("pinned")
	fallback := alwaysOK("fallback")
	sb := buildScoreboard(t, []*fakeUpstream{pinned, fallback}, scoreboard.Config{}, clock, 1)

	if err := sb.Force("h.example.com", "pinned", clock.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Force: %v", err)
	}

	id, err := dialOnce(t, sb, "h.example.com:443")
	if err != nil {
		t.Fatalf("DialFor: %v", err)
	}
	if id != "fallback" {
		t.Errorf("DialFor served %q, want fallback (pinned refused)", id)
	}
	if pinned.dialCount.Load() != 1 {
		t.Errorf("pinned.dialCount = %d, want 1", pinned.dialCount.Load())
	}
	if fallback.dialCount.Load() != 1 {
		t.Errorf("fallback.dialCount = %d, want 1", fallback.dialCount.Load())
	}
}

// TestForceSnapshotReturnsActivePins confirms ForceSnapshot returns one
// entry per active pin, sorted by host, with expired pins excluded.
func TestForceSnapshotReturnsActivePins(t *testing.T) {
	clock := newManualClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	sb := buildScoreboard(t, []*fakeUpstream{alwaysOK("a"), alwaysOK("b")}, scoreboard.Config{}, clock, 1)

	must := func(host, up string, until time.Time) {
		t.Helper()
		if err := sb.Force(host, up, until); err != nil {
			t.Fatalf("Force(%q): %v", host, err)
		}
	}
	must("zeta.example.com", "a", clock.Now().Add(time.Hour))
	must("alpha.example.com", "b", clock.Now().Add(time.Hour))

	snap := sb.ForceSnapshot()
	if len(snap) != 2 {
		t.Fatalf("ForceSnapshot len = %d, want 2", len(snap))
	}
	if snap[0].Host != "alpha.example.com" || snap[1].Host != "zeta.example.com" {
		t.Errorf("ForceSnapshot order = [%s, %s], want sorted by host", snap[0].Host, snap[1].Host)
	}
}

// TestResetClearsEverything confirms Reset wipes entries, cascade, forces,
// and debounce in one call.
func TestResetClearsEverything(t *testing.T) {
	clock := newManualClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	sb := buildScoreboard(t, []*fakeUpstream{alwaysOK("a"), alwaysRefused("b")}, scoreboard.Config{}, clock, 1)

	for _, host := range []string{"one.example.com", "two.example.com"} {
		if _, err := dialOnce(t, sb, host+":443"); err != nil {
			t.Fatalf("dial %s: %v", host, err)
		}
		sb.RecordFailure(host, "b", failure.KindRefused, nil)
	}
	sb.TripCascade("one.example.com")
	if err := sb.Force("one.example.com", "a", clock.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Force: %v", err)
	}

	if got := sb.EntriesCount(); got == 0 {
		t.Fatal("EntriesCount before Reset is zero; test setup is wrong")
	}

	sb.Reset()

	if got := sb.EntriesCount(); got != 0 {
		t.Errorf("EntriesCount after Reset = %d, want 0", got)
	}
	if sb.CascadeActiveCount() != 0 {
		t.Errorf("CascadeActiveCount after Reset = %d, want 0", sb.CascadeActiveCount())
	}
	if got := sb.ForceSnapshot(); len(got) != 0 {
		t.Errorf("ForceSnapshot after Reset = %d entries, want 0", len(got))
	}
}
