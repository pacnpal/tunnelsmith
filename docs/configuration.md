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

## `[[rule]]`

Optional per-host overrides. Phase 8 enables them in the router; Phase 1 just validates them.

| key         | type             | default | notes |
|-------------|------------------|---------|-------|
| `host_glob` | string           | none    | required; `path.Match` semantics, so `*.bbc.co.uk` matches `news.bbc.co.uk` |
| `prefer`    | array of strings | none    | required; references upstream IDs in priority order |
| `force`     | bool             | `false` | when true, never fall back to upstreams not in `prefer` |

Every id in `prefer` must match an `id` from a defined `[[upstream]]`.

## CONNECT-mode caveat (forward note for Phase 5)

Status-code inspection requires plain HTTP. CONNECT tunnels are encrypted between client and destination; Tunnelsmith cannot read status codes for those. Status-code rules apply only to plain HTTP traffic and to body-regex matches. CONNECT and SOCKS5 streams stay untouched.

## See also

- [Architecture](architecture.md): how the scoreboard uses these settings (filled in during Phase 4).
- [Decisions](decisions.md): the running ADR log.
