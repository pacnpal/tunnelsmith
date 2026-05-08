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
