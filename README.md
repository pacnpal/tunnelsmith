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

## Quick start

The binary boots an HTTP CONNECT plus forward proxy on `:8080` and a SOCKS5 listener on `:1080`. The pool tries upstreams in priority order and retries on hard failure up to `failure.max_retries_per_request` (default 5). [`deploy/tunnelsmith.example.toml`](deploy/tunnelsmith.example.toml) ships an unreachable SOCKS5 entry first and `direct` as the fallback, so each curl below exercises the retry path. Image builds run in CI per ADR-001; locally, run the binary directly:

```sh
make build
./bin/tunnelsmith --config deploy/tunnelsmith.example.toml
```

In another shell:

```sh
curl --proxy http://localhost:8080 https://example.com
curl --socks5-hostname localhost:1080 https://example.com
```

The Tunnelsmith logs show one `upstream dial` line per attempt with `outcome=failure` for the unreachable entry and `outcome=success` for `direct`. CI uses [`deploy/docker-compose.example.yml`](deploy/docker-compose.example.yml) to exercise the same path against the built image.

## Configuration

Tunnelsmith reads a TOML config from `--config` (default `/etc/tunnelsmith/config.toml`). A complete commented example is at [`examples/tunnelsmith.toml`](examples/tunnelsmith.toml). Every key is documented in [`docs/configuration.md`](docs/configuration.md).

`tunnelsmith --config <path> --print-config` loads, applies defaults, and prints the resolved config. Use it to confirm what the binary actually sees.

`TUNNELSMITH_LOG_LEVEL` (`debug` | `info` | `warn` | `error`, default `info`) controls log verbosity. Logs are JSON, one line per event.

## Use with Mullvad

Tunnelsmith ships a reference deployment at [`deploy/docker-compose.mullvad.yml`](deploy/docker-compose.mullvad.yml) that runs gluetun in WireGuard mode against your Mullvad account, plus tunnelsmith joining the same network namespace. A single `[[upstream_pool]]` block in the config fans out into one synthetic socks5 upstream per active Mullvad WireGuard relay in the countries you list. The scoreboard then learns per-host which relay works best.

Setup is a one-time keypair generation (counts against Mullvad's 5-device cap) plus two repo-secret-shaped env vars. See [`docs/deployment.md`](docs/deployment.md) for the full walkthrough.

## For container maintainers

If you build a container that benefits from outbound proxying (`*arr` apps, scrapers, downloaders, RSS pollers, federated services), [`docs/integration-guide.md`](docs/integration-guide.md) is a layered checklist for shipping Tunnelsmith support, from "document the standard env-var pattern" to "ship an official compose snippet". The lowest level is no code changes.

For HTTPS coverage of the per-host scoreboard, the optional Phase 11 cooperative reporting protocol lets your app submit per-request outcomes back to Tunnelsmith. Three lines of Go via the [`client`](client) package, or any HTTP client in any language using the wire protocol at [`docs/cooperative-reporting.md`](docs/cooperative-reporting.md). Operators in multi-tenant or LAN-exposed deployments can opt into Phase 12 bearer-token auth on `/v1/report` via `[control].auth_tokens` / `[control].auth_tokens_file`; the empty default keeps the Phase 11 wire shape byte-for-byte.

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
