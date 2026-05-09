# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added (Phase 8)

- `internal/upstream.RuleSet`: compiled view of `[[rule]]` blocks. `Match` evaluates rules in declaration order with `path.Match` semantics over the lowercased host (case insensitive, first-match-wins). Each rule carries the prefer list, the force flag, and pre-compiled body regexes so the request path never has to recompile a pattern. `CheckPreferIDs` validates `Prefer` entries against the merged upstream pool (including pool-derived ids).
- `internal/scoreboard.WithRules` / `ReplaceRules` / `PoolIDs`: scoreboard now consults the configured RuleSet on every Pick. `Force=true` filters non-prefer ids out of both the eligible set and the cooldown-fallback set, so a forced cascade only fires against the listed upstreams. `Force=false` adds a `preferRank` tiebreak that wins over score and base priority, putting preferred upstreams at the top in declaration order.
- `internal/failure.BufferAndDecide`: response-body inspector. Reads up to a caller-supplied byte cap from the upstream body, runs each pattern against the buffered prefix, and returns either a `Matched=true` decision (the listener rotates to the next upstream) or a `Replay` reader the listener writes to the client so a benign body still reaches the client byte-for-byte. Encoded bodies (`Content-Encoding != identity`) skip inspection so a regex never runs against bytes it cannot interpret.
- `internal/listener (HTTP).WithHTTPRules` / `WithHTTPBodyBufferKB` / `ReloadRules` / `ReloadBodyBufferKB`: handleForward consults the live RuleSet and the body buffer cap on every plain-HTTP request. A body-regex match records `KindBodyMatch`, drains the response, and rotates to the next upstream. CONNECT and SOCKS5 paths bypass body inspection entirely.
- `internal/config`: new keys. `[[rule]].body_regex` (per-host pattern list, compiled at load time), `failure.body_buffer_kb` (default `32`, capped at `1024`), `failure.scoring.body_match_penalty` (default `5`), `failure.scoring.body_match_cooldown` (default `60s`). `host_glob` is now probed with `path.Match` at validate time so a malformed pattern surfaces at startup. The top-level `failure.body_regex` field is parsed for forward compatibility but the runtime ignores it; a non-empty value triggers a one-line startup warning pointing operators at `[[rule]].body_regex`.
- `cmd/tunnelsmith`: startup compiles a RuleSet and attaches it to the scoreboard and the HTTP listener; SIGHUP rebuilds and validates the RuleSet against the projected pool before swapping it in. A failed compile or unknown prefer id leaves the previous rules live so a typo cannot silently disable rule-aware routing.
- `docs/configuration.md`: `[[rule]]` section documents glob semantics, prefer/force routing, body-regex semantics, and the encoding caveat. New `failure.body_buffer_kb` and body-match scoring knobs are listed alongside the existing scoring table.

### Added (Phase 7)

- `internal/metrics` package: Prometheus exposition surface backed by a private `*prometheus.Registry`. Counters, histograms, and gauges live under the `tunnelsmith_` namespace. The metric set is intentionally bounded to keep cardinality safe: per-upstream labels only, no per-host labels (per-host detail belongs in the Phase 9 web UI). Gauge names follow Prometheus convention (no `_total` suffix on gauges; the scoreboard-size gauge is `tunnelsmith_scoreboard_entries`). Dependency: `github.com/prometheus/client_golang v1.23.2`.
- `internal/metrics.Server`: exposes `/metrics` plus a cheap `/healthz` endpoint at the address `metrics.bind` names. Setting `metrics.bind = ""` disables the listener entirely.
- `internal/scoreboard.SaveSnapshot` / `LoadSnapshot`: gob-encoded scoreboard state with a fixed magic-plus-version header and an atomic temp-and-rename write. Missing files are not an error so a fresh install starts clean. `internal/scoreboard.PersistenceLoop` ticks on `cache.persist_interval` and runs one final flush at ctx cancellation; setting `persist_interval` to `"0s"` disables periodic writes.
- `internal/scoreboard.Prune` (closes #11): drops zero-score entries whose `lastSeen` is older than `failure.scoring.prune_after`, removes empty per-host maps, evicts expired cascade entries, and clears debounce keys older than `10 * debounce_window`. Runs from the persistence tick and at shutdown.
- `internal/scoreboard.ReplacePool` and `Reload`: hot-swap the upstream pool and the scoring tunings under a single write lock so concurrent Pick / Record* calls cannot tear. Per-(host, upstream) entries survive the swap; entries keyed off ids that no longer exist age out via Prune.
- `internal/listener (HTTP).Reload` and `CloseTransportsExcept`: live updates for the failure detector and the retry cap, plus a way to drop cached transports for upstreams that disappeared in the new pool.
- `cmd/tunnelsmith`: SIGHUP hot-reload. Re-reads the config file, validates it, and applies upstream list, scoring tunings, status detector, and retry cap in place. Listener bindings, decay interval, persistence path / interval, and `[[upstream_pool]]` refresh interval stay frozen at startup; a reload that violates that scope logs a warning and the binary keeps running on the old config.
- `cmd/tunnelsmith`: scoreboard gauge refresher copies `EntriesCount`, `CooledHostsByUpstream`, and `CascadeActiveCount` into the metrics registry every 5 seconds so `/metrics` mirrors the snapshot file.
- `internal/config`: new keys `metrics.bind` (default `:9090`; `""` disables), `cache.persist_interval` (default `30s`; `0s` disables periodic writes), and `failure.scoring.prune_after` (default `24h`; `0s` disables entry pruning).
- `docs/observability.md`: every metric documented (name, type, labels, meaning) plus a worked example of a Prometheus scrape config and a Grafana dashboard import.
- `deploy/grafana-dashboard.json`: importable dashboard with panels for request outcome rate, dial latency, status failures by upstream, scoreboard size, and cascade active hosts.
- Resolution for issue #12 (scoreboard lock contention): `BenchmarkScoreboardWriterContention` covers the worst-case mix of concurrent Pick + RecordSuccess writers running against a snapshotter on a 5ms tick and a SaveSnapshot on a 50ms tick. The bench shows ~3.3 microseconds per op at homelab scale (1k hosts, 20 upstreams) with no starvation of either auxiliary loop, so no mitigation is needed for v1.

### Added (Phase 6)

- `internal/upstream/mullvad` package: relay-list parser, disk-cache fallback, SOCKS5 hostname derivation per ADR-004, and an `Expander` that turns `[[upstream_pool]]` blocks into `config.UpstreamConfig` entries the priority pool consumes. Country filter is case-insensitive, inactive relays are dropped by default, malformed hostnames are skipped with a warn-level log line. The `Run` method drives a refresh ticker; the first snapshot is delivered synchronously so a missing API at startup is fatal.
- `internal/config`: new `[[upstream_pool]]` top-level array. Fields: `provider` ("mullvad" only), `id_prefix`, `priority` (default 200), `countries` (required, non-empty), `include_inactive` (default false), `refresh` (default 12h), `cache_path`. The "at least one upstream defined" rule now accepts either `[[upstream]]` or `[[upstream_pool]]`. Pool priority and refresh use the same `*int` / `*Duration` sentinel pattern as `UpstreamConfig.Priority` so explicit zeros reach `Validate`.
- `cmd/tunnelsmith` expands every `[[upstream_pool]]` block at startup before the priority pool is built. Failures during expansion are fatal so a misconfigured pool surfaces immediately. Phase 7 will move runtime refresh into the SIGHUP hot-reload path.
- `deploy/docker-compose.mullvad.yml`: gluetun (qmcgaw/gluetun:v3.41.1) in WireGuard mode + tunnelsmith joining via `network_mode: "service:gluetun"`. Per ADR-003 the integration uses WireGuard, not OpenVPN.
- `deploy/tunnelsmith.mullvad.toml`: reference runtime config showing a single `[[upstream_pool]]` block.
- `deploy/.env.example`: documents `MULLVAD_WIREGUARD_PRIVATE_KEY` and `MULLVAD_WIREGUARD_ADDRESSES`, including how to generate the keypair, the 5-device cap, and the no-resale ToS note. Real values never live in the file.
- `scripts/verify-mullvad.sh`: writes a single-country tunnelsmith config, restarts the tunnelsmith container, curls `am.i.mullvad.net/json` through the proxy, and asserts `mullvad_exit_ip == true` plus `country == expected`. Defaults to Sweden / Netherlands / USA; configurable via `VERIFY_COUNTRIES`.
- `Makefile` `test-integration` target plus a CI `mullvad-integration` job. Both gate on `MULLVAD_WIREGUARD_PRIVATE_KEY` and `MULLVAD_WIREGUARD_ADDRESSES` being set; without either, the integration step logs a `[skip-ok]` reason and exits 0.
- `docs/deployment.md`: Mullvad WG keypair setup, the 5-device cap, the no-resale ToS clause, end-to-end smoke test, and a troubleshooting checklist.
- `docs/request-lifecycle.md`: end-to-end trace of a single request through Tunnelsmith, including 429 handling, cascade behavior, and the HTTPS opacity caveat.
- `docs/integration-guide.md`: Levels 1-7 for container maintainers who want to ship Tunnelsmith support, plus the common-mistakes and verification checklists.
- `docs/configuration.md`: new `[[upstream_pool]]` table; expanded `[[upstream]]` validation note that pool entries also count toward the "must have at least one upstream" rule.
- README: "Use with Mullvad" callout linking deployment.md, "For container maintainers" callout linking integration-guide.md, plus links to the new request-lifecycle and integration-guide docs.
- ADR-003: Mullvad integration uses WireGuard, not OpenVPN. ADR-004: SOCKS5 hostname pattern is per-server multihop, with the transformation rule documented and the original plan's pattern explicitly superseded.

### Added (Phase 5)

- `internal/failure`: `ParseRetryAfter` covers RFC 7231 §7.1.3 in both shapes (non-negative integer seconds and any of the three HTTP-date forms `net/http.ParseTime` accepts), and `StatusDetector` maps configured `[[failure.status]]` rules to a `failure.Kind` plus an optional honored `Retry-After` duration. Codes outside the supported set (429 / 403 / 451) are skipped silently because no `failure.Kind` exists for them yet.
- `internal/listener` (HTTP plain-HTTP forward path): the forward path now drives its own pick-dial-detect retry loop on top of the scoreboard. 429, 403, and 451 responses are recorded against the (host, upstream) entry with the right kind, the response is drained, and the request rotates to the next-best upstream within the same retry budget. 5xx and other 4xx responses are forwarded to the client without being treated as upstream failures (no penalty, no rotation). Every response that reaches the client on the success path - 2xx, 3xx, 4xx-other, 5xx alike - carries `X-Tunnelsmith-Upstream` and `X-Tunnelsmith-Retries` so operators can see which upstream served and how many retries the request consumed; the cascade-failure 502 carries `X-Tunnelsmith-Cascade` and `X-Tunnelsmith-Retries`.
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
