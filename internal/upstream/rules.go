// rules.go owns the per-host routing layer Tunnelsmith introduced in
// Phase 8. A RuleSet compiles host globs and body-match patterns from
// config and exposes a single Match call the listener and scoreboard
// share. Rules are evaluated in declaration order; the first matching
// rule wins and the rest are not considered.
//
// Routing semantics:
//   - prefer is the ordered list of upstream ids the rule wants to try
//     first for the matching host.
//   - force=true restricts upstream selection to the prefer list. When
//     every preferred upstream has been tried for one request, the
//     scoreboard surfaces ErrPoolExhausted (forced cascade) rather than
//     falling back to the global pool.
//   - force=false uses prefer as a sort hint only; preferred upstreams
//     come first by declaration order, the rest of the pool follows in
//     normal score order, and a request can fall back to non-preferred
//     upstreams when the preferred set is exhausted.
//
// Body inspection: BodyRegex lists pre-compiled patterns the listener
// runs against the buffered prefix of a plain-HTTP response. A match
// is treated as a soft upstream failure (failure.KindBodyMatch). The
// listener wires this through; this package only carries the patterns
// and the matching helper.
//
// RuleSet is read-only after construction. The hot-reload path swaps
// the pointer atomically; readers always see one self-consistent set.

package upstream

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

// Rule is one compiled [[rule]] block. Glob is the lowercased host
// pattern in path.Match form; Prefer is the user's ordered upstream
// id list; PreferRank is a derived map[id]position lookup (1-based
// so 0 cleanly means "not in this rule's prefer list") so the
// scoreboard's Pick can read O(1) ranks without rebuilding the map
// per request; Force toggles the strict-membership behavior described
// in the package doc; BodyRegex is the compiled set of patterns that
// fire KindBodyMatch on a positive match.
type Rule struct {
	Glob       string
	Prefer     []string
	PreferRank map[string]int
	Force      bool
	BodyRegex  []*regexp.Regexp
}

// HasBodyRegex reports whether this rule asked the listener to buffer
// and inspect response bodies for matching hosts. The fast-path check
// the listener uses to decide whether to spend any cycles on body
// detection at all.
func (r *Rule) HasBodyRegex() bool {
	return r != nil && len(r.BodyRegex) > 0
}

// MatchBody runs each compiled pattern against buf and returns the
// first match's source pattern. Returns matched=false when no pattern
// matched. Caller passes the already-buffered prefix; this method does
// not read from any io.Reader.
func (r *Rule) MatchBody(buf []byte) (matched bool, pattern string) {
	if r == nil {
		return false, ""
	}
	for _, re := range r.BodyRegex {
		if re == nil {
			continue
		}
		if re.Match(buf) {
			return true, re.String()
		}
	}
	return false, ""
}

// RuleSet is the compiled view of every [[rule]] block in config order.
// Match returns the first rule whose host_glob matches the lowercased
// host, or nil if no rule applies.
type RuleSet struct {
	rules []*Rule
}

// NewRuleSet compiles cfgs in declaration order. Compile errors include
// the rule index and pattern index so a misconfigured rule is easy to
// fix from the log line. config.Validate already runs each glob and
// regex through compile checks at load time; NewRuleSet repeats the
// work because the compiled regex slice itself is the artifact the
// listener needs at request time.
func NewRuleSet(cfgs []config.RuleConfig) (*RuleSet, error) {
	out := &RuleSet{rules: make([]*Rule, 0, len(cfgs))}
	for i, c := range cfgs {
		if c.HostGlob == "" {
			return nil, fmt.Errorf("rules: rule[%d] has empty host_glob", i)
		}
		// Probe the pattern with an empty subject so a malformed glob
		// surfaces at construction time, not on the first request.
		// path.Match is the same matcher Match calls below.
		if _, err := path.Match(c.HostGlob, ""); err != nil {
			return nil, fmt.Errorf("rules: rule[%d] (host_glob=%q): %w", i, c.HostGlob, err)
		}
		// Prefer must be non-empty. config.Validate already enforces
		// this for the TOML path; the same check here defends against
		// programmatic callers (and tests) that build a RuleConfig
		// directly. A force=true rule with an empty prefer would force
		// every request to ErrPoolExhausted, which is worse than
		// rejecting the input at construction time.
		if len(c.Prefer) == 0 {
			return nil, fmt.Errorf("rules: rule[%d] (host_glob=%q): prefer must list at least one upstream id", i, c.HostGlob)
		}
		var compiled []*regexp.Regexp
		for j, pat := range c.BodyRegex {
			if pat == "" {
				return nil, fmt.Errorf("rules: rule[%d] (host_glob=%q): body_regex[%d] is empty", i, c.HostGlob, j)
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("rules: rule[%d] (host_glob=%q): body_regex[%d]: %w", i, c.HostGlob, j, err)
			}
			compiled = append(compiled, re)
		}
		// Copy Prefer so caller mutations to the source slice cannot
		// race with request-path readers later. Pre-build the
		// PreferRank lookup at compile time: positions are 1-based so
		// the zero value of an int (returned by a missing-key lookup)
		// cleanly means "not in this rule's prefer list".
		prefer := append([]string(nil), c.Prefer...)
		preferRank := make(map[string]int, len(prefer))
		for j, id := range prefer {
			if _, dup := preferRank[id]; dup {
				continue
			}
			preferRank[id] = j + 1
		}
		out.rules = append(out.rules, &Rule{
			Glob:       strings.ToLower(c.HostGlob),
			Prefer:     prefer,
			PreferRank: preferRank,
			Force:      c.Force,
			BodyRegex:  compiled,
		})
	}
	return out, nil
}

// Match returns the first rule whose host_glob matches host (case
// insensitive), or nil if no rule applies. host is normalized to lower
// case before matching so callers do not have to.
func (rs *RuleSet) Match(host string) *Rule {
	if rs == nil || len(rs.rules) == 0 {
		return nil
	}
	h := strings.ToLower(host)
	for _, r := range rs.rules {
		ok, _ := path.Match(r.Glob, h)
		if ok {
			return r
		}
	}
	return nil
}

// Rules returns the compiled rules in declaration order. Callers must
// treat the returned slice as read-only; mutating it would race with
// every concurrent Match.
func (rs *RuleSet) Rules() []*Rule {
	if rs == nil {
		return nil
	}
	out := make([]*Rule, len(rs.rules))
	copy(out, rs.rules)
	return out
}

// Len reports how many rules are compiled in this set. Useful for log
// lines on hot-reload that want to mention the new rule count without
// dereferencing a slice.
func (rs *RuleSet) Len() int {
	if rs == nil {
		return 0
	}
	return len(rs.rules)
}

// CheckPreferIDs verifies every Prefer id in every rule is present in
// known. Used by cmd/tunnelsmith after the upstream pool expands at
// startup, where pool-derived ids are now known. Returns one joined
// error covering every mismatch so operators see all typos at once.
func (rs *RuleSet) CheckPreferIDs(known map[string]struct{}) error {
	if rs == nil {
		return nil
	}
	var errs []error
	for i, r := range rs.rules {
		for _, id := range r.Prefer {
			if _, ok := known[id]; !ok {
				errs = append(errs, fmt.Errorf("rule[%d] (host_glob=%q): prefer references unknown upstream id %q", i, r.Glob, id))
			}
		}
	}
	return errors.Join(errs...)
}
