# Configuration

Tunnelsmith reads a TOML config from `--config` (default `/etc/tunnelsmith/config.toml`). A complete commented example lives at [`examples/tunnelsmith.toml`](../examples/tunnelsmith.toml).

This page lists every key, its type, default, and what it does. The defaults match the constants in `internal/config/config.go`.

## Loading and validation

- `tunnelsmith --config <path>` loads the file at `<path>`. Anything wrong with the file produces a non-zero exit and a clear error. Missing files, unparsable TOML, and validation failures all surface the file path.
- `tunnelsmith --config <path> --print-config` loads, applies defaults, and prints the resolved config as TOML. Useful for confirming what the running binary actually saw.
- Unknown keys (typos, fields that belong to a future schema version) are not fatal. They are logged at `WARN` so you can spot mistakes without breaking forward-compatibility for new fields added in later phases.

## Logging

`TUNNELSMITH_LOG_LEVEL` (env) controls the slog level. One of `debug`, `info` (default), `warn`, `error`. Logs are JSON, one event per line.

## Top-level structure

```toml
[listener]
[cache]
[metrics]
[[upstream]]         # zero or more (must have at least one upstream OR upstream_pool)
[[upstream_pool]]    # zero or more (Phase 6: provider = "mullvad")
[failure]
[[failure.status]]   # zero or more
[[rule]]             # zero or more
```

## `[listener]`

Where the proxy listeners bind. Both default to all-interfaces on the standard local ports.

| key   | type   | default   | notes |
|-------|--------|-----------|-------|
| `http`  | string | `:8080` | host:port for the HTTP CONNECT and forward proxy |
| `socks` | string | `:1080` | host:port for the SOCKS5 listener |

Both must parse via `net.SplitHostPort` and the port must be in `1-65535`.

## `[cache]`

The decision-cache and persistence settings.

| key                | type     | default | notes |
|--------------------|----------|---------|-------|
| `ttl`              | duration | `15m`   | how long a successful (host, upstream) decision stays cached |
| `negative_ttl`     | duration | `1m`    | cascade-failure cooldown for a host where every upstream just failed |
| `persist_path`     | string   | `""`    | absolute file path for scoreboard persistence; empty means in-memory only |
| `persist_interval` | duration | `30s`   | how often the persistence loop snapshots state to `persist_path`; `0s` disables periodic writes (the shutdown flush still runs when `persist_path` is set) |

Durations use Go's `time.ParseDuration` syntax (`120s`, `15m`, `6h`, etc.).

## `[metrics]`

Prometheus exposition surface.

| key    | type   | default   | notes |
|--------|--------|-----------|-------|
| `bind` | string | `:9090`   | host:port for `/metrics` (and `/healthz`); empty disables the listener entirely |

When set, `bind` must parse via `net.SplitHostPort` with a port in `1-65535`. The endpoint is unauthenticated; bind it to an internal interface only. See [observability.md](observability.md) for the metric reference and Grafana dashboard.

## `[ui]`

Web UI. Renders the scoreboard, the upstream pool, and the active force pins, plus four JSON action endpoints for managing state by hand.

| key    | type   | default | notes |
|--------|--------|---------|-------|
| `bind` | string | `:9091` | host:port for the UI HTTP listener; empty disables it entirely |

When set, `bind` must parse via `net.SplitHostPort` with a port in `1-65535`. The UI port is **unauthenticated**: there is no login, no token, no rate limit. The four mutating endpoints (`/api/forget`, `/api/force`, `/api/force/clear`, `/api/reset`) accept any caller that can reach the port. Bind it to a loopback address (`127.0.0.1:9091`) or to a private subnet that only your trusted clients can reach. See [ui.md](ui.md) for the endpoint reference and the security stance.

## `[control]`

Phase 11 cooperative-reporting endpoint. Apps that integrate via [`docs/cooperative-reporting.md`](cooperative-reporting.md) POST per-request outcomes here so Tunnelsmith's scoreboard learns from HTTPS traffic the proxy cannot inspect on its own.

| key    | type   | default | notes |
|--------|--------|---------|-------|
| `bind` | string | `:9092` | host:port for the control HTTP listener; empty disables it entirely |

When set, `bind` must parse via `net.SplitHostPort` with a port in `1-65535`. Like `[ui]`, the control port is **unauthenticated**: there is no login, no token, no rate limit. `POST /v1/report` mutates scoreboard state every time it is called. Bind it to a loopback address or to the private subnet your trusted apps reach Tunnelsmith over. See [`docs/cooperative-reporting.md`](cooperative-reporting.md) for the wire protocol, outcome vocabulary, and the trust boundary.

`control.bind` is **restart-only**. SIGHUP hot-reload does not move the listener; the same constraint applies as `[metrics]` and `[ui]`. Restart the binary to change the address.

## `[[upstream]]`

The config must define at least one upstream, either directly via `[[upstream]]` or by expansion via `[[upstream_pool]]`. Lower priority wins ties; scores from the Phase 4 scoreboard dominate priority once they exist. Every upstream needs a unique `id`.

| key        | type        | default | notes |
|------------|-------------|---------|-------|
| `id`       | string      | none    | required, unique within the file |
| `kind`     | string enum | none    | one of `direct`, `http`, `socks5` |
| `addr`     | string      | none    | required for `http` / `socks5` (`host:port`); must be empty for `direct` |
| `priority` | int         | `100`   | tiebreaker only; lower wins |

Validation:
- IDs must be unique across the file (including those produced by `[[upstream_pool]]` expansion).
- `kind = "direct"` must not set `addr`.
- `kind = "http"` and `kind = "socks5"` require `addr` to parse as `host:port` with a non-empty host and a port in `1-65535`.

## `[[upstream_pool]]`

`[[upstream_pool]]` expands at startup into one or more synthetic `[[upstream]]` entries. Phase 6 only knows how to expand `provider = "mullvad"`, which fans the configured countries out into one socks5 upstream per active Mullvad WireGuard relay. The hostname transformation is documented in [ADR-004](decisions.md#adr-004-mullvad-socks5-hostname-pattern-is-per-server-multihop-not-the-form-in-the-original-plan); operators only name countries.

| key                | type             | default | notes |
|--------------------|------------------|---------|-------|
| `provider`         | string enum      | none    | required; only `"mullvad"` is implemented |
| `id_prefix`        | string           | none    | required; prepended to every generated upstream id (e.g. `mvd` -> `mvd-se-sto-wg-001`); must be unique across pool blocks |
| `priority`         | int              | `200`   | applied to every expanded upstream; default puts pool entries below user-defined `[[upstream]]` (default 100) |
| `countries`        | array of strings | none    | required, non-empty; case-insensitive match against the Mullvad relay-list `country` field (`Sweden`, `USA`, ...) |
| `include_inactive` | bool             | `false` | `true` admits relays Mullvad has flagged inactive (rare; mostly for debugging) |
| `refresh`          | duration         | `12h`   | background relay-list refresh interval |
| `cache_path`       | string           | `""`    | absolute path for a disk fallback used when the relay-list API is unreachable |

Example:

```toml
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
priority  = 100
countries = ["Sweden", "Netherlands", "Switzerland"]
```

A config that uses only `[[upstream_pool]]` and no `[[upstream]]` is valid; a config with neither is rejected.

The pool expansion runs at startup to seed the priority pool, and a per-block refresh goroutine keeps polling the relay list at the configured interval. In v1.0.0 the refresh tick logs the diff (added / removed upstream ids) so operators can see relay churn; the live running pool is whatever startup produced. To pick up a refreshed pool, restart the binary. SIGHUP re-reads the config file, but pools backed by `[[upstream_pool]]` are deliberately frozen across SIGHUP because re-fetching the relay list under signal would block the reload on Mullvad's API. `refresh = "0s"` is allowed and disables periodic refresh entirely; any positive value below 1m is rejected so a typo cannot hammer Mullvad's public relay-list API.

## `[failure]`

Failure-detection settings. The signals feed the scoreboard's penalty / cooldown logic.

| key                          | type             | default       | notes |
|------------------------------|------------------|---------------|-------|
| `connection_refused`         | bool             | `true`        | parsed and accepted; the runtime always classifies dial-side `ECONNREFUSED` as `KindRefused` and applies the configured policy regardless of this value. The opt-out gate is tracked in [#20](https://github.com/pacnpal/tunnelsmith/issues/20); set the field if you want forward compatibility, but do not rely on `false` to skip refused-connection scoring in v1.0.0 |
| `timeout_ms`                 | int (ms)         | `8000`        | per-attempt timeout; must be > 0 |
| `body_regex`                 | array of strings | `[]`          | deprecated; the parser still accepts the field but the runtime ignores it. Move patterns into `[[rule]].body_regex` instead. A non-empty value at startup triggers a one-line warning |
| `body_buffer_kb`             | int              | `32`          | per-response cap (in KiB) on the body prefix the listener buffers for `[[rule]].body_regex` matching. Must be >= 0 and <= 1024. Set to `0` to disable body inspection regardless of any `[[rule]].body_regex` entries; SIGHUP applies the new value live |
| `max_retries_per_request`    | int              | `5`           | retry cap per incoming request; must be >= 1 |
| `status` (`[[failure.status]]`) | array of tables | see below | per-status-code rules |

### `[[failure.status]]`

If the user provides any `[[failure.status]]` entries, they are taken as the complete list. If omitted, Tunnelsmith fills in the proposal's three default rules:

| code | penalty | cooldown | honor_retry_after |
|------|---------|----------|---------------------|
| 429  | 4       | `120s`   | true |
| 403  | 6       | `30m`    | false |
| 451  | 8       | `6h`     | false |

Rule fields:

| key                  | type     | default | notes |
|----------------------|----------|---------|-------|
| `code`               | int      | none    | required; in the `100-599` HTTP status range |
| `penalty`            | int      | `0`     | how much score to subtract from the upstream for this `(host, upstream)` |
| `cooldown`           | duration | `0`     | how long to cool the upstream down for this host after this code |
| `honor_retry_after`  | bool     | `false` | when true, the response's `Retry-After` header overrides `cooldown` |

5xx, 4xx-other, and 2xx codes are deliberately not in the default list: 5xx is usually a transient destination problem rather than an upstream issue, generic 4xx is request-shaped, and 2xx is success unless a body-regex says otherwise.

### `[failure.scoring]`

Knobs for the per-(host, upstream) scoreboard introduced in Phase 4. Sensible defaults match the proposal; override individual keys without restating the rest of the section.

| key                | type     | default | notes |
|--------------------|----------|---------|-------|
| `refused_penalty`  | float    | `3`     | score subtracted on `KindRefused` |
| `refused_cooldown` | duration | `30s`   | how long the (host, upstream) pair sits out after a refused dial |
| `timeout_penalty`  | float    | `2`     | score subtracted on `KindTimeout` |
| `timeout_cooldown` | duration | `15s`   | how long the (host, upstream) pair sits out after a timed-out dial |
| `success_weight`   | float    | `1`     | score added on each success; must be > 0 |
| `score_cap`        | float    | `10`    | absolute cap on `\|score\|` so a long-running winner cannot accumulate so much that one bad minute cannot dethrone it; must be > 0 |
| `probe_chance`     | float    | `0.05`  | per-Pick probability of picking a non-top eligible candidate so a penalized upstream gets a chance to recover; must be in `[0,1]` |
| `decay_interval`   | duration | `5m`    | how often the decay goroutine ticks; must be > 0 |
| `decay_step`       | float    | `0.5`   | absolute amount each entry's score moves toward zero per tick |
| `cascade_ttl`      | duration | `30s`   | negative TTL for a host where every upstream just failed; subsequent requests within the TTL get an immediate cascade error without burning through the pool |
| `debounce_window`  | duration | `100ms` | identical `(host, upstream, kind)` failures arriving within this window collapse into one penalty event |
| `body_match_penalty`  | float   | `5`     | score subtracted on `KindBodyMatch`. Phase 8 fires this when a `[[rule]].body_regex` matches the buffered response prefix |
| `body_match_cooldown` | duration | `60s`  | cooldown applied to the `(host, upstream)` after a body-regex match |
| `prune_after`      | duration | `24h`   | the persistence-tick prune pass drops entries with `score == 0` and `lastSeen` older than this; `0s` disables entry pruning (cascade and debounce eviction still run) |

The rate-limit, forbidden, and legal-block kinds get their penalty and cooldown from `[[failure.status]]` entries. The plain-HTTP listener records the matching kind and rotates to the next upstream within the same request, up to `failure.max_retries_per_request`. Body-match scoring uses `body_match_penalty` and `body_match_cooldown` and is driven by `[[rule]].body_regex`.

## `[[rule]]`

Optional per-host overrides. The routing semantics and the body-regex
inspector run live through the request path.

| key         | type             | default | notes |
|-------------|------------------|---------|-------|
| `host_glob` | string           | none    | required; `path.Match` semantics over the lowercased host. `*.bbc.co.uk` matches `news.bbc.co.uk` and `a.b.bbc.co.uk` (`*` is multi-segment because path.Match's separator is `/`, which never appears in a host). Match is case-insensitive |
| `prefer`    | array of strings | none    | required; references upstream IDs in declaration order |
| `force`     | bool             | `false` | when true, the router never falls back to upstreams outside `prefer`. Forced cascade fires when every preferred upstream has been tried |
| `body_regex` | array of strings | `[]`   | response-body patterns that flag the response as a soft block when matched. Each pattern is compiled as a Go regexp at startup; an invalid pattern fails the load |

Every id in `prefer` must match an `id` from a defined `[[upstream]]`. When
the running config also has an `[[upstream_pool]]` block, prefer ids are
checked at startup against the merged set (after pool expansion). The
SIGHUP reloader re-runs the same check before swapping rule state in.

Rules are evaluated in declaration order. The first rule whose
`host_glob` matches the request host wins; subsequent rules are ignored
for that host. Place specific globs (`*.news.bbc.co.uk`) above broader
ones (`*.bbc.co.uk`) when you want a tighter override.

### Body-regex semantics

Body inspection runs only on the plain-HTTP forward-proxy path. CONNECT
and SOCKS5 traffic carries TLS payloads the proxy cannot decrypt; both
paths skip inspection regardless of any matching rule. The listener's
behavior on a plain-HTTP response when the matched rule has compiled
patterns:

1. Read the response body up to `failure.body_buffer_kb` KiB. Bodies
   larger than the cap are inspected on the prefix only; bytes past the
   cap are streamed to the client without ever being matched.
2. If `Content-Encoding` is set to anything other than empty or
   `identity` (gzip, br, deflate, ...), inspection is skipped: the
   regex would run against compressed bytes that mean nothing in their
   raw form. The body still reaches the client unchanged.
3. If any pattern matches, the listener treats the response as a
   `KindBodyMatch` failure: the upstream is penalized per
   `failure.scoring.body_match_*`, the response body is drained, the
   upstream id is added to `tried`, and the request retries through
   the next-best upstream.
4. If no pattern matches (or inspection was skipped), the listener
   stitches the buffered prefix back in front of the rest of the body
   so the client sees a byte-for-byte copy of the upstream response.

A failed body read mid-inspection (TCP reset, etc.) is logged but does
not record a per-kind failure: the upstream may still be healthy and
the request rotates to the next upstream without an explicit penalty.

## CONNECT and SOCKS5: what status detection cannot see

Status-code detection only runs on plain-HTTP forward-proxy requests. CONNECT tunnels are end-to-end TLS by design, so Tunnelsmith sees ciphertext after the 200 Connection Established line and cannot read status codes inside the tunnel. SOCKS5 is a byte stream from the protocol's first frame; nothing on that path is HTTP-shaped from Tunnelsmith's perspective.

The same caveat applies to header injection, with the precise rule:

- Plain-HTTP responses that an upstream served (any status the detector did not flag as a failure: 2xx, 3xx, 4xx-other, 5xx) carry `X-Tunnelsmith-Upstream` (which upstream served the request) and `X-Tunnelsmith-Retries` (count of failed attempts before success).
- Cascade-failure 502s (the listener exhausted every upstream for the request) carry `X-Tunnelsmith-Cascade` with the host plus `X-Tunnelsmith-Retries`. They do not carry `X-Tunnelsmith-Upstream`: no upstream served, so there is nothing to attribute.
- Listener-generated errors that do not even reach the dial loop (e.g. the 400 for a non-absolute or non-http(s) URL) carry no Tunnelsmith headers at all.
- CONNECT and SOCKS5 paths inject nothing, because Tunnelsmith does not own the response framing on those paths.

Practical consequence: a client that issues an HTTPS request through Tunnelsmith via CONNECT goes through the dial-loop part of the scoreboard (refused, timeout) but not the status or body parts. Phase 8's `[[rule]].body_regex` shares the same constraint: body inspection only runs on plain-HTTP forward-proxy responses, and only when `Content-Encoding` is empty or `identity` (the inspector skips gzip/br/deflate bodies because matching against compressed bytes would never produce a useful signal).

## Hot-reload

Sending `SIGHUP` to the running Tunnelsmith process re-reads the config file from the same path it was loaded from. The reload is best-effort: if the new config fails to parse or validate, the binary logs a warning and keeps running on the old config.

What hot-reload changes in place:

- `[[upstream]]` list (rebuilds the priority pool, swaps it into the scoreboard, drops cached transports for upstreams that disappeared)
- `[failure]` retry cap, `[[failure.status]]` rules, and `body_buffer_kb`
- `[failure.scoring]` penalty weights, cooldowns, probe chance, cascade TTL, debounce window, body-match knobs, prune-after
- `[[rule]]` block list (compiled at reload time and installed on both the scoreboard and the listener; a malformed pattern or unknown prefer id leaves the previous rule set live). The two installs run sequentially under each component's own write lock, so a request mid-reload may briefly see the new rules in one component and the old in the other; the request still completes against a coherent snapshot of whichever rule set each component holds

What hot-reload does NOT change (restart required):

- `[listener]` bindings
- `metrics.bind`
- `ui.bind`
- `cache.persist_path` and `cache.persist_interval`
- `failure.scoring.decay_interval`
- `[[upstream_pool]]` (Mullvad refresh schedule)

The reload outcome is reported on `tunnelsmith_config_reloads_total{result="success" | "error"}`.

## See also

- [Architecture](architecture.md): how the scoreboard uses these settings.
- [Observability](observability.md): the metric surface, scrape config, hot-reload behavior, and Grafana dashboard import.
- [Decisions](decisions.md): the running ADR log.
