# Architecture

The piece that makes Tunnelsmith different from HAProxy or Squid is the per-(host, upstream) scoreboard. This page explains what it tracks, how Pick decides, and why each behavior is there.

## The high-level dial path

A request enters one of the listeners (HTTP CONNECT, plain HTTP forward proxy, or SOCKS5). The listener hands the destination address to the scoreboard's `DialFor`. The scoreboard:

1. Checks whether the host is in cascade cooling. If yes, returns the cascade error immediately and the listener responds 502 (or, for SOCKS5, surfaces the error to the library).
2. Picks the best non-cooled upstream for the host.
3. Dials through it.
4. On success, records the success and returns the conn.
5. On failure, records the failure (penalty + cooldown), advances to the next-best upstream, and tries again.
6. Caps retries at `failure.max_retries_per_request`. If every retry fails, trips cascade for the host and surfaces an aggregated error.

Steps 2-5 form the inner loop. Each call to Pick produces one upstream candidate; the loop drives the whole "try, observe, advance" sequence.

## What the scoreboard tracks

For every `(host, upstream)` pair the scoreboard has touched, it stores:

- **score**: a float that climbs by `success_weight` on each success (capped at `score_cap`) and drops by the kind-specific penalty on each failure (clamped at `-score_cap`).
- **cooldown_until**: a timestamp. While in the future, this upstream is skipped for this host even if it has the best score.
- **last_seen**: the most recent attempt time. Used by the decay loop and operational tooling.
- **global_success_count / global_failure_count**: lifetime counters used by metrics and (in a later phase) global degradation detection.

Entries are created lazily on first touch. A host that has never been served has no scoreboard state for any upstream, so the first request through that host falls back to the pool's static priority order.

## Pick

```
candidates = pool entries minus tried IDs
if cascade[host] active: return ErrCascadeCooling
if candidates empty: return ErrPoolExhausted

eligible = candidates with cooldown_until <= now
cooled   = candidates with cooldown_until > now

if eligible empty:
    return cooled candidate with the soonest expiry  (cooldown is advisory
                                                      when nothing else is
                                                      available)

sort eligible by (score desc, base_priority asc)

if len(eligible) > 1 and rand() < probe_chance:
    return uniformly-random non-top eligible candidate

return eligible[0]
```

Three things to notice:

- **Score wins over priority**, but priority is the tiebreaker for fresh entries (no recorded data) and for entries with identical scores. So a config with `priority = 10, 20, 30` behaves like a static priority list until real outcomes shift the scores.
- **Cooldown is advisory when nothing else is available.** If every untried upstream is on cooldown, Pick returns the one whose cooldown expires soonest rather than 502'ing. The alternative is a stampede of 502s for hosts where every upstream just got penalized.
- **Probe** is the answer to "the cached winner stays cached forever even after the loser has recovered". With probability `probe_chance`, Pick deliberately returns a non-top candidate so a previously-penalized upstream has a chance to climb back up via natural success bookkeeping.

## RecordSuccess and RecordFailure

`RecordSuccess(host, upstream_id, latency)` adds `success_weight` to score (clamped at `score_cap`), clears `cooldown_until`, sets `last_seen`, increments `global_success_count`, and clears the host's cascade entry if any. A single success is enough to take a host out of cascade.

`RecordFailure(host, upstream_id, kind, cooldown_override)` looks up the kind's policy (penalty + cooldown), subtracts penalty from score (clamped at `-score_cap`), bumps `cooldown_until` to `now + cooldown`, sets `last_seen`, and increments `global_failure_count`. `cooldown_override` is `*time.Duration`: `nil` means use the kind's default cooldown, non-nil overrides it verbatim including a literal zero. The pointer shape exists so the listener can honor `Retry-After: 0` (a legal RFC 7231 §7.1.3 value meaning "retry immediately") without it being indistinguishable from "header absent".

Phase 4 fires only `KindRefused` and `KindTimeout` from the dial path with a nil override. Phase 5 fires `KindRateLimit`, `KindForbidden`, and `KindLegalBlock` from the plain-HTTP listener: when the response status matches a configured `[[failure.status]]` rule, the listener calls `RecordFailure` with the matching kind plus the detector's `CooldownOverride` (non-nil when the rule honors `Retry-After` and the header parsed) and rotates to the next upstream within the same retry budget. `KindBodyMatch` stays unwired until Phase 8.

## Failure debounce

Identical `(host, upstream, kind)` failure events arriving within `debounce_window` (default 100ms) collapse into one penalty event. The motivation: 10 concurrent client requests all see the same 429 from one upstream and call RecordFailure within milliseconds of each other. Without debounce, that single rate-limit event becomes a `-40` score swing instead of `-4`. With debounce, only the first call lands a penalty; subsequent calls within the window are no-ops.

The `global_failure_count` increments along with the penalty, not per observation, so the counter stays consistent with what the score actually changed by.

## Cascade cooling

When `DialFor` exhausts retries and every upstream failed for this host, the scoreboard sets `cascade[host] = now + cascade_ttl`. Subsequent calls within the TTL return immediately with `ErrCascadeCooling` and never touch the upstream pool. This is the proposal's "do not amplify a real outage into a stampede".

Cascade clears two ways:

- Natural expiry: the next request after `now + cascade_ttl` walks the pool fresh.
- Single success: a successful `RecordSuccess` for the host clears the entry early.

This means a stuck host never permanently 502s, even if `cascade_ttl` is long: the first dial after expiry gets a real attempt, and a single working upstream pulls the host out.

## Time decay

A goroutine wakes every `decay_interval` (default 5m) and drifts every entry's score toward zero by `decay_step` (default 0.5). The math is the obvious one: positive scores subtract step (clamped at zero), negative scores add step (clamped at zero), zero is fixed.

Why drift: a host with a long stable winner accumulates score on that winner over hours or days. If the winner stops working, even a `-3` per refusal takes a while to dethrone a `+10` cap. Decay re-explores after long stable periods so the scoreboard is responsive to actual current conditions, not a frozen view from yesterday.

`decay_step = 0.5` and `decay_interval = 5m` means a `+10` score takes 100 minutes to reach zero in the absence of new successes. The default success path (1 success bumps score by 1, capped at 10) replenishes faster than decay drains under steady traffic, so a healthy winner stays healthy.

## Random source

The probe roll uses a `*rand.Rand` instance owned by the scoreboard. Tests pass a seeded rand via `WithRand` so probe outcomes are deterministic; the production binary uses an unseeded one (`rand.NewSource(time.Now().UnixNano())`). Access is serialized through a mutex because `*rand.Rand` is not goroutine-safe.

## Locking

A single `sync.RWMutex` guards the entries map and the cascade map. Pick takes the read lock; `RecordSuccess`, `RecordFailure`, and the decay tick take the write lock. Lock pressure is one acquire per request (Pick) plus one per outcome (Record*), so on a busy proxy that is two acquires per request. For v1 homelab use this is fine; Phase 7 metrics work may revisit if profiling shows contention.

The debounce map and the random source each have their own dedicated mutex so they do not block the main read path.

## Where status detection lives

Status detection is on the listener side, not the scoreboard side. `internal/failure.StatusDetector` reads `[[failure.status]]` once at boot and holds a `code -> Kind` map for the supported codes (429, 403, 451). The plain-HTTP forward path drives a pick-dial-detect loop:

1. Pick an upstream. On cascade or pool exhaustion, fall through to a 502 with `X-Tunnelsmith-Cascade`.
2. RoundTrip through the upstream's pinned `http.Transport`.
3. If the RoundTrip returned an error, narrow it: only `failure.IsTimeout` and `failure.IsConnectionRefused` matches translate into a `RecordFailure` (with `KindTimeout` or `KindRefused`). Other RoundTrip errors (TLS verification, HTTP parse, server hangup mid-response, ...) might belong to the destination rather than the upstream, so the listener marks the upstream tried and rotates without penalizing it. The scoreboard's own `DialFor` (used by CONNECT / SOCKS5) operates one layer down, where every error is a real dial-level error, and there it still uses `failure.ClassifyDialError`'s "unknown defaults to refused" mapping.
4. If a response came back, ask the detector. On a positive match, drain the body, `RecordFailure` with the matched Kind plus any honored `Retry-After`, mark tried, retry.
5. On a non-match (the common path), `RecordSuccess` and write the response back to the client with `X-Tunnelsmith-Upstream` and `X-Tunnelsmith-Retries`.

`failure.max_retries_per_request` caps total attempts per request, so dial failures and status failures share the budget. Each retried response drain is bounded two ways: `io.LimitReader` caps bytes (64 KiB), and `drainAndClose` caps wall time (250 ms) before closing the body, so a stalled upstream cannot block retries indefinitely.

CONNECT and SOCKS5 paths skip steps 4 and 5 entirely - the listener cannot inspect a TLS tunnel or a SOCKS byte stream. They use `Scoreboard.DialFor` directly, which runs the dial-only loop.

## What is not here yet

- Body-regex detection (Phase 8).
- Per-host rules (`[[rule]]`) for `force` and `prefer` overrides (Phase 8).
- Scoreboard persistence to disk (Phase 7).
- Prometheus metrics (Phase 7; `Snapshot` is the data source).
- Web UI (Phase 9).
- Global degradation detection ("upstream X is failing for every host, mark it globally degraded"). The proposal lists this as a refinement; it is not in the Phase 4 plan.
