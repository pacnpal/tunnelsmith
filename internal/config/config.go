// Package config loads, validates, and applies defaults to Tunnelsmith's
// TOML configuration. The schema mirrors the one described in
// tunnelsmith-proposal.md.
package config

import (
	"encoding"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	Listener  ListenerConfig   `toml:"listener"`
	Cache     CacheConfig      `toml:"cache"`
	Upstreams []UpstreamConfig `toml:"upstream"`
	Failure   FailureConfig    `toml:"failure"`
	Rules     []RuleConfig     `toml:"rule"`

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
type CacheConfig struct {
	TTL         Duration `toml:"ttl"`          // default: 15m
	NegativeTTL Duration `toml:"negative_ttl"` // default: 1m
	PersistPath string   `toml:"persist_path"` // default: "" (in-memory only)
}

// UpstreamConfig declares one egress option that the router can pick.
// Addr is required for kinds http and socks5 and ignored for kind direct.
type UpstreamConfig struct {
	ID       string       `toml:"id"`
	Kind     UpstreamKind `toml:"kind"`
	Addr     string       `toml:"addr"`
	Priority int          `toml:"priority"` // default: 100
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

// FailureConfig collects the user's failure-detection preferences.
type FailureConfig struct {
	ConnectionRefused    bool         `toml:"connection_refused"`      // default: true (always on for Phase 1; opt-out lands in Phase 5)
	TimeoutMS            int          `toml:"timeout_ms"`              // default: 8000
	BodyRegex            []string     `toml:"body_regex"`              // default: nil
	MaxRetriesPerRequest int          `toml:"max_retries_per_request"` // default: 5
	Status               []StatusRule `toml:"status"`                  // default: see DefaultStatusRules
}

// RuleConfig declares a per-host routing override.
type RuleConfig struct {
	HostGlob string   `toml:"host_glob"`
	Prefer   []string `toml:"prefer"`
	Force    bool     `toml:"force"` // default: false
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
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", sourceLabel, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listener.HTTP == "" {
		c.Listener.HTTP = ":8080"
	}
	if c.Listener.SOCKS == "" {
		c.Listener.SOCKS = ":1080"
	}
	if c.Cache.TTL == 0 {
		c.Cache.TTL = Duration(15 * time.Minute)
	}
	if c.Cache.NegativeTTL == 0 {
		c.Cache.NegativeTTL = Duration(time.Minute)
	}
	if c.Failure.TimeoutMS == 0 {
		c.Failure.TimeoutMS = 8000
	}
	if c.Failure.MaxRetriesPerRequest == 0 {
		c.Failure.MaxRetriesPerRequest = 5
	}
	// Connection-refused detection is always on for Phase 1. Phase 5
	// will introduce an opt-out path if a real use case shows up.
	c.Failure.ConnectionRefused = true
	if len(c.Failure.Status) == 0 {
		c.Failure.Status = append([]StatusRule(nil), DefaultStatusRules...)
	}
	for i := range c.Upstreams {
		if c.Upstreams[i].Priority == 0 {
			c.Upstreams[i].Priority = 100
		}
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

	if len(c.Upstreams) == 0 {
		errs = append(errs, errors.New("at least one [[upstream]] must be defined"))
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
	for i, s := range c.Failure.Status {
		if s.Code < 100 || s.Code > 599 {
			errs = append(errs, fmt.Errorf("failure.status[%d]: code %d is outside the 100-599 range", i, s.Code))
		}
		if s.Cooldown < 0 {
			errs = append(errs, fmt.Errorf("failure.status[%d] (code=%d): cooldown must be >= 0", i, s.Code))
		}
	}

	for i, r := range c.Rules {
		if r.HostGlob == "" {
			errs = append(errs, fmt.Errorf("rule[%d]: host_glob is required", i))
		}
		if len(r.Prefer) == 0 {
			errs = append(errs, fmt.Errorf("rule[%d] (host_glob=%q): prefer must list at least one upstream id", i, r.HostGlob))
		}
		for _, id := range r.Prefer {
			if _, ok := seenIDs[id]; !ok {
				errs = append(errs, fmt.Errorf("rule[%d] (host_glob=%q): prefer references unknown upstream id %q", i, r.HostGlob, id))
			}
		}
	}

	return errors.Join(errs...)
}

func validateAddr(addr, fieldName string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: invalid host:port %q: %w", fieldName, addr, err)
	}
	_ = host
	p, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("%s: port %q is not numeric: %w", fieldName, port, err)
	}
	if p < 1 || p > 65535 {
		return fmt.Errorf("%s: port %d is outside the 1-65535 range", fieldName, p)
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
	p, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("%s: addr port %q is not numeric: %w", where, port, err)
	}
	if p < 1 || p > 65535 {
		return fmt.Errorf("%s: addr port %d is outside the 1-65535 range", where, p)
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
