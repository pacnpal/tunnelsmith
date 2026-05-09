// Package config loads, validates, and applies defaults to Tunnelsmith's
// TOML configuration. Every key, default, and validation rule is documented
// in docs/configuration.md.
package config

import (
	"encoding"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration wraps time.Duration so it can be parsed from TOML strings such
// as "15m" or "120s" via encoding.TextUnmarshaler.
type Duration time.Duration

// UnmarshalText parses a duration string per time.ParseDuration.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", string(text), err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalText returns the duration in time.Duration's canonical string form.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// Duration returns the underlying time.Duration value.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

var (
	_ encoding.TextUnmarshaler = (*Duration)(nil)
	_ encoding.TextMarshaler   = Duration(0)
)

// UpstreamKind enumerates the supported upstream egress mechanisms.
type UpstreamKind string

const (
	KindDirect UpstreamKind = "direct"
	KindHTTP   UpstreamKind = "http"
	KindSOCKS5 UpstreamKind = "socks5"
)

// Config is the top-level Tunnelsmith configuration.
type Config struct {
	Listener      ListenerConfig       `toml:"listener"`
	Cache         CacheConfig          `toml:"cache"`
	Metrics       MetricsConfig        `toml:"metrics"`
	UI            UIConfig             `toml:"ui"`
	Upstreams     []UpstreamConfig     `toml:"upstream"`
	UpstreamPools []UpstreamPoolConfig `toml:"upstream_pool"`
	Failure       FailureConfig        `toml:"failure"`
	Rules         []RuleConfig         `toml:"rule"`

	// UnknownKeys collects TOML keys that were present in the file but did
	// not map to any known struct field. Surfacing these as warnings helps
	// catch typos without breaking forward-compatibility for fields added
	// in later phases.
	UnknownKeys []string `toml:"-"`
}

// ListenerConfig declares which addresses the proxy listeners bind to.
type ListenerConfig struct {
	HTTP  string `toml:"http"`  // default: ":8080"
	SOCKS string `toml:"socks"` // default: ":1080"
}

// CacheConfig controls the decision-cache behavior. Only the structural
// settings live here; per-(host, upstream) scoring lands in Phase 4.
//
// PersistPath, when set, names the on-disk file the scoreboard snapshots
// state into so it survives a restart. The directory must exist; the file
// is created on the first successful write. PersistInterval drives the
// background snapshot ticker added in Phase 7. Setting persist_interval
// to "0s" disables periodic writes; a final write still runs at shutdown
// when PersistPath is set.
type CacheConfig struct {
	TTL             Duration `toml:"ttl"`              // default: 15m
	NegativeTTL     Duration `toml:"negative_ttl"`     // default: 1m
	PersistPath     string   `toml:"persist_path"`     // default: "" (in-memory only)
	PersistInterval Duration `toml:"persist_interval"` // default: 30s
}

// MetricsConfig controls the Prometheus exposition endpoint added in Phase 7.
// Bind sets the host:port the metrics HTTP listener serves on; setting it to
// the empty string disables the listener entirely. The endpoint always serves
// at /metrics.
type MetricsConfig struct {
	Bind string `toml:"bind"` // default: ":9090"; "" disables
}

// UIConfig controls the read-and-act web UI added in Phase 9. Bind sets the
// host:port the UI HTTP listener serves on. Setting Bind to the empty
// string disables the UI entirely; the static assets and API handlers are
// inert and the binary does not open the port. The UI exposes scoreboard
// state and four POST action endpoints (forget, force, reset, plus the
// scoreboard read). There is no auth on the port; the security boundary is
// the Docker / host network, documented in docs/ui.md.
type UIConfig struct {
	Bind string `toml:"bind"` // default: ":9091"; "" disables
}

// UpstreamConfig declares one egress option that the router can pick.
// Addr is required for kinds http and socks5 and ignored for kind direct.
//
// Priority is a *int so applyDefaults can tell "field omitted in TOML" (nil,
// gets defaulted to 100) from "user wrote priority = 0" (non-nil pointer to
// zero, kept as-is so the user can elect 0 as the highest-priority slot).
// After Parse, Priority is always non-nil; downstream code can dereference
// without a nil check.
type UpstreamConfig struct {
	ID       string       `toml:"id"`
	Kind     UpstreamKind `toml:"kind"`
	Addr     string       `toml:"addr"`
	Priority *int         `toml:"priority,omitempty"` // default: 100
}

// PriorityValue returns the resolved priority. After Parse the field is
// always populated; this helper exists for code paths that build an
// UpstreamConfig by hand without going through Parse.
func (u UpstreamConfig) PriorityValue() int {
	if u.Priority == nil {
		return 100
	}
	return *u.Priority
}

// UpstreamPoolKind enumerates the providers an [[upstream_pool]] block can
// expand. Only "mullvad" is implemented in Phase 6.
type UpstreamPoolKind string

const (
	UpstreamPoolMullvad UpstreamPoolKind = "mullvad"
)

// UpstreamPoolConfig declares a synthetic group of upstreams the binary
// will expand at startup. Phase 6 only knows how to expand provider
// "mullvad", which fans the configured countries out into one socks5
// upstream per active Mullvad WireGuard relay (see ADR-004).
//
// Priority and Refresh use *int / *Duration sentinels so applyDefaults
// can tell "field omitted" from "user wrote 0" / "user wrote 0s". After
// Parse both pointers are always populated.
type UpstreamPoolConfig struct {
	Provider        UpstreamPoolKind `toml:"provider"`
	IDPrefix        string           `toml:"id_prefix"`
	Priority        *int             `toml:"priority,omitempty"` // default: 200
	Countries       []string         `toml:"countries"`          // required, non-empty
	IncludeInactive bool             `toml:"include_inactive"`   // default: false
	Refresh         *Duration        `toml:"refresh,omitempty"`  // default: 12h
	CachePath       string           `toml:"cache_path"`         // default: "" (no cache)
}

// PriorityValue returns the resolved priority for the synthetic upstreams
// produced by this pool block. Defaults to 200 so user-defined [[upstream]]
// entries (default 100) take precedence over auto-discovered relays.
func (p UpstreamPoolConfig) PriorityValue() int {
	if p.Priority == nil {
		return 200
	}
	return *p.Priority
}

// RefreshDuration returns the resolved refresh interval as a time.Duration.
// Defaults to 12h.
func (p UpstreamPoolConfig) RefreshDuration() time.Duration {
	if p.Refresh == nil {
		return 12 * time.Hour
	}
	return p.Refresh.Duration()
}

// StatusRule says how a single HTTP status code maps to a failure-detection
// outcome. The defaults for 429, 403, and 451 are populated in
// applyDefaults when the user omits the [[failure.status]] section.
type StatusRule struct {
	Code            int      `toml:"code"`
	Penalty         int      `toml:"penalty"`
	Cooldown        Duration `toml:"cooldown"`
	HonorRetryAfter bool     `toml:"honor_retry_after"` // default: false
}

// FailureConfig collects the user's failure-detection preferences. Bool and
// numeric fields are defaulted via toml.MetaData.IsDefined rather than
// "value == zero", so a user-provided 0 reaches Validate (and rightly fails)
// instead of being silently replaced by the default.
//
// BodyRegex is parsed for forward compatibility but no longer applied;
// Phase 8 moved body-regex detection to the per-rule BodyRegex field so
// buffering only fires for hosts whose [[rule]] opts in. A non-empty
// top-level body_regex produces a startup warning so operators move it.
type FailureConfig struct {
	ConnectionRefused    bool          `toml:"connection_refused"`      // default: true; user can opt out by setting connection_refused = false
	TimeoutMS            int           `toml:"timeout_ms"`              // default: 8000
	BodyRegex            []string      `toml:"body_regex"`              // deprecated: use [[rule]].body_regex instead
	BodyBufferKB         int           `toml:"body_buffer_kb"`          // default: 32; per-response cap on bytes buffered for regex inspection
	MaxRetriesPerRequest int           `toml:"max_retries_per_request"` // default: 5
	Status               []StatusRule  `toml:"status"`                  // default: see DefaultStatusRules
	Scoring              ScoringConfig `toml:"scoring"`                 // default: see ScoringDefaults
}

// ScoringConfig drives the per-(host, upstream) scoreboard introduced in
// Phase 4. Penalty values are positive numbers that the scoreboard subtracts
// from the relevant entry's score; cooldown values name how long the
// affected (host, upstream) pair sits out the rotation after a failure of
// that kind.
//
// Phase 4 fires the refused and timeout policies from the dial path. The
// rate-limit, forbidden, and legal-block kinds are populated from
// [[failure.status]] entries (Phase 5 wires them through the listener);
// body-match policy is left for Phase 8. ScoringConfig only carries the
// kinds that do not have a natural home elsewhere in the config.
type ScoringConfig struct {
	RefusedPenalty  float64  `toml:"refused_penalty"`  // default: 3
	RefusedCooldown Duration `toml:"refused_cooldown"` // default: 30s
	TimeoutPenalty  float64  `toml:"timeout_penalty"`  // default: 2
	TimeoutCooldown Duration `toml:"timeout_cooldown"` // default: 15s

	SuccessWeight float64 `toml:"success_weight"` // default: 1
	ScoreCap      float64 `toml:"score_cap"`      // default: 10

	ProbeChance float64 `toml:"probe_chance"` // default: 0.05

	DecayInterval Duration `toml:"decay_interval"` // default: 5m
	DecayStep     float64  `toml:"decay_step"`     // default: 0.5

	CascadeTTL     Duration `toml:"cascade_ttl"`     // default: 30s
	DebounceWindow Duration `toml:"debounce_window"` // default: 100ms

	// BodyMatchPenalty and BodyMatchCooldown drive the scoreboard's
	// response to a Phase 8 body-regex match. The default penalty (5)
	// sits between timeout (2) and forbidden (6), mirroring the
	// proposal's "soft block" framing; the default cooldown (60s) is
	// short enough to retry quickly when the match was a transient
	// challenge page and long enough to skip a host that consistently
	// returns the configured pattern from this exit.
	BodyMatchPenalty  float64  `toml:"body_match_penalty"`  // default: 5
	BodyMatchCooldown Duration `toml:"body_match_cooldown"` // default: 60s

	// PruneAfter governs when zero-score entries are dropped from the
	// scoreboard during the persistence-tick prune pass. An entry whose
	// score has decayed to zero and whose lastSeen is older than
	// PruneAfter is removed; expired cascade entries and stale debounce
	// keys are pruned alongside it. Set to "0s" to disable pruning.
	PruneAfter Duration `toml:"prune_after"` // default: 24h
}

// ScoringDefaults captures the proposal's recommended scoreboard tuning.
// Used when [failure.scoring] is omitted entirely; per-key defaulting kicks
// in for partial sections so a user can override one field without having
// to restate the rest.
var ScoringDefaults = ScoringConfig{
	RefusedPenalty:    3,
	RefusedCooldown:   Duration(30 * time.Second),
	TimeoutPenalty:    2,
	TimeoutCooldown:   Duration(15 * time.Second),
	SuccessWeight:     1,
	ScoreCap:          10,
	ProbeChance:       0.05,
	DecayInterval:     Duration(5 * time.Minute),
	DecayStep:         0.5,
	CascadeTTL:        Duration(30 * time.Second),
	DebounceWindow:    Duration(100 * time.Millisecond),
	BodyMatchPenalty:  5,
	BodyMatchCooldown: Duration(60 * time.Second),
	PruneAfter:        Duration(24 * time.Hour),
}

// RuleConfig declares a per-host routing override. Phase 8 adds BodyRegex
// for response-body inspection: when non-empty, plain-HTTP responses for
// matching hosts buffer up to FailureConfig.BodyBufferKB and run each
// pattern against the buffered prefix. A match is treated as a soft
// upstream failure (KindBodyMatch) and the request retries through the
// next-best upstream. CONNECT and SOCKS5 traffic bypasses body inspection
// because the proxy cannot see TLS payloads.
type RuleConfig struct {
	HostGlob  string   `toml:"host_glob"`
	Prefer    []string `toml:"prefer"`
	Force     bool     `toml:"force"`      // default: false
	BodyRegex []string `toml:"body_regex"` // default: []; opt-in per rule, plain-HTTP only
}

// DefaultStatusRules captures the proposal's recommended status-code
// handling. Used when the user omits [[failure.status]] entirely. If the
// user defines any status rules, they are taken as the complete list and
// these defaults are not merged in.
var DefaultStatusRules = []StatusRule{
	{Code: 429, Penalty: 4, Cooldown: Duration(120 * time.Second), HonorRetryAfter: true},
	{Code: 403, Penalty: 6, Cooldown: Duration(30 * time.Minute)},
	{Code: 451, Penalty: 8, Cooldown: Duration(6 * time.Hour)},
}

// Load reads, parses, defaults, and validates the TOML config at path.
// A non-nil error is fatal; the caller should exit.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data, path)
}

// Parse is Load without the file read, useful for tests.
func Parse(data []byte, sourceLabel string) (*Config, error) {
	var cfg Config
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", sourceLabel, err)
	}
	for _, key := range md.Undecoded() {
		cfg.UnknownKeys = append(cfg.UnknownKeys, key.String())
	}
	cfg.applyDefaults(md)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", sourceLabel, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults(md toml.MetaData) {
	if !md.IsDefined("listener", "http") {
		c.Listener.HTTP = ":8080"
	}
	if !md.IsDefined("listener", "socks") {
		c.Listener.SOCKS = ":1080"
	}
	if !md.IsDefined("cache", "ttl") {
		c.Cache.TTL = Duration(15 * time.Minute)
	}
	if !md.IsDefined("cache", "negative_ttl") {
		c.Cache.NegativeTTL = Duration(time.Minute)
	}
	if !md.IsDefined("cache", "persist_interval") {
		c.Cache.PersistInterval = Duration(30 * time.Second)
	}
	if !md.IsDefined("metrics", "bind") {
		c.Metrics.Bind = ":9090"
	}
	if !md.IsDefined("ui", "bind") {
		c.UI.Bind = ":9091"
	}
	if !md.IsDefined("failure", "timeout_ms") {
		c.Failure.TimeoutMS = 8000
	}
	if !md.IsDefined("failure", "max_retries_per_request") {
		c.Failure.MaxRetriesPerRequest = 5
	}
	if !md.IsDefined("failure", "connection_refused") {
		c.Failure.ConnectionRefused = true
	}
	if !md.IsDefined("failure", "body_regex") {
		c.Failure.BodyRegex = []string{}
	}
	if !md.IsDefined("failure", "body_buffer_kb") {
		c.Failure.BodyBufferKB = 32
	}
	if !md.IsDefined("failure", "status") {
		c.Failure.Status = append([]StatusRule(nil), DefaultStatusRules...)
	}
	c.applyScoringDefaults(md)
	// IsDefined cannot single out individual array-of-tables entries, so
	// per-upstream priority uses a *int sentinel: nil means the user
	// omitted priority for this entry; non-nil (including 0) is verbatim
	// from the TOML.
	for i := range c.Upstreams {
		if c.Upstreams[i].Priority == nil {
			v := 100
			c.Upstreams[i].Priority = &v
		}
	}
	// [[upstream_pool]] uses the same sentinel pattern for priority and
	// refresh. Defaults: priority 200 (so synthetic pool upstreams sit
	// below user-defined upstreams), refresh 12h.
	for i := range c.UpstreamPools {
		if c.UpstreamPools[i].Priority == nil {
			v := 200
			c.UpstreamPools[i].Priority = &v
		}
		if c.UpstreamPools[i].Refresh == nil {
			d := Duration(12 * time.Hour)
			c.UpstreamPools[i].Refresh = &d
		}
	}
}

// applyScoringDefaults fills any [failure.scoring] field the user omitted
// with its ScoringDefaults value. Per-key IsDefined checks let a user
// override one knob without having to restate the rest of the section.
func (c *Config) applyScoringDefaults(md toml.MetaData) {
	s := &c.Failure.Scoring
	d := ScoringDefaults
	if !md.IsDefined("failure", "scoring", "refused_penalty") {
		s.RefusedPenalty = d.RefusedPenalty
	}
	if !md.IsDefined("failure", "scoring", "refused_cooldown") {
		s.RefusedCooldown = d.RefusedCooldown
	}
	if !md.IsDefined("failure", "scoring", "timeout_penalty") {
		s.TimeoutPenalty = d.TimeoutPenalty
	}
	if !md.IsDefined("failure", "scoring", "timeout_cooldown") {
		s.TimeoutCooldown = d.TimeoutCooldown
	}
	if !md.IsDefined("failure", "scoring", "success_weight") {
		s.SuccessWeight = d.SuccessWeight
	}
	if !md.IsDefined("failure", "scoring", "score_cap") {
		s.ScoreCap = d.ScoreCap
	}
	if !md.IsDefined("failure", "scoring", "probe_chance") {
		s.ProbeChance = d.ProbeChance
	}
	if !md.IsDefined("failure", "scoring", "decay_interval") {
		s.DecayInterval = d.DecayInterval
	}
	if !md.IsDefined("failure", "scoring", "decay_step") {
		s.DecayStep = d.DecayStep
	}
	if !md.IsDefined("failure", "scoring", "cascade_ttl") {
		s.CascadeTTL = d.CascadeTTL
	}
	if !md.IsDefined("failure", "scoring", "debounce_window") {
		s.DebounceWindow = d.DebounceWindow
	}
	if !md.IsDefined("failure", "scoring", "body_match_penalty") {
		s.BodyMatchPenalty = d.BodyMatchPenalty
	}
	if !md.IsDefined("failure", "scoring", "body_match_cooldown") {
		s.BodyMatchCooldown = d.BodyMatchCooldown
	}
	if !md.IsDefined("failure", "scoring", "prune_after") {
		s.PruneAfter = d.PruneAfter
	}
}

// Validate runs every check the build plan calls for. Errors are joined so
// the caller sees every problem at once instead of fixing one at a time.
func (c *Config) Validate() error {
	var errs []error

	if err := validateAddr(c.Listener.HTTP, "listener.http"); err != nil {
		errs = append(errs, err)
	}
	if err := validateAddr(c.Listener.SOCKS, "listener.socks"); err != nil {
		errs = append(errs, err)
	}

	if c.Cache.PersistPath != "" {
		if !filepath.IsAbs(c.Cache.PersistPath) {
			errs = append(errs, fmt.Errorf("cache.persist_path %q must be an absolute path", c.Cache.PersistPath))
		}
	}
	if c.Cache.PersistInterval < 0 {
		errs = append(errs, fmt.Errorf("cache.persist_interval must be >= 0, got %v", c.Cache.PersistInterval.Duration()))
	}

	if c.Metrics.Bind != "" {
		if err := validateAddr(c.Metrics.Bind, "metrics.bind"); err != nil {
			errs = append(errs, err)
		}
	}

	if c.UI.Bind != "" {
		if err := validateAddr(c.UI.Bind, "ui.bind"); err != nil {
			errs = append(errs, err)
		}
	}

	if len(c.Upstreams) == 0 && len(c.UpstreamPools) == 0 {
		errs = append(errs, errors.New("at least one [[upstream]] or [[upstream_pool]] must be defined"))
	}
	seenIDs := make(map[string]int, len(c.Upstreams))
	for i, u := range c.Upstreams {
		idx := fmt.Sprintf("upstream[%d]", i)
		if u.ID == "" {
			errs = append(errs, fmt.Errorf("%s: id is required", idx))
		} else if prev, ok := seenIDs[u.ID]; ok {
			errs = append(errs, fmt.Errorf("%s: duplicate upstream id %q (also at upstream[%d])", idx, u.ID, prev))
		} else {
			seenIDs[u.ID] = i
		}
		switch u.Kind {
		case KindDirect:
			if u.Addr != "" {
				errs = append(errs, fmt.Errorf("%s (id=%q): kind=direct must not set addr", idx, u.ID))
			}
		case KindHTTP, KindSOCKS5:
			if u.Addr == "" {
				errs = append(errs, fmt.Errorf("%s (id=%q): kind=%s requires addr", idx, u.ID, u.Kind))
			} else if err := validateUpstreamAddr(u.Addr, fmt.Sprintf("%s (id=%q)", idx, u.ID)); err != nil {
				errs = append(errs, err)
			}
		case "":
			errs = append(errs, fmt.Errorf("%s (id=%q): kind is required", idx, u.ID))
		default:
			errs = append(errs, fmt.Errorf("%s (id=%q): kind %q is not one of direct, http, socks5", idx, u.ID, u.Kind))
		}
	}

	if c.Failure.TimeoutMS < 1 {
		errs = append(errs, fmt.Errorf("failure.timeout_ms must be > 0, got %d", c.Failure.TimeoutMS))
	}
	if c.Failure.MaxRetriesPerRequest < 1 {
		errs = append(errs, fmt.Errorf("failure.max_retries_per_request must be >= 1, got %d", c.Failure.MaxRetriesPerRequest))
	}
	// body_buffer_kb bounds the per-response read used for body-regex
	// inspection. 0 disables inspection regardless of [[rule]].body_regex
	// entries (the listener's WithHTTPBodyBufferKB / ReloadBodyBufferKB
	// already treat 0 as off, so the config value passes through). The
	// upper bound caps a misconfigured value at 1024 KiB so it cannot
	// pin a multi-megabyte buffer per concurrent request, well below
	// the 8 MiB request-body cap so the two limits cannot be confused.
	if c.Failure.BodyBufferKB < 0 {
		errs = append(errs, fmt.Errorf("failure.body_buffer_kb must be >= 0, got %d", c.Failure.BodyBufferKB))
	} else if c.Failure.BodyBufferKB > 1024 {
		errs = append(errs, fmt.Errorf("failure.body_buffer_kb must be <= 1024, got %d", c.Failure.BodyBufferKB))
	}
	for i, s := range c.Failure.Status {
		if s.Code < 100 || s.Code > 599 {
			errs = append(errs, fmt.Errorf("failure.status[%d]: code %d is outside the 100-599 range", i, s.Code))
		}
		if s.Cooldown < 0 {
			errs = append(errs, fmt.Errorf("failure.status[%d] (code=%d): cooldown must be >= 0", i, s.Code))
		}
	}

	if err := c.Failure.Scoring.validate(); err != nil {
		errs = append(errs, err)
	}

	seenPoolPrefixes := make(map[string]int, len(c.UpstreamPools))
	for i, p := range c.UpstreamPools {
		idx := fmt.Sprintf("upstream_pool[%d]", i)
		switch p.Provider {
		case UpstreamPoolMullvad:
		case "":
			errs = append(errs, fmt.Errorf("%s: provider is required", idx))
		default:
			errs = append(errs, fmt.Errorf("%s: provider %q is not supported (only %q in this version)", idx, p.Provider, UpstreamPoolMullvad))
		}
		if p.IDPrefix == "" {
			errs = append(errs, fmt.Errorf("%s: id_prefix is required", idx))
		} else if prev, ok := seenPoolPrefixes[p.IDPrefix]; ok {
			errs = append(errs, fmt.Errorf("%s: duplicate id_prefix %q (also at upstream_pool[%d])", idx, p.IDPrefix, prev))
		} else {
			seenPoolPrefixes[p.IDPrefix] = i
		}
		if len(p.Countries) == 0 {
			errs = append(errs, fmt.Errorf("%s (id_prefix=%q): countries must list at least one country", idx, p.IDPrefix))
		}
		for j, c := range p.Countries {
			if strings.TrimSpace(c) == "" {
				errs = append(errs, fmt.Errorf("%s (id_prefix=%q): countries[%d] is empty or whitespace", idx, p.IDPrefix, j))
			}
		}
		// refresh = "0s" is allowed and means "disable periodic refresh"
		// (the expander's RunRefresh exits immediately when Refresh <= 0).
		// Any positive value below the 1m floor is almost certainly a typo
		// that would hammer Mullvad's public relay API, so reject those.
		// Negative values are nonsensical for an interval.
		if p.Refresh != nil {
			d := p.Refresh.Duration()
			if d < 0 {
				errs = append(errs, fmt.Errorf("%s (id_prefix=%q): refresh must be >= 0, got %v", idx, p.IDPrefix, d))
			} else if d > 0 && d < time.Minute {
				errs = append(errs, fmt.Errorf("%s (id_prefix=%q): refresh must be 0 (to disable) or >= 1m, got %v", idx, p.IDPrefix, d))
			}
		}
		if p.CachePath != "" && !filepath.IsAbs(p.CachePath) {
			errs = append(errs, fmt.Errorf("%s (id_prefix=%q): cache_path %q must be an absolute path", idx, p.IDPrefix, p.CachePath))
		}
	}

	for i, r := range c.Rules {
		if r.HostGlob == "" {
			errs = append(errs, fmt.Errorf("rule[%d]: host_glob is required", i))
		} else if _, err := path.Match(r.HostGlob, ""); err != nil {
			// path.Match validates the pattern lazily on the first match.
			// Probe with an empty subject so a malformed pattern surfaces
			// at config-load time instead of silently never matching at
			// request time.
			errs = append(errs, fmt.Errorf("rule[%d] (host_glob=%q): invalid glob: %w", i, r.HostGlob, err))
		}
		if len(r.Prefer) == 0 {
			errs = append(errs, fmt.Errorf("rule[%d] (host_glob=%q): prefer must list at least one upstream id", i, r.HostGlob))
		}
		// When [[upstream_pool]] blocks are present, the set of valid
		// upstream ids is not known until startup expansion. Defer the
		// prefer-id existence check to the binary's startup path
		// (cmd/tunnelsmith asserts it against the merged upstream list)
		// rather than wrongly rejecting pool-derived ids here.
		if len(c.UpstreamPools) == 0 {
			for _, id := range r.Prefer {
				if _, ok := seenIDs[id]; !ok {
					errs = append(errs, fmt.Errorf("rule[%d] (host_glob=%q): prefer references unknown upstream id %q", i, r.HostGlob, id))
				}
			}
		}
		for j, pat := range r.BodyRegex {
			if pat == "" {
				errs = append(errs, fmt.Errorf("rule[%d] (host_glob=%q): body_regex[%d] is empty", i, r.HostGlob, j))
				continue
			}
			if _, err := regexp.Compile(pat); err != nil {
				errs = append(errs, fmt.Errorf("rule[%d] (host_glob=%q): body_regex[%d] does not compile: %w", i, r.HostGlob, j, err))
			}
		}
	}

	return errors.Join(errs...)
}

// validate joins every [failure.scoring] field violation it finds. The
// scoreboard relies on every field being non-negative (penalties, cooldowns,
// decay step, debounce window) and on probe_chance falling in [0,1]; values
// outside those ranges produce nonsense scoring behavior, so the parser
// rejects them.
func (s ScoringConfig) validate() error {
	var errs []error
	if s.RefusedPenalty < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.refused_penalty must be >= 0, got %v", s.RefusedPenalty))
	}
	if s.RefusedCooldown < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.refused_cooldown must be >= 0, got %v", s.RefusedCooldown.Duration()))
	}
	if s.TimeoutPenalty < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.timeout_penalty must be >= 0, got %v", s.TimeoutPenalty))
	}
	if s.TimeoutCooldown < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.timeout_cooldown must be >= 0, got %v", s.TimeoutCooldown.Duration()))
	}
	if s.SuccessWeight <= 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.success_weight must be > 0, got %v", s.SuccessWeight))
	}
	if s.ScoreCap <= 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.score_cap must be > 0, got %v", s.ScoreCap))
	}
	if s.ProbeChance < 0 || s.ProbeChance > 1 {
		errs = append(errs, fmt.Errorf("failure.scoring.probe_chance must be in [0,1], got %v", s.ProbeChance))
	}
	if s.DecayInterval <= 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.decay_interval must be > 0, got %v", s.DecayInterval.Duration()))
	}
	if s.DecayStep < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.decay_step must be >= 0, got %v", s.DecayStep))
	}
	if s.CascadeTTL < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.cascade_ttl must be >= 0, got %v", s.CascadeTTL.Duration()))
	}
	if s.DebounceWindow < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.debounce_window must be >= 0, got %v", s.DebounceWindow.Duration()))
	}
	if s.BodyMatchPenalty < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.body_match_penalty must be >= 0, got %v", s.BodyMatchPenalty))
	}
	if s.BodyMatchCooldown < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.body_match_cooldown must be >= 0, got %v", s.BodyMatchCooldown.Duration()))
	}
	if s.PruneAfter < 0 {
		errs = append(errs, fmt.Errorf("failure.scoring.prune_after must be >= 0, got %v", s.PruneAfter.Duration()))
	}
	return errors.Join(errs...)
}

func validateAddr(addr, fieldName string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: invalid host:port %q: %w", fieldName, addr, err)
	}
	if err := validatePort(port); err != nil {
		return fmt.Errorf("%s: %w", fieldName, err)
	}
	return nil
}

func validateUpstreamAddr(addr, where string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: addr %q is not host:port: %w", where, addr, err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("%s: addr %q is missing a host", where, addr)
	}
	if err := validatePort(port); err != nil {
		return fmt.Errorf("%s: addr %w", where, err)
	}
	return nil
}

// validatePort parses a port string and confirms it falls in 1-65535.
// Errors carry no section label; callers wrap with their own prefix.
func validatePort(port string) error {
	p, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("port %q is not numeric: %w", port, err)
	}
	if p < 1 || p > 65535 {
		return fmt.Errorf("port %d is outside the 1-65535 range", p)
	}
	return nil
}

// Marshal returns the resolved config as TOML, suitable for --print-config.
func (c *Config) Marshal() ([]byte, error) {
	var sb strings.Builder
	enc := toml.NewEncoder(&sb)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}
