# Control API

> Reference for the HTTP routes served by `[control].bind` (default `:9092`).

The control endpoint serves three families of routes:

- **`POST /v1/report`** — cooperative outcome reports. Documented in [`docs/cooperative-reporting.md`](cooperative-reporting.md).
- **`GET /v1/providers`** + **`POST /v1/providers/{id_prefix}/refresh`** — vendor-API surface for `[[upstream_pool]]` providers that expose one.
- **`GET /healthz`** — liveness probe.

This document covers the provider routes. They are mounted only when the running config carries at least one `[[upstream_pool]]` block — operators running a static `[[upstream]]`-only config get the old wire shape (404 on these paths). When a pool block exists but its provider has no vendor API (e.g. `provider = "mullvad"`), `GET /v1/providers` still lists it (with `has_api: false`) and `POST /v1/providers/{id_prefix}/refresh` returns 501 Not Implemented.

## Authentication

Provider routes share the bearer-token gate that protects `POST /v1/report`. Configure `[control].auth_tokens` or `[control].auth_tokens_file` to require `Authorization: Bearer <token>` on every provider call; with both empty (the default) the routes are unauthenticated and the operator is expected to bind `control.bind` to loopback or a private subnet.

Auth checks for provider routes do **not** tick `tunnelsmith_reports_rejected_total`, since the counter is scoped to cooperative reports.

## `GET /v1/providers`

Returns the registered `[[upstream_pool]]` bindings.

```sh
curl -s http://localhost:9092/v1/providers | jq .
```

Response (200 OK):

```json
[
  {"id_prefix": "mvd", "provider": "mullvad",  "has_api": false},
  {"id_prefix": "ws",  "provider": "webshare", "has_api": true}
]
```

- `id_prefix` matches the `[[upstream_pool]].id_prefix` value in TOML.
- `provider` is the provider name registered in the registry (`mullvad`, `webshare`, …).
- `has_api` is `true` when the provider's `BuildAPI` returned a non-nil API. When `false`, `POST /v1/providers/{id_prefix}/refresh` will return `501 Not Implemented`.

Response is sorted by `id_prefix` for stability.

| Method | Status | Body |
|--------|--------|------|
| `GET`  | 200    | JSON array (above) |
| any other | 405 | `Allow: GET`, plain text |

## `POST /v1/providers/{id_prefix}/refresh`

Triggers an on-demand vendor list refresh through the provider's `API.RefreshProxyList`. For Webshare, this calls `POST /api/v2/proxy/list/refresh/`.

```sh
curl -X POST http://localhost:9092/v1/providers/ws/refresh
```

With an authenticated control listener:

```sh
curl -X POST \
    -H "Authorization: Bearer $TUNNELSMITH_CONTROL_TOKEN" \
    http://localhost:9092/v1/providers/ws/refresh
```

Query parameters:

| param      | type   | notes |
|------------|--------|-------|
| `plan_id`  | string | optional; forwarded to the vendor. Empty = use the account's default plan |

Success response (`202 Accepted`):

```json
{
  "id_prefix": "ws",
  "provider":  "webshare",
  "status":    "accepted"
}
```

Status `202` (not `200`) because the refresh is asynchronous on the vendor side; the new list will arrive on the next refresh tick (or sooner, if the tick is in flight).

Error responses:

| Status | When | Body |
|--------|------|------|
| `401`  | Auth required (token set non-empty) and `Authorization: Bearer <token>` missing/malformed/unknown. | RFC 6750 `WWW-Authenticate: Bearer realm="tunnelsmith"`; plain-text body |
| `404`  | `{id_prefix}` does not match any registered pool block, **or** the entire route family is unmounted (no `[[upstream_pool]]` configured). | plain text |
| `405`  | Wrong method (only `POST` is accepted). | `Allow: POST`; plain text |
| `429`  | The vendor signaled rate-limited (Webshare answers 429 when on-demand refreshes exceed plan quota). Operators should back off before retrying. | JSON refresh-error envelope |
| `501`  | Pool block exists, but the provider has no `API` surface (e.g. `provider = "mullvad"`). | JSON refresh-error envelope |
| `502`  | The vendor's API returned a non-rate-limit error (e.g. Webshare 403 when `on_demand_refreshes_available` is zero). | JSON refresh-error envelope; error message preserved verbatim from the vendor |
| `504`  | The vendor's API did not respond within the 30-second per-request timeout. | JSON refresh-error envelope |

The refresh-error envelope:

```json
{
  "id_prefix": "ws",
  "provider":  "webshare",
  "error":     "vendor rate limited"
}
```

The `error` field is a short category string drawn from a closed vocabulary so vendor-side details (response bodies, internal hostnames, token-derived URLs) never escape through the HTTP response. The full error chain is logged at WARN with `id_prefix`, `provider`, `http_status`, and the wrapped `err`; that log line is the operator-facing diagnostic. Current categories:

| Category              | Status | When |
|-----------------------|--------|------|
| `"upstream timeout"`  | 504    | provider call exceeded the 30-second deadline |
| `"vendor rate limited"` | 429  | provider's API returned a rate-limit response (Webshare 429) |
| `"refresh failed"`    | 502    | any other provider-side error |

## Adding new provider routes

Routes are dispatched in [`internal/control/providers.go`](../internal/control/providers.go). Adding a new operator-callable action (`/v1/providers/{id_prefix}/profile`, `/v1/providers/{id_prefix}/subscription`, …) is a three-file change:

1. Add the method to the `provider.API` interface in [`internal/upstream/provider/provider.go`](../internal/upstream/provider/provider.go).
2. Implement it in every provider's `apiAdapter` (returning `provider.ErrAPINotSupported` for providers that don't have the underlying vendor call).
3. Add the route in `mountProvidersHandlers` and a handler that follows the existing `handleProviderRefresh` shape — auth gate first, lookup, dispatch, JSON envelope.

The new route should ship with a control-handlers unit test (see `internal/control/providers_test.go` for the template) and an entry in this document.
