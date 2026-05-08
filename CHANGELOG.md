# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added (Phase 5)

- `internal/failure`: `ParseRetryAfter` covers RFC 7231 §7.1.3 in both shapes (non-negative integer seconds and any of the three HTTP-date forms `net/http.ParseTime` accepts), and `StatusDetector` maps configured `[[failure.status]]` rules to a `failure.Kind` plus an optional honored `Retry-After` duration. Codes outside the supported set (429 / 403 / 451) are skipped silently because no `failure.Kind` exists for them yet.
- `internal/listener` (HTTP plain-HTTP forward path): the forward path now drives its own pick-dial-detect retry loop on top of the scoreboard. 429, 403, and 451 responses are recorded against the (host, upstream) entry with the right kind, the response is drained, and the request rotates to the next-best upstream within the same retry budget. 5xx and other 4xx responses pass through unchanged. Successful responses carry `X-Tunnelsmith-Upstream` and `X-Tunnelsmith-Retries`; the cascade-failure 502 carries `X-Tunnelsmith-Cascade` and `X-Tunnelsmith-Retries`.
- `internal/listener` (HTTP): one `http.Transport` per upstream, lazily created and cached, so HTTP keep-alive can pool conns to the destination through that upstream. The Phase 4 single-shared-Transport with `DisableKeepAlives = true` is gone; per-upstream Transport pools are the right shape now that the listener picks upstreams itself.
- `internal/scoreboard`: `Scoreboard.TripCascade(host)` is exported so the listener's plain-HTTP loop can mark cascade after exhausting every upstream for one request. Internal callers converged on the public name.
- `docs/configuration.md`: new "CONNECT and SOCKS5: what status detection cannot see" section explaining why neither status detection nor header injection runs on those paths.

### Added (Phase 4)

- `internal/scoreboard` package: per-(host, upstream) scoreboard with score, cooldowns, time decay, cascade-failure handling with negative TTL, recovery probing, failure debounce, and an injectable random source. `Scoreboard.DialFor` is the listener-side dial entry point; it walks Pick → Dial → Record in a loop bounded by the pool's retry cap and trips cascade for the host on full failure.
- `internal/failure`: `Kind` enum (`KindRefused`, `KindTimeout`, `KindRateLimit`, `KindForbidden`, `KindLegalBlock`, `KindBodyMatch`) plus `ClassifyDialError`, which prefers `KindTimeout` over `KindRefused` when an error satisfies both.
- `internal/config`: new `[failure.scoring]` section with per-kind penalty / cooldown for refused and timeout, `success_weight`, `score_cap`, `probe_chance`, `decay_interval`, `decay_step`, `cascade_ttl`, and `debounce_window`. Defaults match the proposal; per-key IsDefined defaulting lets users override one knob without restating the rest.
- `internal/upstream`: `Pool.Entries()` and `Pool.RetryCap()` accessors so the scoreboard can read the configured pool without mutating it.
- `internal/listener`: HTTP and SOCKS5 listener constructors now take a `*scoreboard.Scoreboard` instead of a `*upstream.Pool`. The pool is still the source of truth for what upstreams exist; the scoreboard wraps it. `Pool.DialFor` stays as a lower-level routine for the upstream package's own unit tests.
- `cmd/tunnelsmith` builds the scoreboard from `cfg.Failure.Scoring` and starts the decay goroutine for the lifetime of the process; the scoreboard log line on startup names the active probe chance, decay interval, cascade TTL, and debounce window.
- `docs/architecture.md`: scoreboard design (placeholder is gone). Covers Pick, RecordSuccess, RecordFailure, debounce, cascade, decay, locking, and what is deliberately not in Phase 4 yet.
- `docs/configuration.md`: `[failure.scoring]` table with every key, default, and validation rule.
- `examples/tunnelsmith.toml`: commented `[failure.scoring]` block showing the defaults.
- README "How it works" subsection summarizing the dial path and linking to the architecture doc.

### Security

- Bumped `golang.org/x/net` from v0.35.0 to v0.38.0 to absorb the fixes for GHSA-qxp5-gwg8-xv66 (HTTP proxy bypass via IPv6 Zone IDs, directly relevant) and GHSA-vvgc-356p-c3xw (XSS in `html`). Project `go` directive moves from `1.22` to `1.23.0` as a knock-on; CI and Dockerfile follow. See ADR-002 for the rationale.

### Fixed (review remediation, PRs #6 / #7 / #8)

- `internal/config`: defaults no longer overwrite user-provided zero or false values. `connection_refused = false`, `priority = 0`, `failure.timeout_ms = 0`, and `failure.max_retries_per_request = 0` are kept verbatim so the user's intent reaches `Validate`. `UpstreamConfig.Priority` moves to `*int`; use `PriorityValue()` to read the resolved priority.
- `internal/config`: shared `validatePort` helper drops the duplicated host:port port validation in `validateAddr` and `validateUpstreamAddr`.
- Documentation cleanups: dropped two dead pointers to the gitignored `tunnelsmith-proposal.md` (package comment, configuration docs); clarified `examples/tunnelsmith.toml`'s `host_glob` comment so it does not imply `*` matches a single dot-separated label.
- `internal/listener` (HTTP): `handleConnect` now drains the `bufio.Reader` returned by Hijack before tunneling, so bytes a client pipelines after the CONNECT request line (typical: the TLS ClientHello) reach the upstream instead of being lost.
- `internal/listener` (HTTP): forward-proxy requests share one `http.Transport` instead of building one per request. Per-request "which upstream served you" logging still works via a context-stashed `*string`. The misleading "one connection at a time" comment is gone.
- `internal/listener` (SOCKS5): `Shutdown` force-closes tracked client conns when its context expires before the WaitGroup drains, fixing a goroutine leak / hang against idle clients.
- `internal/listener` (HTTP / SOCKS5): both constructors fail fast on `nil` pools instead of letting the first request nil-deref.
- `internal/upstream`: pool failure logs now include `kind` ("refused" / "timeout" / "other"), use `"err", err` for slog instead of `err.Error()`, and a pre-canceled context returns a clear "context canceled before any upstream was tried" error rather than counting itself as one attempt.
- `internal/failure`: timeout test no longer dials `192.0.2.1` (TEST-NET-1); a synthetic `net.Error` exercises the classifier deterministically.
- `internal/upstream`: new test file covers `New()` per-kind selection, input validation, CONNECT handshake success / non-2xx / buffered-bytes paths, network-kind validation, context-bounded dial behavior, and a real SOCKS5 round-trip.

### Added (Phase 3)

- `internal/upstream` package: `Pool` type with priority-ordered fallback and a per-request retry cap. `NewPool` validates inputs and stably sorts entries by `Priority`; `DialFor(ctx, network, addr)` walks the list, returns the first conn that opens, and on full failure surfaces `errors.Join` of every per-attempt error with each upstream id prefixed.
- `internal/failure` package: `IsConnectionRefused` and `IsTimeout` classifiers. Walk wrapped errors via `errors.Is` / `errors.As` so they hold up against the `*net.OpError` wrappers `net.Dial` produces and against the `net.Error` interface that timeout errors implement.
- `cmd/tunnelsmith` now builds the full pool from every `[[upstream]]` block and wires both listeners through it. Each dial attempt emits a structured slog line with `upstream_id`, `host`, `outcome`, `latency_ms`, and `attempt`; the listener's existing `connect closed` and `forward done` lines pick up an `upstream_id` field naming the upstream that served the request.
- `deploy/tunnelsmith.example.toml` now ships an unreachable SOCKS5 upstream at priority 10 plus `direct` at priority 20 so the CI integration step exercises the retry path end-to-end. `examples/tunnelsmith.toml` has an explanatory note describing the priority pool semantics.

### Added (Phase 2)

- `internal/upstream` package: `Upstream` interface plus `direct`, `http` (CONNECT), and `socks5` implementations. The factory `New(cfg, timeout)` returns the right impl for each `[[upstream]]` block.
- `internal/listener` package: HTTP CONNECT plus forward proxy listener and SOCKS5 listener. Both expose `Ready()` for bind-completion signalling and `Shutdown(ctx)` for graceful drain bounded by the caller's context.
- `cmd/tunnelsmith` now starts both listeners against the first configured upstream (Phase 2 contract; the priority pool lands in Phase 3) and shuts them down cleanly on SIGINT or SIGTERM.
- `deploy/docker-compose.example.yml` and `deploy/tunnelsmith.example.toml`: minimal stack for CI integration testing of the listeners.
- README "Quick start" with `curl --proxy` and `curl --socks5-hostname` examples.
- Dependencies: `github.com/armon/go-socks5 v0.0.0-20160902184237-e75332964ef5` (last commit on `master`, no tagged releases), `golang.org/x/net v0.35.0` (last release with `go 1.18` directive, compatible with this project's `go 1.22`), `golang.org/x/sync v0.10.0` for `errgroup`.

### Added (Phase 1)

- `internal/config` package: TOML loader with defaults and validation. Rejects bad input with file-prefixed error messages and surfaces unknown keys as warnings rather than failing closed.
- `Config`, `ListenerConfig`, `CacheConfig`, `UpstreamConfig`, `FailureConfig`, `StatusRule`, `RuleConfig` types matching the proposal's TOML schema. `Duration` wrapper so `time.Duration` round-trips through TOML.
- Defaults for listener addresses, cache TTLs, failure timeouts, retry caps, and the recommended 429 / 403 / 451 status rules.
- `cmd/tunnelsmith` flags: `--config` (path to TOML), `--print-config` (load and reprint resolved TOML), `--version`.
- Structured JSON logging via `log/slog`; level configurable through `TUNNELSMITH_LOG_LEVEL`.
- Dependency: `github.com/BurntSushi/toml v1.6.0`.
- `examples/tunnelsmith.toml` rewritten as a complete commented v1 example.
- `docs/configuration.md` documents every key, type, default, and validation rule.
- README "Configuration" section pointing to the example and the docs.

### Added (Phase 0)

- Initial repository scaffolding: directory layout, Go module, version-printing CLI in `cmd/tunnelsmith`.
- `Makefile` with `build`, `test`, `lint`, `docker`, `clean`, `tidy` targets.
- `Dockerfile` (multi-stage, distroless) intended for CI use only; see ADR-001.
- `.golangci.yml` configured for golangci-lint v2 with errcheck, govet, ineffassign, staticcheck, unused, plus gofmt and goimports as formatters.
- `.gitignore`, `.dockerignore`.
- `LICENSE` (MIT).
- README, CHANGELOG, placeholder docs in `docs/` (architecture, configuration, deployment).
- ADR-001: image builds happen in CI only, not on developer hosts.
- GitHub Actions: `ci.yml` runs lint, test, build, and a CI-only docker build; `release.yml` builds and pushes multi-arch images on `v*` tags.
