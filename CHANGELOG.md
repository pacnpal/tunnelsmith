# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
