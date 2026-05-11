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

Phase 4 fires only `KindRefused` and `KindTimeout` from the dial path with a nil override. Phase 5 fires `KindRateLimit`, `KindForbidden`, and `KindLegalBlock` from the plain-HTTP listener: when the response status matches a configured `[[failure.status]]` rule, the listener calls `RecordFailure` with the matching kind plus the detector's `CooldownOverride` (non-nil when the rule honors `Retry-After` and the header parsed) and rotates to the next upstream within the same retry budget. Phase 8 fires `KindBodyMatch` from the same listener: when a `[[rule]]` matches the host and one of its compiled `body_regex` patterns matches the buffered response prefix, the listener records `KindBodyMatch` and rotates the request to the next upstream.

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

A single `sync.RWMutex` guards the entries map and the cascade map. Pick takes the read lock; `RecordSuccess`, `RecordFailure`, and the decay tick take the write lock. Lock pressure is one acquire per request (Pick) plus one per outcome (Record*), so on a busy proxy that is two acquires per request. The Phase 7 `BenchmarkScoreboardWriterContention` measured ~3.3 microseconds per op at homelab scale (1k hosts, 20 upstreams) under concurrent Pick + RecordSuccess writers; v1 is fine without further mitigation.

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

## Per-host rules

A `[[rule]]` block applies a host-glob filter on top of the score-based pick. Rules compile into a `RuleSet` at startup; the scoreboard looks the rule up on every Pick. Two flags shape the routing:

- `force = true` narrows the candidate set to ids in `prefer` before scoring runs. Cooldown-fallback also stays inside the prefer set, so a forced rule whose preferred upstreams are all in tried produces `ErrPoolExhausted` (and a forced cascade) rather than touching unrelated upstreams.
- `force = false` (default) keeps the full candidate set but adds a `preferRank` tiebreak that wins over score and base priority. Preferred upstreams sort to the top in declaration order; non-preferred candidates fall in behind by the existing `(score desc, base_priority asc)` rule.

Body-regex inspection is separately gated by `failure.body_buffer_kb` and runs only on plain-HTTP responses (CONNECT and SOCKS5 traffic carries TLS the proxy cannot read). The listener feeds compiled patterns through `internal/failure.BufferAndDecide`, which reads up to the configured cap, runs each pattern, and returns either a match decision or a replay reader that streams the buffered prefix plus rest to the client. Encoded bodies (`Content-Encoding != identity`) skip inspection.

The RuleSet is hot-reloadable: `Scoreboard.ReplaceRules` and `HTTPServer.ReloadRules` install the same compiled set sequentially, each under its own component's write lock. Both calls take their write lock for the swap, so a request reading either component's pointer sees one self-consistent rule set, never a half-written state. Across components the swap is sequential, not atomic: a request mid-reload may briefly observe the new rules in one component and the old in the other (for example, the listener could route through the old prefer set while inspecting bodies with the new patterns). The window is short and bounded; routing decisions converge on the next request once both swaps complete.

## Cooperative outcome reporting (Phase 11)

Body-regex inspection only sees plain HTTP. CONNECT and SOCKS5 carry TLS opaquely, so the listener has no signal beyond dial-level failure for HTTPS. Phase 11 closes that gap with an out-of-band channel: the proxy advertises which exit served a request via `X-Tunnelsmith-Upstream` (already on plain-HTTP responses; now also on the CONNECT 200 response), and apps that opt in POST per-request outcomes back to a small control listener (`internal/control`, default `:9092`).

The control endpoint maps a closed outcome vocabulary (`ok`, `rate_limited`, `forbidden`, `legal_block`, `geo_block`, `timeout`, `refused`) to existing `failure.Kind` values and calls `Scoreboard.RecordSuccess` or `Scoreboard.RecordFailure`. No new kinds are introduced; the scoreboard treats app-reported failures identically to listener-detected ones. The full wire protocol is in [`docs/cooperative-reporting.md`](cooperative-reporting.md). The rationale for choosing this over MITM TLS interception is in [`ADR-006`](decisions.md).

## Hot-swap of the upstream pool (Phase 11.1)

`[[upstream_pool]]` blocks expand at startup into one synthetic upstream per Mullvad relay matching the country filter. v1.0.0 polled the relay list on a per-block refresh ticker but only logged the diff — the running priority pool stayed at the startup snapshot, so picking up a Mullvad relay rotation needed a binary restart. v1.1.0 closes that gap: each successful refresh tick rebuilds the pool from `(static [[upstream]]) ∪ (every block's latest expansion)` and installs it atomically through `Scoreboard.ReplacePool`. Cached HTTP transports drop on swap so new `Upstream` objects use freshly-pinned `DialContext` closures; force pins to upstreams that disappeared in the new expansion are evicted automatically; per-(host, upstream) scoreboard entries for removed ids stay in the table but become unreachable through `Pick` and age out via score decay. Concurrent refreshes from multiple `[[upstream_pool]]` blocks serialize through a composer mutex so the merged view never tears. `tunnelsmith_pool_hotswap_total{result}` counts every refresh-tick swap. SIGHUP still does not touch the pool when `[[upstream_pool]]` is configured — pool-shape changes are owned end-to-end by the refresh ticker.

## Opt-in bearer-token auth on the control endpoint (Phase 12)

Phase 11 shipped the control listener with no credential check; the security boundary was the network (loopback or private subnet). v1.1.0 adds an opt-in bearer-token gate so multi-tenant or LAN-exposed deployments can gate `POST /v1/report` without breaking the no-auth wire shape. `[control].auth_tokens` (inline) and `[control].auth_tokens_file` (one token per line, `#` comments, dedup'd union) configure a token set; an empty set keeps Phase 11 behaviour byte-for-byte. When non-empty, requests must carry `Authorization: Bearer <token>`; 401 responses include `WWW-Authenticate: Bearer realm="tunnelsmith"` plus RFC 6750 §3 `Cache-Control: no-store` and `Pragma: no-cache`. The auth check runs `crypto/subtle.ConstantTimeCompare` across the live token set without short-circuiting on match, so per-request work is independent of which token matched. Each handler captures a single `TokenSnapshot` at the start of the request and runs every auth question (`Enabled`, `Allow`) off that view so a concurrent SIGHUP rotation cannot tear the decision. Two new reject reasons (`auth_missing`, `auth_failed`) join `tunnelsmith_reports_rejected_total{reason}`. The Go SDK gained `client.Options.Token` for the corresponding client side. Rationale and the threat model (plaintext tokens on the listener; TLS termination is a Phase 13 candidate) are in [`ADR-007`](decisions.md).

## What v1.1.0 deliberately does not have

- Global degradation detection ("upstream X is failing for every host, mark it globally degraded"). The proposal lists this as a refinement; tracked in [docs/roadmap.md](roadmap.md#v2-candidates) for post-v1 consideration.
- TLS on the control listener itself. Phase 12 bearer tokens travel in plaintext until a Phase 13 follow-up wires TLS termination; until then, bind to loopback or a trusted private subnet (or front the listener with a reverse proxy that terminates TLS). Also tracked in [docs/roadmap.md](roadmap.md#v2-candidates).
