package upstream

import (
	"strings"
	"testing"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

func TestRuleSetMatchHostGlobCases(t *testing.T) {
	t.Parallel()
	rs, err := NewRuleSet([]config.RuleConfig{
		{HostGlob: "*.bbc.co.uk", Prefer: []string{"a"}, Force: true},
		{HostGlob: "*.itch.io", Prefer: []string{"b"}},
		{HostGlob: "exact.example", Prefer: []string{"c"}},
	})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	cases := []struct {
		host   string
		wantID string // first prefer id of the matched rule, "" for nil
	}{
		{"news.bbc.co.uk", "a"},
		{"WEATHER.BBC.CO.UK", "a"},
		{"a.b.c.bbc.co.uk", "a"},
		{"www.itch.io", "b"},
		{"exact.example", "c"},
		{"nothing.matches.example.org", ""},
		{"bbc.co.uk", ""}, // bare apex is not a *.bbc.co.uk match
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			r := rs.Match(tc.host)
			if tc.wantID == "" {
				if r != nil {
					t.Fatalf("Match(%q) = %+v, want nil", tc.host, r)
				}
				return
			}
			if r == nil {
				t.Fatalf("Match(%q) = nil, want rule with prefer[0] = %q", tc.host, tc.wantID)
			}
			if r.Prefer[0] != tc.wantID {
				t.Fatalf("Match(%q).Prefer[0] = %q, want %q", tc.host, r.Prefer[0], tc.wantID)
			}
		})
	}
}

func TestRuleSetFirstMatchWins(t *testing.T) {
	t.Parallel()
	// Two overlapping rules: the first one declared should match a host
	// that matches both globs.
	rs, err := NewRuleSet([]config.RuleConfig{
		{HostGlob: "*.bbc.co.uk", Prefer: []string{"specific"}, Force: true},
		{HostGlob: "*.co.uk", Prefer: []string{"general"}},
	})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	r := rs.Match("news.bbc.co.uk")
	if r == nil {
		t.Fatal("Match returned nil; expected first rule to match")
	}
	if r.Prefer[0] != "specific" {
		t.Errorf("Prefer[0] = %q, want %q (first-match-wins)", r.Prefer[0], "specific")
	}
	if !r.Force {
		t.Error("Force = false, want true (from first rule)")
	}
}

func TestRuleSetCompilesBodyRegex(t *testing.T) {
	t.Parallel()
	rs, err := NewRuleSet([]config.RuleConfig{
		{
			HostGlob:  "*.geo-block.example",
			Prefer:    []string{"alt"},
			BodyRegex: []string{"(?i)content not available", "region.?lock"},
		},
	})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	r := rs.Match("foo.geo-block.example")
	if !r.HasBodyRegex() {
		t.Fatal("HasBodyRegex = false, want true")
	}
	if got := len(r.BodyRegex); got != 2 {
		t.Fatalf("len(BodyRegex) = %d, want 2", got)
	}
	matched, pat := r.MatchBody([]byte("Sorry, this content not available in your country."))
	if !matched {
		t.Fatal("MatchBody = false, want true")
	}
	if !strings.Contains(pat, "content not available") {
		t.Errorf("pattern = %q, want one mentioning 'content not available'", pat)
	}
	matched, _ = r.MatchBody([]byte("everything is fine"))
	if matched {
		t.Error("MatchBody on benign body returned true")
	}
}

func TestRuleSetCompileErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		cfgs  []config.RuleConfig
		match string
	}{
		{
			name:  "empty host_glob",
			cfgs:  []config.RuleConfig{{HostGlob: "", Prefer: []string{"x"}}},
			match: "empty host_glob",
		},
		{
			name:  "malformed glob",
			cfgs:  []config.RuleConfig{{HostGlob: "[bad", Prefer: []string{"x"}}},
			match: "[bad",
		},
		{
			name:  "empty prefer",
			cfgs:  []config.RuleConfig{{HostGlob: "*.x", Prefer: nil}},
			match: "prefer must list at least one upstream id",
		},
		{
			name:  "empty body regex entry",
			cfgs:  []config.RuleConfig{{HostGlob: "*.x", Prefer: []string{"x"}, BodyRegex: []string{""}}},
			match: "is empty",
		},
		{
			name:  "uncompilable body regex",
			cfgs:  []config.RuleConfig{{HostGlob: "*.x", Prefer: []string{"x"}, BodyRegex: []string{"[bad"}}},
			match: "body_regex[0]",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRuleSet(tc.cfgs)
			if err == nil {
				t.Fatalf("expected error matching %q, got nil", tc.match)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.match)
			}
		})
	}
}

func TestRuleSetNilSafe(t *testing.T) {
	t.Parallel()
	var rs *RuleSet
	if r := rs.Match("anything"); r != nil {
		t.Errorf("Match on nil set = %+v, want nil", r)
	}
	if rs.Len() != 0 {
		t.Error("Len on nil set != 0")
	}
	if err := rs.CheckPreferIDs(nil); err != nil {
		t.Errorf("CheckPreferIDs on nil set = %v, want nil", err)
	}
}

func TestRuleSetCheckPreferIDs(t *testing.T) {
	t.Parallel()
	rs, err := NewRuleSet([]config.RuleConfig{
		{HostGlob: "*.a", Prefer: []string{"known", "missing-1"}},
		{HostGlob: "*.b", Prefer: []string{"missing-2"}},
	})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	known := map[string]struct{}{"known": {}}
	err = rs.CheckPreferIDs(known)
	if err == nil {
		t.Fatal("CheckPreferIDs returned nil; expected joined error")
	}
	for _, want := range []string{"missing-1", "missing-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	// All ids known: nil error.
	if err := rs.CheckPreferIDs(map[string]struct{}{
		"known":     {},
		"missing-1": {},
		"missing-2": {},
	}); err != nil {
		t.Errorf("CheckPreferIDs with full set = %v, want nil", err)
	}
}
