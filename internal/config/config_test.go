package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	if cfg.Failure.TimeoutMS != 8000 {
		t.Errorf("Failure.TimeoutMS default = %d, want 8000", cfg.Failure.TimeoutMS)
	}
	if cfg.Failure.MaxRetriesPerRequest != 5 {
		t.Errorf("Failure.MaxRetriesPerRequest default = %d, want 5", cfg.Failure.MaxRetriesPerRequest)
	}
	if !cfg.Failure.ConnectionRefused {
		t.Error("Failure.ConnectionRefused default should be true")
	}
	if got := len(cfg.Failure.Status); got != len(DefaultStatusRules) {
		t.Errorf("len(Failure.Status) default = %d, want %d", got, len(DefaultStatusRules))
	}
	if cfg.Upstreams[0].Priority != 100 {
		t.Errorf("Upstreams[0].Priority default = %d, want 100", cfg.Upstreams[0].Priority)
	}
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
			contains: "at least one [[upstream]]",
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
