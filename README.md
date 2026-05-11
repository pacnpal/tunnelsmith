# Tunnelsmith

[![CI](https://github.com/pacnpal/tunnelsmith/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/pacnpal/tunnelsmith/actions/workflows/ci.yml?query=branch%3Amain)
[![Latest release](https://img.shields.io/github/v/release/pacnpal/tunnelsmith?sort=semver&display_name=tag)](https://github.com/pacnpal/tunnelsmith/releases/latest)
[![Container image](https://img.shields.io/badge/ghcr.io-pacnpal%2Ftunnelsmith-blue?logo=docker)](https://github.com/pacnpal/tunnelsmith/pkgs/container/tunnelsmith)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A per-destination egress router for HTTP and SOCKS5. Picks the right exit for each URL based on what is actually working.

> **Status: v1.1.0.** v1.0.0 shipped the per-host scoreboard, Mullvad pool integration, web UI, metrics, and Phase 11 cooperative outcome reporting. v1.1.0 adds Phase 11.1 (hot-swap of the running `[[upstream_pool]]` on Mullvad relay churn) and Phase 12 (opt-in bearer-token auth on the cooperative-reporting endpoint). See [`CHANGELOG.md`](CHANGELOG.md) for the full release notes.

## What it does

Tunnelsmith sits in front of apps as an HTTP and SOCKS5 proxy. For each request, it looks up the destination host in a per-(host, upstream) scoreboard, picks the highest-scored upstream that is not in cooldown, tries the request, and updates the scoreboard based on the outcome. When an upstream starts failing for a host, Tunnelsmith demotes it for that host only and tries the next-best candidate. When the upstream recovers, Tunnelsmith probes it occasionally and lets it climb back up. The result is a proxy that learns which exit works for which destination and adapts as conditions change.

This is the gap between HAProxy (no per-host memory), Squid (static rules, no learning), and `scrapy-rotating-proxies` (Scrapy-only, alive/dead per proxy globally).

## How it works

Each request goes through the scoreboard's `DialFor`. The scoreboard picks the best non-cooled upstream for the host, dials, and either records success and returns the conn or records the failure (penalty + cooldown for that pair) and advances. Retries are capped per request; if every retry fails, the host enters cascade cooling and subsequent requests fail fast for a short TTL instead of stampeding the pool. A small probe chance occasionally picks a non-top candidate so previously penalized upstreams get a chance to recover. Time decay drifts old scores toward zero so the scoreboard responds to current conditions rather than yesterday's. Optional `[[rule]]` blocks pin specific hosts to specific upstreams (`prefer` / `force`) and run response-body regex inspection for soft geo-blocks served as 200s. See [`docs/architecture.md`](docs/architecture.md) for the full design.

## Installation

### Container image

Multi-arch images (`linux/amd64`, `linux/arm64`) are published to GitHub Container Registry on every release:

```sh
docker pull ghcr.io/pacnpal/tunnelsmith:1.1.0
docker run --rm ghcr.io/pacnpal/tunnelsmith:1.1.0 --version
```

Image tags omit the leading `v` that git tags carry (so git tag `v1.1.0` → image tag `1.1.0`). `:latest` is **not** published by the release workflow; always pin to a semver tag. Replace `1.1.0` with the desired version from the [releases page](https://github.com/pacnpal/tunnelsmith/releases).

The image is built from [`Dockerfile`](Dockerfile) using a distroless base (`gcr.io/distroless/static-debian12:nonroot`) and runs as a non-root user. The Dockerfile `EXPOSE`s ports `8080` (HTTP CONNECT), `1080` (SOCKS5), `9090` (metrics), and `9091` (web UI). The control endpoint on `:9092` is enabled by default but not in the `EXPOSE` list; publish it explicitly when you need it (`-p 9092:9092`).

### Build from source

Requires Go 1.23 or later.

```sh
git clone https://github.com/pacnpal/tunnelsmith.git
cd tunnelsmith
make build          # produces bin/tunnelsmith
./bin/tunnelsmith --version
```

Other useful Make targets:

| Target | Description |
|--------|-------------|
| `make build` | Compile the binary into `bin/tunnelsmith` |
| `make test` | Run all unit tests |
| `make lint` | Run `golangci-lint` over the whole module |
| `make tidy` | Run `go mod tidy` |
| `make clean` | Remove `bin/` and clear the test cache |

## Quick start

### Running the binary

The binary starts five listeners by default: HTTP CONNECT and forward proxy on `:8080`, SOCKS5 on `:1080`, Prometheus metrics on `:9090`, web UI on `:9091`, and the cooperative-reporting control endpoint on `:9092`. The optional listeners (`[metrics].bind`, `[ui].bind`, `[control].bind`) can be disabled by setting them to `""` in the config; `[listener].http` and `[listener].socks` require valid host:port values and cannot be disabled. You need a TOML config file with at least one `[[upstream]]` or `[[upstream_pool]]` block.

The minimal config below sends everything out the host's default route (`direct`). Copy it into a file, then start the binary:

```toml
# minimal.toml
[listener]
http  = ":8080"
socks = ":1080"

[[upstream]]
id       = "direct"
kind     = "direct"
priority = 10
```

```sh
./bin/tunnelsmith --config minimal.toml
```

In another shell, verify both listeners:

```sh
curl --proxy http://localhost:8080 https://example.com
curl --socks5-hostname localhost:1080 https://example.com
```

The example config at [`deploy/tunnelsmith.example.toml`](deploy/tunnelsmith.example.toml) adds an intentionally unreachable SOCKS5 entry first so every request exercises the retry/fallback path — useful for confirming the pool is wired up correctly:

```sh
./bin/tunnelsmith --config deploy/tunnelsmith.example.toml
```

Tunnelsmith logs one `upstream dial` line per attempt; you will see `outcome=failure` for the unreachable entry and `outcome=success` for `direct`.

### Running with Docker

```sh
docker run -d \
  --name tunnelsmith \
  -p 8080:8080 \
  -p 1080:1080 \
  -v "$(pwd)/deploy/tunnelsmith.example.toml:/etc/tunnelsmith/config.toml:ro" \
  ghcr.io/pacnpal/tunnelsmith:1.1.0 \
  --config /etc/tunnelsmith/config.toml
```

### Running with Docker Compose

Save the two files below alongside each other, then run `docker compose up -d`.

**`config.toml`** — a two-upstream pool: one SOCKS5 proxy, one direct fallback:

```toml
[listener]
http  = ":8080"
socks = ":1080"

[cache]
persist_path = "/data/tunnelsmith/scoreboard.db"

[metrics]
bind = ":9090"

[ui]
bind = ":9091"

[control]
bind = ":9092"

[failure]
timeout_ms              = 8000
max_retries_per_request = 5

[[failure.status]]
code              = 429
penalty           = 4
cooldown          = "120s"
honor_retry_after = true

[[failure.status]]
code     = 403
penalty  = 6
cooldown = "30m"

[[failure.status]]
code     = 451
penalty  = 8
cooldown = "6h"

[[upstream]]
id       = "my-socks5"
kind     = "socks5"
addr     = "proxy.example.com:1080"
priority = 10

[[upstream]]
id       = "direct"
kind     = "direct"
priority = 20
```

**`docker-compose.yml`**:

```yaml
services:
  tunnelsmith:
    image: ghcr.io/pacnpal/tunnelsmith:1.1.0
    container_name: tunnelsmith
    restart: unless-stopped
    ports:
      - "8080:8080"   # HTTP CONNECT and forward proxy
      - "1080:1080"   # SOCKS5
      - "9090:9090"   # Prometheus metrics
      - "9091:9091"   # Web UI (bind to a private interface in production)
      # WARNING: the control endpoint accepts unauthenticated POST /v1/report by
      # default (empty auth_tokens set = permit all).  Do NOT publish 9092 to
      # untrusted networks without setting [control].auth_tokens or
      # auth_tokens_file in your config.  Remove the line below if you don't
      # need external cooperative reporting.
      - "9092:9092"   # Cooperative-reporting control endpoint
    volumes:
      - ./config.toml:/etc/tunnelsmith/config.toml:ro
      - tunnelsmith-data:/data/tunnelsmith
    command:
      - --config=/etc/tunnelsmith/config.toml
    environment:
      TUNNELSMITH_LOG_LEVEL: info

volumes:
  # The image runs as a non-root user (UID/GID 65532).  Docker creates named
  # volumes owned by root, so the first run will fail to write the scoreboard
  # snapshot unless you pre-create the volume directory and chown it:
  #   docker run --rm -v tunnelsmith-data:/data/tunnelsmith alpine \
  #     chown -R 65532:65532 /data/tunnelsmith
  # Alternatively use a host bind-mount and chown the host directory.
  tunnelsmith-data:
```

Then:

```sh
docker compose up -d
curl --proxy http://localhost:8080 https://example.com
curl --socks5-hostname localhost:1080 https://example.com
```

Open `http://localhost:9091` in a browser to see the live scoreboard. Metrics are at `http://localhost:9090/metrics`.

### CLI reference

```
tunnelsmith [flags]

Flags:
  --config <path>     Path to the TOML config file (default: /etc/tunnelsmith/config.toml)
  --print-config      Load the config, apply defaults, print the resolved TOML to stdout, and exit
  --version           Print version, commit, and build date, then exit

Environment:
  TUNNELSMITH_LOG_LEVEL   Log verbosity: debug | info | warn | error (default: info)
```

Use `--print-config` to confirm what the binary actually sees after defaults are applied:

```sh
./bin/tunnelsmith --config my-config.toml --print-config
```

Logs are JSON, one event per line. Set `TUNNELSMITH_LOG_LEVEL=debug` for verbose dial traces.

## Configuration

Tunnelsmith reads a TOML config from `--config` (default `/etc/tunnelsmith/config.toml`). A complete commented example is at [`examples/tunnelsmith.toml`](examples/tunnelsmith.toml). Every key is documented in [`docs/configuration.md`](docs/configuration.md).

### Config overview

```toml
[listener]
http  = ":8080"   # HTTP CONNECT and forward proxy
socks = ":1080"   # SOCKS5 listener

[cache]
persist_path     = ""      # absolute path for scoreboard persistence (empty = in-memory only)
persist_interval = "30s"   # how often to snapshot to persist_path

[metrics]
bind = ":9090"   # Prometheus /metrics and /healthz (empty disables)

[ui]
bind = ":9091"   # web UI (empty disables)

[control]
bind = ":9092"   # cooperative-reporting endpoint (empty disables)

# At least one [[upstream]] or [[upstream_pool]] is required.
# Lower priority value wins (tried first). Unique id required.

[[upstream]]
id       = "direct"
kind     = "direct"   # send traffic out the host's default route; no addr
priority = 10

[[upstream]]
id       = "my-socks5"
kind     = "socks5"
addr     = "proxy.example.com:1080"
priority = 20

[[upstream]]
id       = "my-http-proxy"
kind     = "http"
addr     = "proxy.example.com:8888"   # HTTP CONNECT proxy
priority = 30

[failure]
timeout_ms              = 8000   # per-attempt connect/read timeout in ms
max_retries_per_request = 5      # cap on per-request retries through the pool

# Optional per-host routing rules.
[[rule]]
host_glob = "*.bbc.co.uk"
prefer    = ["my-socks5"]
force     = true   # do not fall back to other upstreams on failure
```

### Key sections

**`[failure.scoring]`** — Per-(host, upstream) scoring knobs for ECONNREFUSED, timeout, and body-regex matches; score cap; probe chance; time decay (`decay_step`, `decay_interval`); cascade TTL. Per-status-code rules (HTTP 429/403/451 penalties and cooldowns) come from `[[failure.status]]`. All have sane defaults; omit the section to use them.

**`[[rule]]`** — Optional per-host routing overrides. `prefer` lists upstreams to try first (in order). `force = true` prevents fallback to other upstreams. `body_regex` fires body-match detection on plain-HTTP responses so geo-block soft-failures served as `200 OK` still penalize the right upstream.

**`[control]` auth (Phase 12)** — Add `auth_tokens = ["your-token"]` or point `auth_tokens_file` at a one-token-per-line file (must be an **absolute path**) to gate `POST /v1/report` with bearer-token auth. Hot-reloads on SIGHUP without a restart.

Sending `SIGHUP` applies a subset of config changes in place without a restart:

**Hot-reloadable on SIGHUP:**
- All `[failure.scoring]` configuration options — **except `decay_interval`**, which requires restart (see below)
- `[[failure.status]]` codes and per-code penalty/cooldown
- `failure.connection_refused`, `failure.body_buffer_kb`
- `failure.max_retries_per_request` — hot-reloads for the HTTP forward-proxy path; when `[[upstream_pool]]` is in play the pool is not rebuilt on SIGHUP, so CONNECT/SOCKS retry behavior requires a restart
- `[[rule]]` blocks (host routing overrides and body-regex patterns)
- `[control].auth_tokens` and `[control].auth_tokens_file` (bearer-token rotation)
- Static `[[upstream]]` list — only when no `[[upstream_pool]]` blocks are in play in either the running or new config

**Require a restart to take effect:**
- Listener bind addresses: `[listener].http`, `[listener].socks`, `[metrics].bind`, `[ui].bind`, `[control].bind`
- `[control].gate_healthz`
- `cache.persist_path` and `cache.persist_interval`
- `[[upstream_pool]]` blocks — the entire block configuration (provider, countries, refresh schedule, `id_prefix`, `priority`, `include_inactive`, `cache_path`, etc.) is captured at boot; SIGHUP will not apply edits to these fields (the block shape is restart-frozen). The refresh ticker continues to hot-swap pool expansions at runtime (e.g., relay churn), but that is independent of SIGHUP.
- `failure.scoring.decay_interval` — the decay ticker is started once at boot; changing this field requires a restart to retune the goroutine's interval

## Use with Mullvad

Tunnelsmith ships a reference deployment at [`deploy/docker-compose.mullvad.yml`](deploy/docker-compose.mullvad.yml) that runs gluetun in WireGuard mode against your Mullvad account, plus tunnelsmith joining the same network namespace. A single `[[upstream_pool]]` block in the config fans out into one synthetic socks5 upstream per active Mullvad WireGuard relay in the countries you list. The scoreboard then learns per-host which relay works best. As of v1.1.0, relay-list churn is handled via hot-swap — no restart needed when Mullvad rotates relays.

### Quick setup

**1. Generate a WireGuard keypair** on [mullvad.net/en/account/wireguard-config](https://mullvad.net/en/account/wireguard-config). Download the `.conf` file and copy the `PrivateKey` and `Address` values. Each keypair counts against Mullvad's 5-device cap.

**2. Populate `deploy/.env`:**

```sh
cp deploy/.env.example deploy/.env
$EDITOR deploy/.env
# Set MULLVAD_WIREGUARD_PRIVATE_KEY and MULLVAD_WIREGUARD_ADDRESSES
```

**3. Pick your exit countries** in `deploy/tunnelsmith.mullvad.toml`:

```toml
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden", "Netherlands", "Switzerland"]
```

**4. Bring up the stack:**

```sh
docker compose -f deploy/docker-compose.mullvad.yml up -d
```

**5. Smoke test:**

```sh
curl --socks5-hostname localhost:1080 https://am.i.mullvad.net/json | jq .
# Expected: "mullvad_exit_ip": true, with a country in your list
```

See [`docs/deployment.md`](docs/deployment.md) for the full walkthrough including troubleshooting.

## For container maintainers

If you build a container that benefits from outbound proxying (`*arr` apps, scrapers, downloaders, RSS pollers, federated services), [`docs/integration-guide.md`](docs/integration-guide.md) is a layered checklist for shipping Tunnelsmith support, from "document the standard env-var pattern" to "ship an official compose snippet". The lowest level is no code changes.

For HTTPS coverage of the per-host scoreboard, the optional Phase 11 cooperative reporting protocol lets your app submit per-request outcomes back to Tunnelsmith. Three lines of Go via the [`client`](client) package, or any HTTP client in any language using the wire protocol at [`docs/cooperative-reporting.md`](docs/cooperative-reporting.md):

```go
// Go SDK — three-line integration; error handling shown inline.
c, err := client.New(client.Options{
    ProxyURL:   "http://tunnelsmith:8080",
    ControlURL: "http://tunnelsmith:9092",
})
if err != nil {
    log.Fatal(err)
}
resp, err := c.Get("https://example.com/api/things")
if err != nil {
    log.Fatal(err)
}
defer resp.Body.Close()
// Reports are best-effort; errors are safe to ignore (or log via client.Options.Logger).
_ = client.Report(resp, "ok")
```

Operators in multi-tenant or LAN-exposed deployments can opt into Phase 12 bearer-token auth on `/v1/report` via `[control].auth_tokens` / `[control].auth_tokens_file`; the empty default keeps the Phase 11 wire shape byte-for-byte.

## Documentation

- [`docs/configuration.md`](docs/configuration.md): every config key explained
- [`docs/architecture.md`](docs/architecture.md): how the scoreboard works (Phase 4)
- [`docs/deployment.md`](docs/deployment.md): running Tunnelsmith with Mullvad and other upstreams (Phase 6)
- [`docs/observability.md`](docs/observability.md): Prometheus metrics, persistence, and SIGHUP hot-reload (Phase 7)
- [`docs/ui.md`](docs/ui.md): the web UI, the four action endpoints, and the security stance (Phase 9)
- [`docs/request-lifecycle.md`](docs/request-lifecycle.md): end-to-end trace of a single request
- [`docs/integration-guide.md`](docs/integration-guide.md): for container maintainers adding Tunnelsmith support
- [`docs/cooperative-reporting.md`](docs/cooperative-reporting.md): wire protocol for app-driven outcome reporting and the Phase 12 bearer-token auth section
- [`docs/roadmap.md`](docs/roadmap.md): what's deliberately out of scope for v1 and what's tracked for v2
- [`docs/decisions.md`](docs/decisions.md): architecture decision records

## Metrics and persistence

When `metrics.bind` is set (default `:9090`), Tunnelsmith exposes Prometheus metrics at `/metrics`. Setting `cache.persist_path` makes the scoreboard survive restarts via a gob snapshot on `cache.persist_interval`. Sending `SIGHUP` re-reads the config file and applies upstreams, scoring, and detection rules in place. See [`docs/observability.md`](docs/observability.md) for the metric reference, scrape config, and Grafana dashboard import.

## Web UI

The web UI on `:9091` (set `[ui] bind` to change or to `""` to disable) renders the live scoreboard, the upstream pool, and the active force pins, plus four action endpoints (`forget`, `force`, `force/clear`, `reset`). The port is unauthenticated by design; bind it to a private interface only. See [`docs/ui.md`](docs/ui.md). Unraid users can install via the Community Apps template at [`deploy/unraid-template.xml`](deploy/unraid-template.xml).

## License

MIT. See [`LICENSE`](LICENSE).
