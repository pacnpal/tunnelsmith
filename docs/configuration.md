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

| key                | type     | default | notes |
|--------------------|----------|---------|-------|
| `bind`             | string   | `:9092` | host:port for the control HTTP listener; empty disables it entirely |
| `auth_tokens`      | []string | `[]`    | Phase 12 inline bearer tokens; empty = no auth (Phase 11 wire shape). Each token must be an RFC 6750 token68 string — empty strings **and any token containing whitespace** (leading, trailing, or embedded) are rejected at config-load time, since `extractBearer` either trims them away or fails them as malformed |
| `auth_tokens_file` | string   | `""`    | Phase 12 absolute path to a one-token-per-line file; `#` comments and blank lines ignored, leading/trailing line whitespace trimmed. A line whose token contains internal whitespace returns an error at load time (same RFC 6750 token68 rule as `auth_tokens`). A missing file at startup warns and is treated as empty (SIGHUP retries). Combined with `auth_tokens` as a dedup'd union |
| `gate_healthz`     | bool     | `false` | when `true` and auth is enabled, `/healthz` also requires `Authorization: Bearer <token>`. Default `false` keeps liveness probes ungated for orchestrators that cannot inject a token |
| `tls_cert_file`    | string   | `""`    | absolute path to a PEM-encoded TLS certificate. Both-or-neither with `tls_key_file`; when both are set the listener terminates HTTPS via `http.Server.ServeTLS`. Empty (the default) keeps the pre-1.2 plaintext wire shape |
| `tls_key_file`     | string   | `""`    | absolute path to the PEM-encoded private key matching `tls_cert_file`. Both-or-neither with `tls_cert_file` |

When set, `bind` must parse via `net.SplitHostPort` with a port in `1-65535`.

The control port is **unauthenticated by default**: `auth_tokens` empty and `auth_tokens_file` unset is the Phase 11 wire shape, and `POST /v1/report` mutates scoreboard state every time it is called. Operators relying on that default should bind to a loopback address or to a private subnet that only trusted apps reach Tunnelsmith over.

When `auth_tokens` and/or `auth_tokens_file` is non-empty (Phase 12), every `POST /v1/report` must carry `Authorization: Bearer <token>`. A missing or malformed header returns `401 Unauthorized` with `WWW-Authenticate: Bearer realm="tunnelsmith"`; rejections tick `tunnelsmith_reports_rejected_total{reason}` with `auth_missing` or `auth_failed`. Combined with `tls_cert_file` / `tls_key_file` (1.2+), the plaintext-token risk ADR-007 named in its Non-goals is closed at the transport layer — see [ADR-009](decisions.md#adr-009-tls-on-the-control-listener-12) for the trust-stance update and [`docs/cooperative-reporting.md`](cooperative-reporting.md) for the wire protocol.

`tls_cert_file` and `tls_key_file` are **opt-in and both-or-neither**. Setting only one is a config-load error so a half-configured deployment can't silently fall back to plaintext. Both empty preserves the Phase 11/12 wire shape byte-for-byte. Both set switches `Serve` from `http.Server.Serve` to `http.Server.ServeTLS`, which loads the PEM files at boot. **Cert rotation is restart-only** in v1 — SIGHUP does not re-read the cert files, matching the existing bind-path policy.

`control.bind` is **restart-only**. SIGHUP hot-reload does not move the listener; the same constraint applies as `[metrics]` and `[ui]`. Restart the binary to change the address.

`auth_tokens` and `auth_tokens_file` **are** hot-reloaded on SIGHUP, but the policy differs from startup. On a clean reload the runtime rebuilds the merged token set and rotates it through `control.Server.ReplaceTokens`. When `auth_tokens_file` is missing on SIGHUP **the runtime does not rotate** — it warns and preserves the current live token set instead. Hard errors (permission denied, IO failure) preserve the current set for the same reason: a logrotate-style brief disappearance or a permission flap should not silently disable auth. Side-effect: a SIGHUP that *also* edits inline `auth_tokens` will not apply those edits while the file is missing. To pick up new tokens, make sure `auth_tokens_file` (when set) resolves cleanly before signalling.

`gate_healthz` is **restart-only**. The handler closure that decides whether `/healthz` is gated is captured at `NewServer` / handler-mount time, so SIGHUP cannot flip the running listener. Restart the binary to change `gate_healthz`. (Phase 13 candidate: hot-rebind the gate.)

## `[[upstream]]`

The config must define at least one upstream, either directly via `[[upstream]]` or by expansion via `[[upstream_pool]]`. Lower priority wins ties; scores from the Phase 4 scoreboard dominate priority once they exist. Every upstream needs a unique `id`.

| key        | type        | default | notes |
|------------|-------------|---------|-------|
| `id`       | string      | none    | required, unique within the file |
| `kind`     | string enum | none    | one of `direct`, `http`, `socks5` |
| `addr`     | string      | none    | required for `http` / `socks5` (`host:port`); must be empty for `direct` |
| `priority` | int         | `100`   | tiebreaker only; lower wins |
| `username` | string      | `""`    | optional proxy-auth username for `http` / `socks5`. For `http`, the dialer sends `Proxy-Authorization: Basic base64(user:pass)` per RFC 7617 on every CONNECT. For `socks5`, the SOCKS5 user/password auth method is offered to the proxy. Ignored for `kind = "direct"` |
| `password` | string      | `""`    | optional proxy-auth password; pairs with `username`. Empty username means no auth |

Validation:
- IDs must be unique across the file (including those produced by `[[upstream_pool]]` expansion).
- `kind = "direct"` must not set `addr`.
- `kind = "http"` and `kind = "socks5"` require `addr` to parse as `host:port` with a non-empty host and a port in `1-65535`.
- `username` / `password` are emitted by the Webshare expander automatically; an operator hand-writing an `[[upstream]]` may also set them to authenticate against any auth-required HTTP CONNECT or SOCKS5 proxy.

> **Credential note.** `username` and `password` are stored in the TOML file in plaintext and re-printed verbatim by `tunnelsmith --print-config`. Treat the config file as a secret: keep it out of version control, restrict its mode to `0o600`, and prefer mounting it from a secret store (Docker secrets, Kubernetes `Secret`, etc.) rather than baking credentials into a committed file. For the Webshare provider the recommended pattern is `api_token_file` (which the expander reads once at startup) — see [`[[upstream_pool]]`](#upstream_pool) below.

## `[[upstream_pool]]`

`[[upstream_pool]]` expands at startup into one or more synthetic `[[upstream]]` entries through a registered provider. Tunnelsmith ships with two providers out of the box:

- `provider = "mullvad"` — fans the configured countries out into one socks5 upstream per active Mullvad WireGuard relay (see [ADR-004](decisions.md#adr-004-mullvad-socks5-hostname-pattern-is-per-server-multihop-not-the-form-in-the-original-plan)).
- `provider = "webshare"` — fans the operator's Webshare proxy plan out into one HTTP-with-Basic-auth (or SOCKS5) upstream per active proxy (see [`docs/providers.md`](providers.md#webshare)).

Both providers share a common set of generic keys and add their own provider-specific keys on top. Adding a third provider is a single-package change documented in [`docs/providers.md`](providers.md#adding-a-new-provider).

### Generic keys

These apply to every provider; provider-specific keys are documented in the per-provider sections below.

| key                | type             | default | notes |
|--------------------|------------------|---------|-------|
| `provider`         | string           | none    | required; must match a registered provider (e.g. `"mullvad"`, `"webshare"`). Unknown values are rejected at config-load with the list of registered providers in the error |
| `id_prefix`        | string           | none    | required; prepended to every generated upstream id (e.g. `mvd` -> `mvd-se-sto-wg-001`); must be unique across pool blocks |
| `priority`         | int              | `200`   | applied to every expanded upstream; default puts pool entries below user-defined `[[upstream]]` (default 100) |
| `refresh`          | duration         | `12h`   | background list-refresh interval; `0s` disables refresh, any positive value below `1m` is rejected to avoid hammering vendor APIs |
| `cache_path`       | string           | `""`    | absolute path for a disk fallback used when the vendor's list endpoint is unreachable |

### `provider = "mullvad"`

Mullvad-specific keys:

| key                | type             | default | notes |
|--------------------|------------------|---------|-------|
| `countries`        | array of strings | none    | required, non-empty; case-insensitive match against the Mullvad relay-list `country` field (`Sweden`, `USA`, ...) |
| `include_inactive` | bool             | `false` | `true` admits relays Mullvad has flagged inactive (rare; mostly for debugging) |

Example:

```toml
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
priority  = 100
countries = ["Sweden", "Netherlands", "Switzerland"]
```

### `provider = "webshare"`

Webshare-specific keys. Exactly one of `api_token` / `api_token_file` is required; the other Webshare fields default to sensible values.

| key               | type             | default    | notes |
|-------------------|------------------|------------|-------|
| `api_token`       | string           | `""`       | Webshare API token; one of `api_token` / `api_token_file` is required. Inline tokens are visible in `--print-config`; prefer `api_token_file` for production |
| `api_token_file`  | string           | `""`       | absolute path to a one-token file; the file's contents are trimmed of surrounding whitespace |
| `mode`            | string           | `"direct"` | Webshare list mode: `direct` (per-proxy IP:port returned by the API) or `backbone` (rotating IPs behind `p.webshare.io`). Must be `backbone` if your plan's `pool_filter` is `residential` |
| `kind`            | string           | `"http"`   | upstream kind to materialise as: `http` (sends `Proxy-Authorization: Basic` per RFC 7617) or `socks5` (uses SOCKS5 user/pass auth) |
| `plan_id`         | string           | `""`       | optional Webshare plan id; empty uses the account's default plan |
| `country_codes`   | array of strings | `[]`       | ISO 3166-1 alpha-2 codes (`"US"`, `"DE"`); empty = no filter. Note: this is **`country_codes`**, not `countries` — Webshare's API filters by ISO code, not display name |
| `proxy_username`  | string           | `""`       | optional override for the per-proxy CONNECT username. When set, every materialised upstream uses this value instead of the one returned by Webshare's proxy-list response. Must be paired with `proxy_password`. Mutually exclusive with `proxy_username_env` |
| `proxy_password`  | string           | `""`       | optional override for the per-proxy CONNECT password. Must be paired with `proxy_username`. Mutually exclusive with `proxy_password_env` |
| `proxy_username_env` | string        | `""`       | name of an environment variable to read the proxy username from at startup. Useful for Docker `environment:` blocks. Read once at startup; the resolved value never appears in `--print-config`. Mutually exclusive with `proxy_username` |
| `proxy_password_env` | string        | `""`       | name of an environment variable to read the proxy password from at startup. Same semantics as `proxy_username_env`. Mutually exclusive with `proxy_password` |

Example:

```toml
[[upstream_pool]]
provider       = "webshare"
id_prefix      = "ws"
priority       = 110
api_token_file = "/etc/tunnelsmith/webshare.token"
mode           = "direct"
kind           = "http"
country_codes  = ["US", "GB", "DE"]
refresh        = "1h"
cache_path     = "/data/tunnelsmith/webshare-cache.json"
```

A config that uses only `[[upstream_pool]]` and no `[[upstream]]` is valid; a config with neither is rejected.

### Shared expansion behavior

The pool expansion runs at startup to seed the priority pool, and a per-block refresh goroutine keeps polling the vendor's list endpoint at the configured interval. As of Phase 11.1, the refresh tick **hot-swaps the running priority pool** on every successful diff: a new `*upstream.Pool` is built from the merged `(static [[upstream]]) ∪ (every block's latest expansion)` slice and installed via `Scoreboard.ReplacePool`. Cached HTTP transports are dropped during the swap so the new pool's `Upstream` objects use freshly-pinned `DialContext` closures. Force pins to upstream ids that disappear in the new expansion are evicted automatically. Per-(host, upstream) entries for removed ids stay in the scoreboard's table but become unreachable through `Pick`; they age out via score decay. The diff is logged at `INFO` (counts plus a small sample) and at `DEBUG` (full id lists), and `tunnelsmith_pool_hotswap_total{result}` counts every swap. SIGHUP still does not touch the pool when `[[upstream_pool]]` is configured — pool-shape changes are owned end-to-end by the refresh ticker, and changes to `[[upstream]]` (static entries) under a pool deployment still require a binary restart. `refresh = "0s"` is allowed and disables periodic refresh entirely; any positive value below `1m` is rejected so a typo cannot hammer the vendor's API.

### Provider-API control

Webshare additionally exposes a vendor API surface through Tunnelsmith's control endpoint (`POST /v1/providers/{id_prefix}/refresh` triggers an on-demand list rotation). See [`docs/control-api.md`](control-api.md) for the full route reference. Mullvad has no vendor API and returns 501 Not Implemented for the refresh route.

## `[failure]`

Failure-detection settings. The signals feed the scoreboard's penalty / cooldown logic.

| key                          | type             | default       | notes |
|------------------------------|------------------|---------------|-------|
| `connection_refused`         | bool             | `true`        | when `false`, dial-side `ECONNREFUSED` errors are not scored: the upstream's penalty and cooldown are unchanged. Applies to both the HTTP forward path and the CONNECT/SOCKS tunnel path. The failed dial is still logged and counted in metrics; only the scoreboard penalty/cooldown step is skipped. Hot-reload (SIGHUP) picks up a changed value live |
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
- `[[upstream_pool]]` block shape (`provider`, `id_prefix`, `priority`, `countries`, `include_inactive`, `refresh`, `cache_path`). The pool's *expansion* is hot-swapped on every refresh tick (Phase 11.1); the *block configuration* itself is not.
- `[[upstream]]` static entries when an `[[upstream_pool]]` block is present. Static-only deployments hot-reload static entries fine; pool deployments need a restart so the composer's startup-captured static slice picks up the change.

The reload outcome is reported on `tunnelsmith_config_reloads_total{result="success" | "error"}`.

## See also

- [Architecture](architecture.md): how the scoreboard uses these settings.
- [Observability](observability.md): the metric surface, scrape config, hot-reload behavior, and Grafana dashboard import.
- [Decisions](decisions.md): the running ADR log.
