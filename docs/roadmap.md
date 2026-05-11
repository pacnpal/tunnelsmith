# Roadmap

What's deliberately out of scope for v1, in rough priority order. Items listed here are not commitments; they're things we considered, decided not to ship in v1.0.0, and want to evaluate after we have real-world v1 deployment data.

## v1.x maintenance

These are incremental fixes against the v1 surface and may land in a v1.x point release rather than waiting for v2.

- ~~**`failure.connection_refused` opt-out actually works.**~~ Shipped in [#24](https://github.com/pacnpal/tunnelsmith/pull/24) (closes [#20](https://github.com/pacnpal/tunnelsmith/issues/20)). `Scoreboard.DialFor` now consults `Config.ConnectionRefused` before scoring an ECONNREFUSED dial outcome, the HTTP forward path applies the same gate before scoring, and the listener exposes `ReloadConnectionRefused` so SIGHUP picks up live edits.
- ~~**Hot-reload of `[[upstream_pool]]` Mullvad relay churn.**~~ Shipped in Phase 11.1. The refresh tick now hot-swaps the running priority pool on every successful diff via `Scoreboard.ReplacePool`; cached HTTP transports are dropped, the gauge refresher updates, and `tunnelsmith_pool_hotswap_total{result}` records the swap. SIGHUP still skips pool changes when `[[upstream_pool]]` is configured (static `[[upstream]]` edits remain restart-only in pool deployments) since the refresh ticker now owns pool-shape changes end-to-end.

## Planned

Designs that have been locked in but not yet implemented. Each has an ADR in `docs/decisions.md`.

- ~~**Phase 12: bearer-token auth on the control endpoint.**~~ Shipped in [#27](https://github.com/pacnpal/tunnelsmith/pull/27). `[control].auth_tokens` (inline) and `[control].auth_tokens_file` (one token per line, `#` comments, dedup'd union) configure an opt-in token set; empty set keeps the Phase 11 wire shape byte-for-byte. `POST /v1/report` (and `/healthz` when `[control].gate_healthz = true`) require `Authorization: Bearer <token>`; 401 responses include `WWW-Authenticate: Bearer realm="tunnelsmith"` plus RFC 6750 §3 `Cache-Control: no-store` / `Pragma: no-cache`. Auth check uses `crypto/subtle.ConstantTimeCompare` over a per-request `TokenSnapshot` so SIGHUP rotation can't tear an in-flight decision. New `tunnelsmith_reports_rejected_total{reason}` labels: `auth_missing` and `auth_failed`. Go SDK gained `client.Options.Token`. See [ADR-007](decisions.md).
- ~~**Phase 13: pluggable `[[upstream_pool]]` providers + Webshare adapter.**~~ Shipped in [#32](https://github.com/pacnpal/tunnelsmith/pull/32). `internal/upstream/provider` defines a `Provider` / `Expander` / optional `API` interface trio plus a registry; each provider package registers itself in `init()` and `cmd/tunnelsmith` dispatches through `provider.Default().Lookup` with no concrete provider type referenced. The hard-coded provider enum in `internal/config` is replaced by a registry-backed validator. A first-class Webshare adapter ships alongside Mullvad: `provider = "webshare"` expands a Webshare plan into one HTTP-Basic-auth or SOCKS5 upstream per active proxy via paginated `GET /api/v2/proxy/list/` (max 500 pages, `valid = false` dropped), with a disk-cached last-known-good fallback. Two new bearer-gated control routes — `GET /v1/providers` and `POST /v1/providers/{id_prefix}/refresh` — expose vendor-side actions; vendor errors map to `429` / `504` / `502` by category. See [ADR-008](decisions.md) and [`docs/providers.md`](providers.md).
- ~~**Phase 14: TLS on the control listener.**~~ Shipped in [#33](https://github.com/pacnpal/tunnelsmith/pull/33). New `[control].tls_cert_file` + `[control].tls_key_file` keys (both-or-neither; absolute paths; restart-only) switch the listener from `http.Server.Serve` to `http.Server.ServeTLS` with `MinVersion: tls.VersionTLS12` pinned on the underlying `tls.Config`. Both empty preserves the Phase 11/12 plaintext wire shape byte-for-byte; only one set is a config-load error and `Serve` re-checks the invariant at startup so a caller that bypasses `config.Validate` cannot silently downgrade an intended-TLS listener to plaintext. The keypair is pre-loaded with `tls.LoadX509KeyPair` before `net.Listen` so a bad cert/key path surfaces without leaving an orphaned TCP socket bound on the control address. Cert rotation is restart-only in v1.2 — `GetCertificate` / file-watch hot-reload is an explicit non-goal. Combined with Phase 12 bearer-token auth, the plaintext-token risk ADR-007 named in its Non-goals is closed at the transport layer. See [ADR-009](decisions.md).

## v2 candidates

Bigger design decisions worth their own ADR before any code lands.

- **Global degradation detection.** "Upstream X is failing for every host, demote it globally." Tunnelsmith's per-(host, upstream) scoreboard intentionally keeps every host's view independent; a global signal would require either a second aggregated table or a periodic sweep. The proposal lists this as a refinement; the v1 alternative (cascade cooling per host) covers most of the value.
- **UDP support.** v1 is HTTP and SOCKS5 (TCP) only. UDP-over-SOCKS5 is in the SOCKS5 spec; the failure-detection model would have to change since UDP has no connection-refused.
- ~~**Transparent HTTPS interception.**~~ Resolved by Phase 11 cooperative reporting; superseded by [`ADR-006`](decisions.md). Apps that already terminate TLS submit per-request outcomes back to Tunnelsmith instead of the proxy decrypting the stream. MITM may be revisited if a workload demands coverage of uninstrumented HTTPS clients (browsers, closed-source binaries).
- **Custom failure-detection hooks.** Lua or Starlark scripts that decide failure from the response. The Phase 8 `[[rule]].body_regex` handles the most common case; full scripting is more flexibility than v1 needed.
- **Multi-instance clustering.** Two Tunnelsmith binaries sharing one scoreboard. v1 is single-process; a second instance starts cold. Useful for HA but adds a coordination dependency.
- **Authentication on the proxy listeners.** v1 binds to loopback or a private network and trusts the network boundary. A token or basic-auth gate would let it run on a public interface; matches Squid's model.
- **Active health-checking of upstreams.** v1 only learns from real traffic. Active probing would catch a quiet upstream that has been broken since startup, at the cost of touching exits the user has not asked for.

## How to propose changes

Open a GitHub issue with a `roadmap:` prefix in the title. If the change has design tradeoffs, draft an ADR in `docs/decisions.md` first; if it's a fix or a small extension, an issue is enough.
