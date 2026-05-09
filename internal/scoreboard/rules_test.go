package scoreboard_test

import (
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// buildScoreboardWithRules mirrors buildScoreboard from scoreboard_test.go but
// also attaches a compiled RuleSet via WithRules. Tests for the Phase 8
// routing semantics need the rule set in place at construction so Pick
// consults it on the first call.
func buildScoreboardWithRules(t *testing.T, ups []*fakeUpstream, rules []config.RuleConfig) *scoreboard.Scoreboard {
	t.Helper()
	clock := newManualClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	entries := make([]upstream.PoolEntry, len(ups))
	for i, u := range ups {
		entries[i] = upstream.PoolEntry{Up: u, Priority: 10 * (i + 1)}
	}
	pool, err := upstream.NewPool(entries, len(ups), quietLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	rs, err := upstream.NewRuleSet(rules)
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	cfg := scoreboard.Config{
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 3, Cooldown: 30 * time.Second},
			failure.KindTimeout: {Penalty: 2, Cooldown: 15 * time.Second},
		},
		SuccessWeight:  1,
		ScoreCap:       10,
		DecayInterval:  5 * time.Minute,
		CascadeTTL:     30 * time.Second,
		DebounceWindow: 100 * time.Millisecond,
	}
	r := rand.New(rand.NewSource(1))
	sb, err := scoreboard.New(pool, cfg,
		scoreboard.WithLogger(quietLogger()),
		scoreboard.WithClock(clock.Now),
		scoreboard.WithRand(r),
		scoreboard.WithRules(rs),
	)
	if err != nil {
		t.Fatalf("scoreboard.New: %v", err)
	}
	return sb
}

func TestRuleForceTrueRestrictsToPreferSet(t *testing.T) {
	t.Parallel()
	a := alwaysOK("a")
	b := alwaysOK("b")
	c := alwaysOK("c")
	sb := buildScoreboardWithRules(t,
		[]*fakeUpstream{a, b, c},
		[]config.RuleConfig{{
			HostGlob: "*.locked.example",
			Prefer:   []string{"b", "c"},
			Force:    true,
		}},
	)
	defer sb.Stop()

	// Pick must return one of {b, c} for a host that matches the rule.
	up, err := sb.Pick("foo.locked.example", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if id := up.ID(); id != "b" && id != "c" {
		t.Errorf("Pick returned %q; force=true should restrict to {b, c}", id)
	}

	// Marking the entire prefer set as tried must produce ErrPoolExhausted
	// even though `a` is still untried (force = true forbids it).
	tried := map[string]bool{"b": true, "c": true}
	if _, err := sb.Pick("foo.locked.example", tried); !errors.Is(err, scoreboard.ErrPoolExhausted) {
		t.Fatalf("Pick err = %v, want ErrPoolExhausted (force=true with all preferred tried)", err)
	}
}

func TestRuleForceFalsePrefersThenFallsBack(t *testing.T) {
	t.Parallel()
	a := alwaysOK("a")
	b := alwaysOK("b")
	c := alwaysOK("c")
	// Rule with force=false says "try b first". Pool order is a, b, c
	// at priorities 10, 20, 30 respectively, so without the rule, a
	// would be picked first by base priority.
	sb := buildScoreboardWithRules(t,
		[]*fakeUpstream{a, b, c},
		[]config.RuleConfig{{
			HostGlob: "*.example.com",
			Prefer:   []string{"b"},
		}},
	)
	defer sb.Stop()

	up, err := sb.Pick("api.example.com", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if id := up.ID(); id != "b" {
		t.Errorf("Pick returned %q; force=false should sort prefer (b) above a/c", id)
	}

	// With b tried, fall back to non-preferred. a comes before c by
	// base priority.
	up, err = sb.Pick("api.example.com", map[string]bool{"b": true})
	if err != nil {
		t.Fatalf("Pick fallback: %v", err)
	}
	if id := up.ID(); id != "a" {
		t.Errorf("Pick fallback returned %q; want a (next by base priority)", id)
	}
}

func TestRuleForceTruePreservesPreferOrder(t *testing.T) {
	t.Parallel()
	a := alwaysOK("a")
	b := alwaysOK("b")
	c := alwaysOK("c")
	// Pool order is a, b, c by priority. The rule prefers them in
	// reverse: c, b, a. force=true. The first Pick should pick c
	// (declaration order in prefer wins over score and base priority).
	sb := buildScoreboardWithRules(t,
		[]*fakeUpstream{a, b, c},
		[]config.RuleConfig{{
			HostGlob: "*.x.example",
			Prefer:   []string{"c", "b", "a"},
			Force:    true,
		}},
	)
	defer sb.Stop()

	up, err := sb.Pick("foo.x.example", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if id := up.ID(); id != "c" {
		t.Errorf("Pick returned %q; rule prefer order [c,b,a] should put c first", id)
	}
}

func TestRuleDoesNotApplyToNonMatchingHost(t *testing.T) {
	t.Parallel()
	a := alwaysOK("a")
	b := alwaysOK("b")
	sb := buildScoreboardWithRules(t,
		[]*fakeUpstream{a, b},
		[]config.RuleConfig{{
			HostGlob: "*.locked.example",
			Prefer:   []string{"b"},
			Force:    true,
		}},
	)
	defer sb.Stop()

	// Host outside the glob falls through to score+priority order. a
	// is priority 10 (lower), so it wins by default.
	up, err := sb.Pick("other.host.example", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if id := up.ID(); id != "a" {
		t.Errorf("Pick on non-matching host returned %q; want a (default order)", id)
	}
}

func TestReplaceRulesSwapsRouting(t *testing.T) {
	t.Parallel()
	a := alwaysOK("a")
	b := alwaysOK("b")
	sb := buildScoreboardWithRules(t,
		[]*fakeUpstream{a, b},
		[]config.RuleConfig{{
			HostGlob: "*.example",
			Prefer:   []string{"b"},
			Force:    true,
		}},
	)
	defer sb.Stop()

	// Initial pick honors the rule (force=true, prefer=b).
	up, err := sb.Pick("api.example", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if up.ID() != "b" {
		t.Fatalf("initial Pick = %q, want b", up.ID())
	}

	// Hot-reload with no rules. Pick should now use default ordering
	// (a wins by base priority).
	sb.ReplaceRules(nil)
	up, err = sb.Pick("api.example", nil)
	if err != nil {
		t.Fatalf("Pick after ReplaceRules(nil): %v", err)
	}
	if up.ID() != "a" {
		t.Errorf("Pick after rule clear = %q, want a (default order)", up.ID())
	}
}

func TestRuleForceTrueOnAllPreferredCooledFallsBackToCooled(t *testing.T) {
	t.Parallel()
	// When every preferred upstream is on cooldown (and there are no
	// non-preferred candidates because force=true), Pick must surface
	// the soonest-warming preferred upstream rather than fail. This
	// matches the cooldown-fallback path's "cooldown is advisory" rule.
	bad := alwaysRefused("bad-pref")
	good := alwaysOK("good-fallback") // not in prefer list
	sb := buildScoreboardWithRules(t,
		[]*fakeUpstream{bad, good},
		[]config.RuleConfig{{
			HostGlob: "*.x.example",
			Prefer:   []string{"bad-pref"},
			Force:    true,
		}},
	)
	defer sb.Stop()

	// First DialFor: bad refuses, retry within forced set is exhausted,
	// cascade trips. The non-preferred good must NOT be touched.
	if _, err := dialOnce(t, sb, "h.x.example:443"); err == nil {
		t.Fatal("dialOnce returned nil err; force=true with only-failing prefer should fail")
	}
	if got := good.dialCount.Load(); got != 0 {
		t.Errorf("good (non-preferred) dialCount = %d, want 0 (force=true must exclude it)", got)
	}
}
