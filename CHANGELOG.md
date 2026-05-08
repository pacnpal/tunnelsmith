# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
