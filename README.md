# Tunnelsmith

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A per-destination egress router for HTTP and SOCKS5. Picks the right exit for each URL based on what is actually working.

> **Status: pre-alpha.** The repository scaffold is in place. Functionality lands phase by phase. Do not deploy this yet.

## What it does

Tunnelsmith sits in front of apps as an HTTP and SOCKS5 proxy. For each request, it looks up the destination host in a per-(host, upstream) scoreboard, picks the highest-scored upstream that is not in cooldown, tries the request, and updates the scoreboard based on the outcome. When an upstream starts failing for a host, Tunnelsmith demotes it for that host only and tries the next-best candidate. When the upstream recovers, Tunnelsmith probes it occasionally and lets it climb back up. The result is a proxy that learns which exit works for which destination and adapts as conditions change.

This is the gap between HAProxy (no per-host memory), Squid (static rules, no learning), and `scrapy-rotating-proxies` (Scrapy-only, alive/dead per proxy globally).

## Quick start

The Phase 2 binary boots an HTTP CONNECT plus forward proxy on `:8080` and a SOCKS5 listener on `:1080`. With the minimal `direct` upstream in [`deploy/tunnelsmith.example.toml`](deploy/tunnelsmith.example.toml), both listeners route through the host's default route. Image builds run in CI per ADR-001; locally, run the binary directly:

```sh
make build
./bin/tunnelsmith --config deploy/tunnelsmith.example.toml
```

In another shell:

```sh
curl --proxy http://localhost:8080 https://example.com
curl --socks5-hostname localhost:1080 https://example.com
```

CI uses [`deploy/docker-compose.example.yml`](deploy/docker-compose.example.yml) to exercise the same path against the built image.

## Configuration

Tunnelsmith reads a TOML config from `--config` (default `/etc/tunnelsmith/config.toml`). A complete commented example is at [`examples/tunnelsmith.toml`](examples/tunnelsmith.toml). Every key is documented in [`docs/configuration.md`](docs/configuration.md).

`tunnelsmith --config <path> --print-config` loads, applies defaults, and prints the resolved config. Use it to confirm what the binary actually sees.

`TUNNELSMITH_LOG_LEVEL` (`debug` | `info` | `warn` | `error`, default `info`) controls log verbosity. Logs are JSON, one line per event.

## Documentation

- [`docs/configuration.md`](docs/configuration.md): every config key explained
- [`docs/architecture.md`](docs/architecture.md): how the scoreboard works (Phase 4)
- [`docs/deployment.md`](docs/deployment.md): running Tunnelsmith with Mullvad and other upstreams (Phase 6)
- [`docs/decisions.md`](docs/decisions.md): architecture decision records

## License

MIT. See [`LICENSE`](LICENSE).
