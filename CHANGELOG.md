# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
