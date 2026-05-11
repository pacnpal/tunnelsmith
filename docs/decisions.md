# Architecture Decision Records

Each record captures a non-obvious choice that shaped Tunnelsmith. Past tense, short context, decision, alternatives considered, and the immediate consequences.

## ADR-001: Docker images are built in CI, not on developer hosts

### Context

The release workflow used to be: a developer ran `docker buildx` locally, pushed a tag to GHCR, and updated the changelog. The image hash diverged across developers; reproducing a release on a different host took unpredictable amounts of fiddling. A release that included a single typo in the Dockerfile would not be caught until the published image was pulled.

### Decision

Tunnelsmith images are built by GitHub Actions on push of a `v*` tag. The workflow runs `docker buildx build --push` against a pinned base image and emits the canonical `linux/amd64` + `linux/arm64` manifest list to `ghcr.io/pacnpal/tunnelsmith`. Developer machines do not push releases.

### Alternatives considered

- Self-hosted runner. Same trust as a developer machine; rejected.
- Skip multi-arch. Would force arm64 users (the project's largest user segment per the issue tracker) to build their own image. Rejected.

### Consequences

- Release cadence is governed by tags; cutting `v0.x` is a single `git tag -s v0.x && git push --tags`. No follow-up manual steps.
- Local `make docker` is a development convenience only. It produces an image with `version=dev`; CI overwrites the LDFLAGS.
- A broken Dockerfile fails CI loudly rather than producing a silent bad image.

## ADR-002: Bump Go directive to 1.23 to absorb golang.org/x/net security fixes

### Context

`go.mod` declared `go 1.21`. CVE-2024-45337 (in `golang.org/x/crypto`, fixed in 0.31.0) pulled in dependencies that themselves required Go 1.22 minimum. CVE-2025-22871 (in `golang.org/x/net`, fixed in 0.38.0) raised the floor further.

### Decision

Bump `go.mod` to `go 1.23.0`. CI matrix runs 1.23 only; older versions are not supported.

### Alternatives considered

- Pin x/net at the last 1.21-compatible version. Rejected: forfeits the CVE fix.
- Use module-level replace directives. Rejected: opaque, breaks `go install`.

### Consequences

- Distro packagers on long-tail distros (Debian Bullseye Go 1.19) cannot build Tunnelsmith from source without backporting Go. We document the requirement in the README. The container image is the recommended distribution channel anyway.
- Future toolchain bumps are routine; this ADR sets the precedent.

## ADR-003: Mullvad integration uses WireGuard, not OpenVPN

### Context

Mullvad's OpenVPN support EOL'd on 2026-01-15. The original deployment plan called for two paths: an OpenVPN-based stack for compatibility and a WireGuard one for performance. After the EOL, only WireGuard remains.

### Decision

`deploy/docker-compose.mullvad.yml` runs gluetun in WireGuard mode. The legacy OpenVPN compose file was removed.

### Alternatives considered

- Run a self-hosted WireGuard tunnel without gluetun. Rejected: gluetun handles per-server selection, kill-switching, and DNS leak prevention. Reinventing those is out of scope.
- Skip WireGuard entirely and run OpenVPN against a third-party that still supports it. Rejected: Mullvad is the documented vendor; using a different vendor for the reference stack would surprise users.

### Consequences

- The compose file pins gluetun by SHA. Updates are explicit.
- WireGuard requires a Mullvad-issued keypair per device. Each developer/staging environment needs its own; the docs say so.
- Performance is materially better than OpenVPN was. The scoreboard's debounce window is unchanged.

## ADR-004: Mullvad SOCKS5 hostname pattern is per-server multihop, not the form in the original plan

### Context

The original deployment plan said "use `socks5-{country}-{city}.relays.mullvad.net:1080` from inside the tunnel". That hostname pattern does not actually exist in production. Mullvad publishes one SOCKS5 server per WireGuard relay, addressed via per-server multihop: `{relay}-socks5-{number}.relays.mullvad.net:1080` where `{relay}-{number}` is the relay's WireGuard hostname.

### Decision

Tunnelsmith's `[[upstream_pool]] provider = "mullvad"` block fetches Mullvad's relay-list API, filters by country, and emits one synthetic `[[upstream]]` per active relay using the transformation `^(.+-wg)-(\d+)$` → `$1-socks5-$2.relays.mullvad.net:1080`. Operators name countries; the binary computes the rest.

### Alternatives considered

- Hand-curate a list of `socks5-{country}-{city}` hostnames. Rejected: the names do not exist.
- Use `socks5.mullvad.net` (the single-server form). Rejected: it routes through one Sweden exit regardless of upstream choice, which defeats the per-host scoreboard's purpose.

### Consequences

- The relay list lives behind an HTTP fetch and a small cache. The cache survives a Mullvad API outage; the live fetch is the source of truth.
- Per-server multihop carries a small extra latency cost vs. single-hop. The scoreboard's per-(host, upstream) scoring handles it transparently.
- Per-server granularity is what makes "this relay works for this destination" a meaningful signal. The single-server form would erase that.

## ADR-005: Release-image verification runs in CI, not on the host

### Context

We need to confirm that the image published to `ghcr.io/pacnpal/tunnelsmith:vN.M` runs, binds the listeners, exposes `/metrics`, and serves a CONNECT request. Running that check on a developer host requires Docker, network connectivity to GHCR, and a known port — friction every release.

### Decision

A post-publish GHCR-verification job in the release workflow pulls the freshly-published manifest by digest, runs it with a minimal config, hits `/healthz` and `/metrics`, drives a CONNECT through it, and fails the release if any check fails. Developers running `make release-verify` get the same script but against `:dev`.

### Alternatives considered

- Skip the verification step. Rejected: a broken image stays published until a user reports it.
- Run the verification before publishing. Rejected: the digest does not exist until after publish, and pinning by tag instead of digest opens the race window of a same-tag overwrite.

### Consequences

- A bad release is caught by CI before any user pulls it. Job time adds ~90s.
- The verification script is reused for the Mullvad-stack smoke check. One script, two call sites.

## ADR-006: HTTPS coverage uses cooperative app reporting, not TLS interception

### Context

The scoreboard's headline value is "learn which exit works for which destination". On plain HTTP that's automatic — the listener sees status codes and body bytes. On HTTPS via CONNECT and SOCKS5 it sees nothing past the handshake. A naive approach to recover HTTPS coverage is TLS interception (MITM): provision a CA, install it in every client, terminate TLS at Tunnelsmith, inspect the response, re-encrypt to the destination. That works for some closed environments but is poisonous for the project's user base (`*arr` apps, scrapers, etc.) because every app would need the CA in its trust store and every certificate-pinned destination would break.

### Decision

Tunnelsmith does not intercept TLS. Instead, the Phase 11 control endpoint accepts cooperative reports: an app that already terminates TLS (because it is the legitimate endpoint) submits per-request outcomes via `POST /v1/report` and Tunnelsmith feeds them into the scoreboard exactly like a listener-detected status code. The Go SDK ships in this repo; the wire protocol is documented at `docs/cooperative-reporting.md` and any HTTP client in any language can speak it.

### Alternatives considered

- TLS interception with a Tunnelsmith CA. Rejected: cert pinning incompatibility, install ceremony, security stance — the project is "a smart proxy", not "a MITM tool".
- Sniff TCP-level metrics (RTT, retransmits) and infer outcome. Rejected: too noisy, and CONNECT semantics hide most of the useful signal anyway.
- Run the proxy as an HTTPS forward proxy (no CONNECT) and have apps trust Tunnelsmith's cert. Rejected: every HTTPS client would need explicit configuration to trust a non-public CA, and the cert-pinning issue is unchanged.

### Consequences

- App maintainers who want HTTPS coverage do three lines of integration work or speak the wire protocol directly. That's the price of correctness.
- The control listener is a new attack surface (Phase 11). The default is "no auth" because the trust boundary is the network; Phase 12 adds opt-in bearer tokens.
- HTTPS without app integration still works — the proxy still routes traffic correctly. It just doesn't learn from per-request outcome past TCP success.

## ADR-007: Bearer-token auth on the control endpoint (Phase 12)

### Context

Phase 11 named "bearer-token auth as a follow-up" in the trade-offs section. Phase 12 ships it.

The design space was small. Bearer is the simplest credible primitive: stateless, no key infrastructure, fits in one HTTP header, every HTTP client supports it. The alternatives (mTLS, OAuth/JWT, signed payloads) all add infrastructure the workload does not need yet.

### Decision

1. Add an optional bearer-token auth gate to `POST /v1/report`. Tokens are configured via:
   - `[control].auth_tokens = ["t1", "t2"]` (inline list), and/or
   - `[control].auth_tokens_file = "/etc/tunnelsmith/control_tokens"` (one token per line, `#` comments OK, hot-reloadable on SIGHUP).
2. Empty token set is the **default** and is identical to Phase 11 behavior. Upgrade is non-breaking; operators opt in by adding tokens.
3. Token verification uses `crypto/subtle.ConstantTimeCompare` to defeat timing oracles. Lint enforces no `==` against credential bytes.
4. 401 responses include `WWW-Authenticate: Bearer realm="tunnelsmith"`.
5. Two new reject reasons in `tunnelsmith_reports_rejected_total{reason}`: `auth_missing` (no `Authorization` header but auth enabled) and `auth_failed` (header present but token did not match or was malformed).
6. SIGHUP re-reads tokens from config and from `auth_tokens_file`. New tokens take effect for the next request; in-flight requests keep their existing accept/reject decision.
7. The Go SDK adds `Options.Token`. When non-empty, every report POST (auto-report and synchronous `Report`) carries `Authorization: Bearer <token>`.
8. `GET /healthz` stays ungated by default so liveness probes work without a token.

### Non-goals (named explicitly)

- **TLS on the control listener.** Auth without TLS still leaks tokens to anyone sniffing the network. Operators who need both should run the listener behind a reverse proxy that terminates TLS, or wait for a future phase that lands TLS on `internal/control`. Phase 12 does not bundle TLS because the two changes have independent rollouts and combining them would slow both.
- **mTLS, OAuth, JWT, JWKS, OIDC.** Bearer is the right size for v1 of auth. Anything richer is a separate ADR if a workload demands it.
- **Per-token rate limits or scopes.** A token that can submit reports submits any report. Multi-token support is for rotation and (future) auditability, not for limiting any single token's surface.
- **Token storage.** Tunnelsmith does not generate, rotate, or persist tokens. Operators do that out of band.
- **Auth on the UI listener.** Same network-boundary stance there; orthogonal change.

### Consequences

- Operators who run the control endpoint on loopback or a private subnet can keep doing so unchanged. Empty token set = same wire shape as Phase 11.
- Operators in multi-tenant or LAN-exposed deployments get a credible auth gate without ceremony. Each app gets its own token; rotation is a SIGHUP away.
- Token compromise is a real failure mode. Without TLS on the control listener, bearer tokens travel in plaintext over the network path between the app and Tunnelsmith and can be captured by any observer on the Docker network, the LAN, or any intermediate hop — see the TLS entry under Non-goals above. Mitigations: bind the listener to loopback or to a network segment the operator already trusts, use short-lived tokens, rotate periodically, and when Phase 13 lands the listener-side TLS, terminate TLS at the listener. Operators evaluating Phase 12 for multi-tenant or LAN-exposed deployments should treat plaintext-token transmission as the primary risk to plan around until that follow-up ships.
- The wire protocol stays backward-compatible. Existing apps without tokens keep working as long as the operator has not configured tokens server-side. Once tokens are configured, every app must opt in or it gets 401.
- The metrics reject vocabulary grows by two labels. Operators with alerting on `tunnelsmith_reports_rejected_total` should add a panel for `auth_missing` + `auth_failed` to catch misconfigured clients.
- The control package gains a small `auth.go` and a `tokenSet` abstraction; the rest of the surface is one new option per existing constructor.

### References

- `docs/cooperative-reporting.md` — wire-protocol contract that grows the optional `Authorization` header.
- `docs/roadmap.md` — planned follow-up tracking for Phase 12.
- ADR-006 — the Phase 11 decision this builds on.
- RFC 6750 — Bearer Token usage in HTTP Authorization headers.

## ADR-008: `[[upstream_pool]]` providers are pluggable via a registry, with an optional vendor-API surface

### Context

Tunnelsmith's first release shipped one provider, Mullvad, hard-coded into `cmd/tunnelsmith` via a `switch block.Provider { case "mullvad": ... }`. Adding a second provider (Webshare) under that shape would mean editing `cmd/tunnelsmith/main.go`, `internal/config/config.go`'s enum, and growing the validation switch — and the next provider after Webshare would mean the same edits again. The fan-out is small per change but the call graph keeps `cmd/` coupled to every provider's wire details, which makes both code review and forks-with-additional-providers harder than necessary.

Three forces are in tension:

1. **Closed set, in-binary providers.** Every provider must ship in the Tunnelsmith binary at build time. Out-of-process providers (gRPC, plugin sockets) would require multi-process orchestration we explicitly don't want.
2. **Different vendors expose different surfaces.** Mullvad has nothing operator-callable — its API is a public read-only relay list. Webshare has paginated lists *plus* an on-demand refresh endpoint operators legitimately want to script. Future providers will have different shapes again (per-IP replacement, profile/subscription).
3. **Fork-and-PR contributions are a first-class workflow.** Users who already pay for a vendor like Bright Data or IPRoyal should be able to ship support in their own fork, gather feedback, and propose the package upstream without rewriting half of the binary.

### Decision

Introduce three small interfaces in `internal/upstream/provider`:

- `Expander` (`Snapshot` + `RunRefresh`) — the shape every provider has, owns the "fan out to `[]config.UpstreamConfig`" path.
- `API` (`RefreshProxyList` for v1; growable) — the optional vendor-API surface. Providers without one return `provider.ErrAPINotSupported` from `Provider.BuildAPI`.
- `Provider` (`Name` + `ValidateConfig` + `BuildExpander` + `BuildAPI`) — the registry entry; one per supported vendor.

Plus a package-global `Registry`. Each provider package's `init()` calls `provider.MustRegister(NewProvider())`. The aggregator package `internal/upstream/providers` blank-imports every supported provider and binds `config.SetProviderValidator` so `config.Validate` defers per-block validation to the provider's own rules.

The control listener gains two routes:

- `GET /v1/providers` — lists every registered binding plus whether it has an API.
- `POST /v1/providers/{id_prefix}/refresh` — calls into `provider.API.RefreshProxyList`. Returns 501 for providers whose `BuildAPI` returned `ErrAPINotSupported`.

`cmd/tunnelsmith` no longer references any concrete provider type. Adding a new provider is a new package plus one blank-import line in `internal/upstream/providers`.

### Alternatives considered

- **Plugin sockets / RPC providers.** Rejected. Multi-process orchestration adds a runtime dependency, complicates failure modes (one process crashed; the other is fine), and doesn't fit the binary-distribution model.
- **Go `plugin` package.** Rejected. Only works on Linux/macOS, hostile to cross-platform builds, the symbol-resolution and version-skew issues are well-documented disasters.
- **A `ProviderKind` enum in `config/`.** Rejected. That was what we had — every new provider means editing the enum, the validation switch, and `cmd/tunnelsmith`. Three files per provider when one would do.
- **One mega-interface containing every possible vendor call.** Rejected. Mullvad would have to stub out a dozen methods, and the interface would churn every time a new vendor capability was added. The current `API` interface starts at one method and grows by adding methods explicitly (with the same `ErrAPINotSupported` escape hatch).

### Non-goals (named explicitly)

- **Hot-loaded providers.** A provider that wasn't compiled into the binary cannot run. The fork-and-PR workflow is the supported way to add one.
- **Dynamic provider registration over the network.** The control endpoint surfaces what was registered at startup. There is no `POST /v1/providers/register`.
- **Per-provider config schema generation.** TOML's static shape is shared across all providers. Provider-specific fields live on `UpstreamPoolConfig` and each provider's `ValidateConfig` decides whether the fields it cares about are well-formed.
- **TLS termination on the provider routes.** Shares the same network-boundary stance as ADR-007 — bind to loopback or a private subnet until the control listener gains native TLS.

### Consequences

- Adding a new provider is a single new package plus one blank import line. No `cmd/tunnelsmith` edits. No `config/` enum edits.
- Operators in multi-tenant deployments can rotate Webshare IPs (or any future provider's equivalent) through the control endpoint without shelling into the container or signaling the process.
- Providers without a vendor API surface (Mullvad) are first-class: `GET /v1/providers` reports `has_api: false`, the refresh route returns 501, and nothing about config or runtime behavior changes.
- `UpstreamConfig` grows two fields (`Username`, `Password`) to carry Webshare's per-proxy credentials. Mullvad expansion leaves them empty and the http/socks5 dialers no-op the auth path when username is `""`, so the change is transparent for the Mullvad deployment.
- The CHANGELOG growth pattern shifts: instead of "Phase N: new provider", new providers land under "Providers: added X" entries that don't need to coordinate with other phase work.

### References

- `internal/upstream/provider/provider.go` — interface definitions.
- `internal/upstream/provider/registry.go` — registry implementation.
- `internal/upstream/providers/providers.go` — aggregator + config validator hook.
- `internal/control/providers.go` — `GET /v1/providers` and `POST /v1/providers/{id_prefix}/refresh`.
- `docs/providers.md` — the adapter-author guide for fork + PR.
- `docs/control-api.md` — the route reference.
