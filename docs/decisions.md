# Architecture Decision Records

A running log of decisions that shape the build. Newest entries at the bottom. Each entry uses the same shape: Context, Decision, Consequences. If a later decision supersedes an earlier one, link the two.

---

## ADR-001: Docker images are built in CI, not on developer hosts

**Date:** 2026-05-08
**Status:** Accepted

### Context

The original build plan in `_planning/tunnelsmith-build-plan.md` required `make docker` to succeed at every phase gate, plus an explicit `docker run --rm tunnelsmith:dev` step to confirm the image prints its version. That implies a Docker daemon on every developer machine and adds a local feedback loop that is slower than just running the binary.

Talor flagged this during Phase 0 kickoff and asked that the image be produced exclusively by a GitHub Actions workflow. Local invocations of `make docker` and `docker run` against the project image are not part of any phase gate.

### Decision

1. The `make docker` target stays in the `Makefile` so CI has a single command to call. Developers do not run it.
2. Phase gates that previously required `make docker` now require the corresponding CI job (`docker` job in `.github/workflows/ci.yml`) to be green.
3. Manual verification steps that previously required `docker compose up` move into CI integration steps. Where the verification is something a developer needs to do interactively (running the binary against a test fixture, sending SIGHUP, opening a browser), they run the binary directly via `./bin/tunnelsmith`.
4. The `docker info` pre-flight check is `[skip-ok]` if the daemon is not running locally.
5. The build plan was edited in place to reflect this. The plan lives in `_planning/` and is gitignored, so the edit does not appear in commit history. This ADR is the durable record of the change.

### Consequences

- Faster local iteration. No daemon needed for everyday work.
- Image-related regressions are caught only in CI rather than at the developer's terminal. The trade-off is acceptable because the binary itself can be exercised locally without the image, and image-shaped issues (missing files, wrong base, ENTRYPOINT mistakes) tend to be rare and visible the first time CI runs after a relevant change.
- Release verification for v1.0.0 (Phase 10) still pulls from GHCR, which technically requires a local daemon. That is a release sanity check, not a build-time loop, and is fine to revisit when Phase 10 starts.

---

## ADR-002: Bump Go directive to 1.23 to absorb golang.org/x/net security fixes

**Date:** 2026-05-08
**Status:** Accepted

### Context

GitHub Dependabot opened two moderate alerts against `main` for `golang.org/x/net v0.35.0`:

- GHSA-qxp5-gwg8-xv66: HTTP proxy bypass via IPv6 Zone IDs (fixed in v0.36.0). Directly relevant: Tunnelsmith is an HTTP and SOCKS5 proxy.
- GHSA-vvgc-356p-c3xw: cross-site scripting in `html` package (fixed in v0.38.0). Less directly relevant but the cleanest fix is to land both in one bump.

The Phase 1 changelog pinned `golang.org/x/net v0.35.0` because v0.35.0 was the last release whose `go.mod` declared `go 1.18`, which kept the project's own `go 1.22` directive viable. Every release at v0.36.0 or later requires `go 1.23.0`, so picking up the security fixes forces the project's `go` directive up to 1.23.

### Decision

1. Bump `golang.org/x/net` from v0.35.0 to v0.38.0 (the lowest version that patches both alerts).
2. Bump the project's `go` directive from `1.22` to `1.23.0` (the value `go mod tidy` settles on after the dependency bump).
3. Bump `GO_VERSION` from `1.22` to `1.23` in `.github/workflows/ci.yml` and in the `Dockerfile` `ARG`.

### Consequences

- The host Go on contributor machines must be `>= 1.23`. Most modern installs are already there; CI is the only environment that we control.
- Future bumps of `golang.org/x/net` should not require another Go bump in the short term. If a later vuln forces another, the trade-off (security patch vs. Go floor) stays the same and we follow this same path.
- Supersedes the Phase 1 rationale that pinned x/net at v0.35.0 to keep `go 1.22`. That pin is no longer load-bearing.
