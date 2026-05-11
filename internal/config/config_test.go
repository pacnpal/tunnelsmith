package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain installs a test-only ProviderValidator that mirrors the
// rules the in-tree mullvad and webshare providers enforce in
// production via internal/upstream/providers. The config package can't
// import internal/upstream/providers without a cycle (providers imports
// config to set the hook), so the test recreates the minimum set of
// rules the test cases below assert against. Keeping this here lets
// config tests run as a leaf node of the dependency tree.
func TestMain(m *testing.M) {
	SetProviderValidator(func(cfg UpstreamPoolConfig) error {
		switch cfg.Provider {
		case "mullvad":
			if len(cfg.Countries) == 0 {
				return fmt.Errorf("mullvad: countries must list at least one country")
			}
			for i, c := range cfg.Countries {
				if strings.TrimSpace(c) == "" {
					return fmt.Errorf("mullvad: countries[%d] is empty or whitespace", i)
				}
			}
			return nil
		case "webshare":
			hasInline := strings.TrimSpace(cfg.APIToken) != ""
			hasFile := strings.TrimSpace(cfg.APITokenFile) != ""
			if !hasInline && !hasFile {
				return fmt.Errorf("webshare: one of api_token or api_token_file is required")
			}
			if hasInline && hasFile {
				return fmt.Errorf("webshare: set only one of api_token or api_token_file")
			}
			return nil
		default:
			return fmt.Errorf("provider %q is not supported", cfg.Provider)
		}
	})
	os.Exit(m.Run())
}

const validConfig = `
[listener]
http  = ":8080"
socks = ":1080"

[cache]
ttl          = "20m"
negative_ttl = "30s"

[[upstream]]
id       = "direct"
kind     = "direct"
priority = 10

[[upstream]]
id       = "mullvad-se-got"
kind     = "socks5"
addr     = "se-got-wg-001.relays.mullvad.net:1080"
priority = 20

[[upstream]]
id       = "proton-nl"
kind     = "http"
addr     = "gluetun-proton-nl:8888"
priority = 30

[failure]
timeout_ms              = 5000
max_retries_per_request = 7
body_regex              = ["content.?not.?available"]

[[failure.status]]
code              = 429
penalty           = 4
cooldown          = "120s"
honor_retry_after = true

[[rule]]
host_glob = "*.itch.io"
prefer    = ["direct", "mullvad-se-got"]
`

func TestParseValidConfig(t *testing.T) {
	cfg, err := Parse([]byte(validConfig), "valid.toml")
	if err != nil {
		t.Fatalf("Parse returned unexpected error: %v", err)
	}
	if got, want := cfg.Listener.HTTP, ":8080"; got != want {
		t.Errorf("Listener.HTTP = %q, want %q", got, want)
	}
	if got, want := cfg.Cache.TTL.Duration(), 20*time.Minute; got != want {
		t.Errorf("Cache.TTL = %v, want %v", got, want)
	}
	if got, want := len(cfg.Upstreams), 3; got != want {
		t.Fatalf("len(Upstreams) = %d, want %d", got, want)
	}
	if got, want := cfg.Failure.MaxRetriesPerRequest, 7; got != want {
		t.Errorf("Failure.MaxRetriesPerRequest = %d, want %d", got, want)
	}
	if !cfg.Failure.ConnectionRefused {
		t.Error("Failure.ConnectionRefused should default to true")
	}
	if len(cfg.Failure.Status) != 1 {
		t.Errorf("len(Failure.Status) = %d, want 1 (user provided)", len(cfg.Failure.Status))
	}
}

func TestDefaultsApplied(t *testing.T) {
	const minimal = `
[[upstream]]
id   = "direct"
kind = "direct"
`
	cfg, err := Parse([]byte(minimal), "minimal.toml")
	if err != nil {
		t.Fatalf("Parse returned unexpected error: %v", err)
	}
	if cfg.Listener.HTTP != ":8080" {
		t.Errorf("Listener.HTTP default = %q, want %q", cfg.Listener.HTTP, ":8080")
	}
	if cfg.Listener.SOCKS != ":1080" {
		t.Errorf("Listener.SOCKS default = %q, want %q", cfg.Listener.SOCKS, ":1080")
	}
	if cfg.Cache.TTL.Duration() != 15*time.Minute {
		t.Errorf("Cache.TTL default = %v, want 15m", cfg.Cache.TTL.Duration())
	}
	if cfg.Cache.NegativeTTL.Duration() != time.Minute {
		t.Errorf("Cache.NegativeTTL default = %v, want 1m", cfg.Cache.NegativeTTL.Duration())
	}
	if cfg.Cache.PersistInterval.Duration() != 30*time.Second {
		t.Errorf("Cache.PersistInterval default = %v, want 30s", cfg.Cache.PersistInterval.Duration())
	}
	if cfg.Metrics.Bind != ":9090" {
		t.Errorf("Metrics.Bind default = %q, want :9090", cfg.Metrics.Bind)
	}
	if cfg.UI.Bind != ":9091" {
		t.Errorf("UI.Bind default = %q, want :9091", cfg.UI.Bind)
	}
	if cfg.Control.Bind != ":9092" {
		t.Errorf("Control.Bind default = %q, want :9092", cfg.Control.Bind)
	}
	if cfg.Failure.TimeoutMS != 8000 {
		t.Errorf("Failure.TimeoutMS default = %d, want 8000", cfg.Failure.TimeoutMS)
	}
	if cfg.Failure.MaxRetriesPerRequest != 5 {
		t.Errorf("Failure.MaxRetriesPerRequest default = %d, want 5", cfg.Failure.MaxRetriesPerRequest)
	}
	if cfg.Failure.BodyBufferKB != 32 {
		t.Errorf("Failure.BodyBufferKB default = %d, want 32", cfg.Failure.BodyBufferKB)
	}
	if !cfg.Failure.ConnectionRefused {
		t.Error("Failure.ConnectionRefused default should be true")
	}
	if got := len(cfg.Failure.Status); got != len(DefaultStatusRules) {
		t.Errorf("len(Failure.Status) default = %d, want %d", got, len(DefaultStatusRules))
	}
	if cfg.Upstreams[0].Priority == nil {
		t.Fatal("Upstreams[0].Priority is nil; applyDefaults should populate it")
	}
	if got := *cfg.Upstreams[0].Priority; got != 100 {
		t.Errorf("Upstreams[0].Priority default = %d, want 100", got)
	}
}

func TestUpstreamPoolDefaultsApplied(t *testing.T) {
	t.Parallel()
	const src = `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden"]
`
	cfg, err := Parse([]byte(src), "pool-min.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.UpstreamPools) != 1 {
		t.Fatalf("want 1 upstream_pool, got %d", len(cfg.UpstreamPools))
	}
	p := cfg.UpstreamPools[0]
	if p.Priority == nil || *p.Priority != 200 {
		t.Errorf("Priority default = %v, want 200", p.Priority)
	}
	if p.PriorityValue() != 200 {
		t.Errorf("PriorityValue = %d, want 200", p.PriorityValue())
	}
	if p.Refresh == nil || p.Refresh.Duration() != 12*time.Hour {
		t.Errorf("Refresh default = %v, want 12h", p.Refresh)
	}
	if p.RefreshDuration() != 12*time.Hour {
		t.Errorf("RefreshDuration = %v, want 12h", p.RefreshDuration())
	}
	if p.IncludeInactive {
		t.Error("IncludeInactive default should be false")
	}
}

func TestUpstreamPoolPriorityZeroIsHonored(t *testing.T) {
	t.Parallel()
	const src = `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden"]
priority  = 0
`
	cfg, err := Parse([]byte(src), "pool-zero.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := cfg.UpstreamPools[0]
	if p.Priority == nil || *p.Priority != 0 {
		t.Errorf("Priority = %v, want 0 (user-provided zero must not be overwritten)", p.Priority)
	}
}

func TestUpstreamPoolReplacesNeedForUpstream(t *testing.T) {
	t.Parallel()
	const src = `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden"]
`
	if _, err := Parse([]byte(src), "pool-only.toml"); err != nil {
		t.Fatalf("Parse: %v (a config with only upstream_pool entries should validate)", err)
	}
}

func TestUpstreamPoolRefreshZeroDisablesPolling(t *testing.T) {
	t.Parallel()
	const src = `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden"]
refresh   = "0s"
`
	cfg, err := Parse([]byte(src), "pool-zero-refresh.toml")
	if err != nil {
		t.Fatalf("Parse: %v (refresh = 0s must validate; the expander treats 0 as 'disabled')", err)
	}
	if cfg.UpstreamPools[0].RefreshDuration() != 0 {
		t.Fatalf("RefreshDuration = %v, want 0", cfg.UpstreamPools[0].RefreshDuration())
	}
}

func TestRulePreferIDCheckDeferredWhenUpstreamPoolPresent(t *testing.T) {
	t.Parallel()
	// "mvd-se-sto-wg-001" cannot exist at parse time but will after the
	// Mullvad pool block expands at startup. config.Validate must accept
	// the rule; cmd/tunnelsmith re-checks against the merged upstream set
	// after expansion.
	const src = `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden"]

[[rule]]
host_glob = "*.example.com"
prefer    = ["mvd-se-sto-wg-001"]
`
	if _, err := Parse([]byte(src), "pool-rule.toml"); err != nil {
		t.Fatalf("Parse: %v (rule preferring a pool-derived id should not fail at parse time)", err)
	}
}

// TestExplicitZeroAndFalseSurviveDefaults locks in the bug fix from PR #6
// review feedback: applyDefaults must not silently overwrite user-provided
// false / 0 values. The earlier "value == zero" defaulting hid an explicit
// connection_refused = false, an upstream priority = 0, and explicit zeros
// for the failure timeout/retry caps that should reach Validate.
func TestExplicitZeroAndFalseSurviveDefaults(t *testing.T) {
	t.Parallel()

	t.Run("connection_refused = false is honored", func(t *testing.T) {
		t.Parallel()
		const src = `
[failure]
connection_refused = false

[[upstream]]
id   = "d"
kind = "direct"
`
		cfg, err := Parse([]byte(src), "cr.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Failure.ConnectionRefused {
			t.Error("ConnectionRefused was overwritten to true; user said false")
		}
	})

	t.Run("priority = 0 is honored", func(t *testing.T) {
		t.Parallel()
		const src = `
[[upstream]]
id       = "d"
kind     = "direct"
priority = 0
`
		cfg, err := Parse([]byte(src), "p0.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Upstreams[0].Priority == nil {
			t.Fatal("Priority is nil; user wrote 0")
		}
		if got := *cfg.Upstreams[0].Priority; got != 0 {
			t.Errorf("Priority = %d, want 0", got)
		}
	})

	t.Run("timeout_ms = 0 fails validation instead of being silently defaulted", func(t *testing.T) {
		t.Parallel()
		const src = `
[failure]
timeout_ms = 0

[[upstream]]
id   = "d"
kind = "direct"
`
		_, err := Parse([]byte(src), "t0.toml")
		if err == nil {
			t.Fatal("expected validation error for timeout_ms = 0, got nil")
		}
		if !strings.Contains(err.Error(), "timeout_ms must be > 0") {
			t.Errorf("error = %q, want one mentioning timeout_ms must be > 0", err.Error())
		}
	})

	t.Run("max_retries_per_request = 0 fails validation", func(t *testing.T) {
		t.Parallel()
		const src = `
[failure]
max_retries_per_request = 0

[[upstream]]
id   = "d"
kind = "direct"
`
		_, err := Parse([]byte(src), "r0.toml")
		if err == nil {
			t.Fatal("expected validation error for max_retries_per_request = 0, got nil")
		}
		if !strings.Contains(err.Error(), "max_retries_per_request must be >= 1") {
			t.Errorf("error = %q, want one mentioning max_retries_per_request", err.Error())
		}
	})
}

func TestValidationFailures(t *testing.T) {
	cases := []struct {
		name     string
		toml     string
		contains string
	}{
		{
			name: "no upstreams",
			toml: `
[listener]
http = ":8080"
`,
			contains: "at least one [[upstream]] or [[upstream_pool]]",
		},
		{
			name: "missing upstream id",
			toml: `
[[upstream]]
kind = "direct"
`,
			contains: "id is required",
		},
		{
			name: "duplicate upstream ids",
			toml: `
[[upstream]]
id   = "x"
kind = "direct"
[[upstream]]
id   = "x"
kind = "direct"
`,
			contains: "duplicate upstream id",
		},
		{
			name: "unknown upstream kind",
			toml: `
[[upstream]]
id   = "weird"
kind = "wireguard"
addr = "x:1"
`,
			contains: "is not one of direct, http, socks5",
		},
		{
			name: "direct must not have addr",
			toml: `
[[upstream]]
id   = "d"
kind = "direct"
addr = "host:1234"
`,
			contains: "must not set addr",
		},
		{
			name: "socks5 missing addr",
			toml: `
[[upstream]]
id   = "s"
kind = "socks5"
`,
			contains: "requires addr",
		},
		{
			name: "http addr missing port",
			toml: `
[[upstream]]
id   = "h"
kind = "http"
addr = "no-port-here"
`,
			contains: "is not host:port",
		},
		{
			name: "addr port out of range",
			toml: `
[[upstream]]
id   = "h"
kind = "http"
addr = "host:70000"
`,
			contains: "outside the 1-65535 range",
		},
		{
			name: "addr port not numeric",
			toml: `
[[upstream]]
id   = "h"
kind = "http"
addr = "host:abc"
`,
			contains: "not numeric",
		},
		{
			name: "listener port out of range",
			toml: `
[listener]
http = ":99999"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "outside the 1-65535 range",
		},
		{
			name: "listener missing port",
			toml: `
[listener]
http = "no-port"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "invalid host:port",
		},
		{
			name: "duration parse failure",
			toml: `
[cache]
ttl = "fifteen-minutes"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "parse duration",
		},
		{
			name: "status code below 100",
			toml: `
[[failure.status]]
code     = 99
penalty  = 1
cooldown = "1s"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "outside the 100-599 range",
		},
		{
			name: "status code above 599",
			toml: `
[[failure.status]]
code     = 999
penalty  = 1
cooldown = "1s"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "outside the 100-599 range",
		},
		{
			name: "rule prefer references unknown id",
			toml: `
[[upstream]]
id = "d"
kind = "direct"
[[rule]]
host_glob = "*.example"
prefer    = ["mystery"]
`,
			contains: `unknown upstream id "mystery"`,
		},
		{
			name: "rule body_regex does not compile",
			toml: `
[[upstream]]
id = "d"
kind = "direct"
[[rule]]
host_glob = "*.example"
prefer    = ["d"]
body_regex = ["[unterminated"]
`,
			contains: "body_regex[0] does not compile",
		},
		{
			name: "rule body_regex empty entry",
			toml: `
[[upstream]]
id = "d"
kind = "direct"
[[rule]]
host_glob = "*.example"
prefer    = ["d"]
body_regex = [""]
`,
			contains: "body_regex[0] is empty",
		},
		{
			name: "rule host_glob malformed",
			toml: `
[[upstream]]
id = "d"
kind = "direct"
[[rule]]
host_glob = "[bad"
prefer    = ["d"]
`,
			contains: "invalid glob",
		},
		{
			name: "body_buffer_kb negative",
			toml: `
[failure]
body_buffer_kb = -1

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "body_buffer_kb must be >= 0",
		},
		{
			name: "body_buffer_kb above cap",
			toml: `
[failure]
body_buffer_kb = 4096

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "body_buffer_kb must be <= 1024",
		},
		{
			name: "rule missing host_glob",
			toml: `
[[upstream]]
id = "d"
kind = "direct"
[[rule]]
prefer = ["d"]
`,
			contains: "host_glob is required",
		},
		{
			name: "rule with empty prefer",
			toml: `
[[upstream]]
id = "d"
kind = "direct"
[[rule]]
host_glob = "*.example"
prefer    = []
`,
			contains: "prefer must list at least one upstream id",
		},
		{
			name: "cache persist_path must be absolute",
			toml: `
[cache]
persist_path = "relative/path.db"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "must be an absolute path",
		},
		{
			name: "cache persist_interval rejects negative",
			toml: `
[cache]
persist_interval = "-1s"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "cache.persist_interval must be >= 0",
		},
		{
			name: "metrics.bind rejects malformed addr",
			toml: `
[metrics]
bind = "not-a-host-port"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "metrics.bind",
		},
		{
			name: "metrics.bind rejects out-of-range port",
			toml: `
[metrics]
bind = ":99999"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "outside the 1-65535 range",
		},
		{
			name: "ui.bind rejects malformed addr",
			toml: `
[ui]
bind = "not-a-host-port"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "ui.bind",
		},
		{
			name: "control.bind rejects malformed addr",
			toml: `
[control]
bind = "not-a-host-port"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "control.bind",
		},
		{
			name: "upstream_pool missing provider",
			toml: `
[[upstream_pool]]
id_prefix = "mvd"
countries = ["Sweden"]
`,
			contains: "provider is required",
		},
		{
			name: "upstream_pool unsupported provider",
			toml: `
[[upstream_pool]]
provider  = "nordvpn"
id_prefix = "nrd"
countries = ["Sweden"]
`,
			contains: "is not supported",
		},
		{
			name: "upstream_pool missing id_prefix",
			toml: `
[[upstream_pool]]
provider  = "mullvad"
countries = ["Sweden"]
`,
			contains: "id_prefix is required",
		},
		{
			name: "upstream_pool empty countries",
			toml: `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = []
`,
			contains: "countries must list at least one country",
		},
		{
			name: "upstream_pool refresh below 1m floor",
			toml: `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden"]
refresh   = "30s"
`,
			contains: "refresh must be 0 (to disable) or >= 1m",
		},
		{
			name: "upstream_pool negative refresh rejected",
			toml: `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden"]
refresh   = "-1s"
`,
			contains: "refresh must be >= 0",
		},
		{
			name: "upstream_pool empty country entry",
			toml: `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden", ""]
`,
			contains: "countries[1] is empty or whitespace",
		},
		{
			name: "upstream_pool whitespace-only country entry",
			toml: `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["   "]
`,
			contains: "countries[0] is empty or whitespace",
		},
		{
			name: "upstream_pool relative cache_path",
			toml: `
[[upstream_pool]]
provider   = "mullvad"
id_prefix  = "mvd"
countries  = ["Sweden"]
cache_path = "relative/path.json"
`,
			contains: "cache_path",
		},
		{
			name: "upstream_pool duplicate id_prefix",
			toml: `
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden"]
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["USA"]
`,
			contains: "duplicate id_prefix",
		},
		{
			name: "control auth_tokens rejects empty token",
			toml: `
[control]
auth_tokens = ["good", ""]
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "empty token",
		},
		{
			name: "control auth_tokens rejects leading whitespace",
			toml: `
[control]
auth_tokens = [" alpha"]
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "contains whitespace",
		},
		{
			name: "control auth_tokens rejects trailing whitespace",
			toml: `
[control]
auth_tokens = ["alpha\t"]
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "contains whitespace",
		},
		{
			name: "control auth_tokens rejects embedded whitespace",
			toml: `
[control]
auth_tokens = ["alpha beta"]
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "contains whitespace",
		},
		{
			name: "control auth_tokens_file rejects relative path",
			toml: `
[control]
auth_tokens_file = "tokens.txt"
[[upstream]]
id = "d"
kind = "direct"
`,
			contains: "must be an absolute path",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.toml), tc.name+".toml")
			if err == nil {
				t.Fatalf("Parse succeeded; want error containing %q", tc.contains)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.contains)
			}
		})
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err == nil {
		t.Fatal("Load on missing file returned nil error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tunnelsmith.toml")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Upstreams[1].Addr; got != "se-got-wg-001.relays.mullvad.net:1080" {
		t.Errorf("Load lost upstream addr: %q", got)
	}
}

func TestUnknownKeysSurface(t *testing.T) {
	const withTypo = `
[listener]
http = ":8080"
[[upstream]]
id = "d"
kind = "direct"
[failure]
timout_ms = 9000
`
	cfg, err := Parse([]byte(withTypo), "typo.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.UnknownKeys) == 0 {
		t.Fatal("UnknownKeys was empty; expected to catch the timout_ms typo")
	}
	found := false
	for _, k := range cfg.UnknownKeys {
		if strings.Contains(k, "timout_ms") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("UnknownKeys = %v; expected one containing 'timout_ms'", cfg.UnknownKeys)
	}
}

func TestScoringDefaultsApplied(t *testing.T) {
	t.Parallel()
	const minimal = `
[[upstream]]
id   = "direct"
kind = "direct"
`
	cfg, err := Parse([]byte(minimal), "scoring-defaults.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := cfg.Failure.Scoring
	if s.RefusedPenalty != ScoringDefaults.RefusedPenalty {
		t.Errorf("RefusedPenalty default = %v, want %v", s.RefusedPenalty, ScoringDefaults.RefusedPenalty)
	}
	if s.RefusedCooldown.Duration() != ScoringDefaults.RefusedCooldown.Duration() {
		t.Errorf("RefusedCooldown default = %v, want %v", s.RefusedCooldown.Duration(), ScoringDefaults.RefusedCooldown.Duration())
	}
	if s.TimeoutPenalty != ScoringDefaults.TimeoutPenalty {
		t.Errorf("TimeoutPenalty default = %v, want %v", s.TimeoutPenalty, ScoringDefaults.TimeoutPenalty)
	}
	if s.TimeoutCooldown.Duration() != ScoringDefaults.TimeoutCooldown.Duration() {
		t.Errorf("TimeoutCooldown default = %v, want %v", s.TimeoutCooldown.Duration(), ScoringDefaults.TimeoutCooldown.Duration())
	}
	if s.SuccessWeight != ScoringDefaults.SuccessWeight {
		t.Errorf("SuccessWeight default = %v, want %v", s.SuccessWeight, ScoringDefaults.SuccessWeight)
	}
	if s.ScoreCap != ScoringDefaults.ScoreCap {
		t.Errorf("ScoreCap default = %v, want %v", s.ScoreCap, ScoringDefaults.ScoreCap)
	}
	if s.ProbeChance != ScoringDefaults.ProbeChance {
		t.Errorf("ProbeChance default = %v, want %v", s.ProbeChance, ScoringDefaults.ProbeChance)
	}
	if s.DecayInterval.Duration() != ScoringDefaults.DecayInterval.Duration() {
		t.Errorf("DecayInterval default = %v, want %v", s.DecayInterval.Duration(), ScoringDefaults.DecayInterval.Duration())
	}
	if s.DecayStep != ScoringDefaults.DecayStep {
		t.Errorf("DecayStep default = %v, want %v", s.DecayStep, ScoringDefaults.DecayStep)
	}
	if s.CascadeTTL.Duration() != ScoringDefaults.CascadeTTL.Duration() {
		t.Errorf("CascadeTTL default = %v, want %v", s.CascadeTTL.Duration(), ScoringDefaults.CascadeTTL.Duration())
	}
	if s.DebounceWindow.Duration() != ScoringDefaults.DebounceWindow.Duration() {
		t.Errorf("DebounceWindow default = %v, want %v", s.DebounceWindow.Duration(), ScoringDefaults.DebounceWindow.Duration())
	}
	if s.PruneAfter.Duration() != ScoringDefaults.PruneAfter.Duration() {
		t.Errorf("PruneAfter default = %v, want %v", s.PruneAfter.Duration(), ScoringDefaults.PruneAfter.Duration())
	}
	if s.BodyMatchPenalty != ScoringDefaults.BodyMatchPenalty {
		t.Errorf("BodyMatchPenalty default = %v, want %v", s.BodyMatchPenalty, ScoringDefaults.BodyMatchPenalty)
	}
	if s.BodyMatchCooldown.Duration() != ScoringDefaults.BodyMatchCooldown.Duration() {
		t.Errorf("BodyMatchCooldown default = %v, want %v", s.BodyMatchCooldown.Duration(), ScoringDefaults.BodyMatchCooldown.Duration())
	}
}

func TestRuleBodyRegexAndForceParse(t *testing.T) {
	t.Parallel()
	const src = `
[[upstream]]
id   = "direct"
kind = "direct"

[[upstream]]
id   = "fallback"
kind = "socks5"
addr = "127.0.0.1:1080"

[[rule]]
host_glob  = "*.bbc.co.uk"
prefer     = ["fallback"]
force      = true

[[rule]]
host_glob  = "*.itch.io"
prefer     = ["direct"]
body_regex = ["content.?not.?available", "region.?lock"]
`
	cfg, err := Parse([]byte(src), "rule-body.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(cfg.Rules); got != 2 {
		t.Fatalf("len(Rules) = %d, want 2", got)
	}
	if !cfg.Rules[0].Force {
		t.Error("Rules[0].Force = false, want true")
	}
	if got := len(cfg.Rules[0].BodyRegex); got != 0 {
		t.Errorf("Rules[0].BodyRegex len = %d, want 0", got)
	}
	if got := len(cfg.Rules[1].BodyRegex); got != 2 {
		t.Fatalf("Rules[1].BodyRegex len = %d, want 2", got)
	}
	if cfg.Rules[1].Force {
		t.Error("Rules[1].Force = true, want false (default)")
	}
}

func TestScoringPartialOverridePreservesOtherDefaults(t *testing.T) {
	t.Parallel()
	const partial = `
[failure.scoring]
probe_chance = 0.25

[[upstream]]
id   = "direct"
kind = "direct"
`
	cfg, err := Parse([]byte(partial), "scoring-partial.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := cfg.Failure.Scoring
	if s.ProbeChance != 0.25 {
		t.Errorf("ProbeChance = %v, want 0.25 from user override", s.ProbeChance)
	}
	if s.RefusedPenalty != ScoringDefaults.RefusedPenalty {
		t.Errorf("RefusedPenalty = %v, want %v from defaults", s.RefusedPenalty, ScoringDefaults.RefusedPenalty)
	}
	if s.DecayInterval.Duration() != ScoringDefaults.DecayInterval.Duration() {
		t.Errorf("DecayInterval = %v, want %v from defaults", s.DecayInterval.Duration(), ScoringDefaults.DecayInterval.Duration())
	}
}

func TestScoringValidationFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		toml     string
		contains string
	}{
		{
			name: "negative refused_penalty",
			toml: `
[failure.scoring]
refused_penalty = -1

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "refused_penalty must be >= 0",
		},
		{
			name: "negative refused_cooldown",
			toml: `
[failure.scoring]
refused_cooldown = "-5s"

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "refused_cooldown must be >= 0",
		},
		{
			name: "zero success_weight",
			toml: `
[failure.scoring]
success_weight = 0

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "success_weight must be > 0",
		},
		{
			name: "zero score_cap",
			toml: `
[failure.scoring]
score_cap = 0

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "score_cap must be > 0",
		},
		{
			name: "probe_chance too high",
			toml: `
[failure.scoring]
probe_chance = 1.5

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "probe_chance must be in [0,1]",
		},
		{
			name: "negative probe_chance",
			toml: `
[failure.scoring]
probe_chance = -0.1

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "probe_chance must be in [0,1]",
		},
		{
			name: "zero decay_interval",
			toml: `
[failure.scoring]
decay_interval = "0s"

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "decay_interval must be > 0",
		},
		{
			name: "negative debounce_window",
			toml: `
[failure.scoring]
debounce_window = "-10ms"

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "debounce_window must be >= 0",
		},
		{
			name: "negative prune_after",
			toml: `
[failure.scoring]
prune_after = "-1m"

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "prune_after must be >= 0",
		},
		{
			name: "negative body_match_penalty",
			toml: `
[failure.scoring]
body_match_penalty = -2

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "body_match_penalty must be >= 0",
		},
		{
			name: "negative body_match_cooldown",
			toml: `
[failure.scoring]
body_match_cooldown = "-30s"

[[upstream]]
id   = "d"
kind = "direct"
`,
			contains: "body_match_cooldown must be >= 0",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.toml), tc.name+".toml")
			if err == nil {
				t.Fatalf("Parse succeeded; want error containing %q", tc.contains)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.contains)
			}
		})
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	cfg, err := Parse([]byte(validConfig), "rt.toml")
	if err != nil {
		t.Fatal(err)
	}
	out, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), `id = "direct"`) {
		t.Errorf("Marshal output missing direct upstream entry:\n%s", out)
	}
	cfg2, err := Parse(out, "round-trip.toml")
	if err != nil {
		t.Fatalf("re-parse marshaled output failed: %v", err)
	}
	if got, want := cfg2.Cache.TTL.Duration(), 20*time.Minute; got != want {
		t.Errorf("round-tripped Cache.TTL = %v, want %v", got, want)
	}
}
