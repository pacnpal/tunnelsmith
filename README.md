# Tunnelsmith

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A per-destination egress router for HTTP and SOCKS5. Picks the right exit for each URL based on what is actually working.

> **Status: pre-alpha.** The repository scaffold is in place. Functionality lands phase by phase. Do not deploy this yet.

## What it does

Tunnelsmith sits in front of apps as an HTTP and SOCKS5 proxy. For each request, it looks up the destination host in a per-(host, upstream) scoreboard, picks the highest-scored upstream that is not in cooldown, tries the request, and updates the scoreboard based on the outcome. When an upstream starts failing for a host, Tunnelsmith demotes it for that host only and tries the next-best candidate. When the upstream recovers, Tunnelsmith probes it occasionally and lets it climb back up. The result is a proxy that learns which exit works for which destination and adapts as conditions change.

This is the gap between HAProxy (no per-host memory), Squid (static rules, no learning), and `scrapy-rotating-proxies` (Scrapy-only, alive/dead per proxy globally).

## Documentation

- [`docs/architecture.md`](docs/architecture.md): how the scoreboard works
- [`docs/configuration.md`](docs/configuration.md): every config key explained
- [`docs/deployment.md`](docs/deployment.md): running Tunnelsmith with Mullvad and other upstreams
- [`docs/decisions.md`](docs/decisions.md): architecture decision records

## License

MIT. See [`LICENSE`](LICENSE).
