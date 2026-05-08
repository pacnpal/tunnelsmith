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
[[upstream]]    # one or more
[failure]
[[failure.status]]   # zero or more
[[rule]]        # zero or more
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

| key            | type     | default | notes |
|----------------|----------|---------|-------|
| `ttl`          | duration | `15m`   | how long a successful (host, upstream) decision stays cached |
| `negative_ttl` | duration | `1m`    | cascade-failure cooldown for a host where every upstream just failed |
| `persist_path` | string   | `""`    | absolute file path for scoreboard persistence; empty means in-memory only |

Durations use Go's `time.ParseDuration` syntax (`120s`, `15m`, `6h`, etc.).

## `[[upstream]]`

At least one is required. Lower priority wins ties; scores from the Phase 4 scoreboard dominate priority once they exist. Every upstream needs a unique `id`.

| key        | type        | default | notes |
|------------|-------------|---------|-------|
| `id`       | string      | none    | required, unique within the file |
| `kind`     | string enum | none    | one of `direct`, `http`, `socks5` |
| `addr`     | string      | none    | required for `http` / `socks5` (`host:port`); must be empty for `direct` |
| `priority` | int         | `100`   | tiebreaker only; lower wins |

Validation:
- IDs must be unique across the file.
- `kind = "direct"` must not set `addr`.
- `kind = "http"` and `kind = "socks5"` require `addr` to parse as `host:port` with a non-empty host and a port in `1-65535`.

## `[failure]`

Failure-detection settings. The signals are wired to scoring in Phase 4 onward.

| key                          | type             | default       | notes |
|------------------------------|------------------|---------------|-------|
| `connection_refused`         | bool             | `true`        | always on for Phase 1; opt-out lands in Phase 5 |
| `timeout_ms`                 | int (ms)         | `8000`        | per-attempt timeout; must be > 0 |
| `body_regex`                 | array of strings | `[]`          | response-body patterns that count as failure (Phase 8 wires this) |
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

The rate-limit, forbidden, and legal-block kinds get their penalty and cooldown from `[[failure.status]]` entries. Phase 5 wires them through the plain-HTTP listener: when a 429, 403, or 451 lands, the listener records a failure of the matching kind and rotates to the next upstream within the same request, up to `failure.max_retries_per_request`. Body-match policy lands in Phase 8.

## `[[rule]]`

Optional per-host overrides. Phase 8 enables them in the router; Phase 1 just validates them.

| key         | type             | default | notes |
|-------------|------------------|---------|-------|
| `host_glob` | string           | none    | required; `path.Match` semantics, so `*.bbc.co.uk` matches `news.bbc.co.uk` |
| `prefer`    | array of strings | none    | required; references upstream IDs in priority order |
| `force`     | bool             | `false` | when true, never fall back to upstreams not in `prefer` |

Every id in `prefer` must match an `id` from a defined `[[upstream]]`.

## CONNECT and SOCKS5: what status detection cannot see

Status-code detection only runs on plain-HTTP forward-proxy requests. CONNECT tunnels are end-to-end TLS by design, so Tunnelsmith sees ciphertext after the 200 Connection Established line and cannot read status codes inside the tunnel. SOCKS5 is a byte stream from the protocol's first frame; nothing on that path is HTTP-shaped from Tunnelsmith's perspective.

The same caveat applies to header injection. Plain-HTTP responses get `X-Tunnelsmith-Upstream` (which upstream served the request) and `X-Tunnelsmith-Retries` (count of failed attempts before success). Cascade-failure 502s carry `X-Tunnelsmith-Cascade` with the host. CONNECT and SOCKS5 paths cannot inject anything because Tunnelsmith does not own the response framing on those paths.

Practical consequence: a client that issues an HTTPS request through Tunnelsmith via CONNECT goes through the dial-loop part of the scoreboard (refused, timeout) but not the status part. If you want a host's HTTP and HTTPS traffic to share rate-limit cycling, send the HTTP traffic as a forward-proxy request (no CONNECT) and the HTTPS traffic via CONNECT to a destination that returns 429 over TLS - the HTTP side will rotate exits, the HTTPS side will not, until Phase 8 ships the body-regex hook for response-side inspection.

Body-regex detection in Phase 8 will share this constraint: it can read response bodies on the plain-HTTP path only.

## See also

- [Architecture](architecture.md): how the scoreboard uses these settings (filled in during Phase 4).
- [Decisions](decisions.md): the running ADR log.
