# Cooperative outcome reporting

This is the wire-protocol reference for Tunnelsmith's Phase 11 cooperative reporting endpoint. It is the contract any app — in any language — implements to feed per-request outcomes back to the scoreboard.

The motivation: Tunnelsmith cannot read TLS, so HTTPS responses are invisible to the proxy. The legitimate TLS endpoint — your app — already has the cleartext. Cooperative reporting is the channel your app uses to share what it saw.

[`ADR-006`](decisions.md) is the design rationale.

## What you get

Your app submits one JSON object per request. Tunnelsmith maps the outcome to an existing failure kind and feeds the scoreboard. Your next request to the same destination rotates upstreams when an outcome demands it, just like Tunnelsmith already does for plain-HTTP status codes.

You decide what counts as "the request worked" or "this exit is broken for this destination". Outcomes can be richer than HTTP status alone — apps report semantic signals like `geo_block` (HTTP 200 with a region-restricted body) or app-classified `timeout` (a stream stalled even though the TCP connection stayed up).

## What this is not

- Not authentication. The control endpoint trusts the network boundary, same as the UI listener. Bind it to loopback or to a private subnet only.
- Not idempotent. If your app retries a report, the scoreboard gets penalized twice. Deduplicate on your side if you care.
- Not sufficient for uninstrumented clients. Browsers, closed-source SDKs, OS update channels, and anything else you cannot modify do not benefit. ADR-006 lists this explicitly as a non-goal.

## Step 1: read the upstream id

Tunnelsmith advertises which exit served each request via the `X-Tunnelsmith-Upstream` HTTP header.

- **Plain HTTP** (no CONNECT): the header is on every response Tunnelsmith returns.
- **HTTPS via CONNECT**: the header is on the `200 Connection Established` response Tunnelsmith returns to the CONNECT request, before the TLS handshake.

How to read it depends on your client library.

### Go

`http.Transport.OnProxyConnectResponse` is the canonical hook (Go 1.20+). The Tunnelsmith Go SDK ([`github.com/pacnpal/tunnelsmith/client`](../client)) wires this up automatically. If you are not using the SDK:

```go
tr := &http.Transport{
    Proxy: http.ProxyURL(tunnelsmithURL),
    OnProxyConnectResponse: func(ctx context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
        upstream := res.Header.Get("X-Tunnelsmith-Upstream")
        // Stash this somewhere your reporting code can find it.
        _ = upstream
        return nil
    },
}
```

### Python

`urllib3` exposes the CONNECT response on the underlying connection. Reading it requires a small custom adapter; see the urllib3 docs on `pool.urlopen` and `_make_request`. Plain-HTTP clients (`requests`) just read `response.headers["X-Tunnelsmith-Upstream"]`.

### Node.js

`undici` exposes proxy-CONNECT events on `Dispatcher` and `ProxyAgent`. The header lands on the `connect` event's response object. Plain-HTTP responses surface it on `response.headers` directly.

### Bare HTTP client / any language

If your stack lets you parse the raw CONNECT response, the header is plain ASCII and arrives between the status line and the empty line:

```
HTTP/1.1 200 Connection Established\r\n
X-Tunnelsmith-Upstream: mullvad-se-got\r\n
\r\n
```

## Step 2: POST the outcome

```http
POST /v1/report HTTP/1.1
Host: tunnelsmith:9092
Content-Type: application/json

{
  "host":        "example.com:443",
  "upstream":    "mullvad-se-got",
  "outcome":     "geo_block",
  "http_status": 200
}
```

### Required fields

| field | type | notes |
|---|---|---|
| `host` | string | Destination the report applies to. The hostname is what matters — the **scoreboard keys every (host, upstream) pair on hostname only**, mirroring the proxy's own dial-path convention (`internal/scoreboard/hostOnly`). You may submit a `host:port` pair for forensic clarity (e.g. `example.com:443` for HTTPS traffic, `example.com` for plain HTTP); the server normalizes (lowercase, RFC 1123 labels, IP-literal canonicalization) and then strips any port before recording, so `example.com:443` and `example.com` reach the same scoreboard entry. The Go SDK emits `host:port` for HTTPS and hostname-only for plain HTTP; either form works. |
| `upstream` | string | Upstream id from `X-Tunnelsmith-Upstream`. Tunnelsmith rejects ids it does not know with `404`. |
| `outcome` | string | One of the values in the table below. Unknown outcomes return `400`. |

### Optional fields

| field | type | notes |
|---|---|---|
| `http_status` | int | The status the destination returned. Logged for forensics, not used for routing. |

### Outcome vocabulary

This is a **closed set**. Unknown values are rejected so a typo on your side surfaces immediately rather than being silently dropped.

| outcome | when to send it | scoreboard effect |
|---|---|---|
| `ok` | The request succeeded as expected. | `RecordSuccess` (score +1, capped). |
| `rate_limited` | Destination rate-limited the exit's IP (HTTP 429-equivalent semantics, even on a 200). | Maps to `KindRateLimit`. |
| `forbidden` | Destination denied access to the exit (HTTP 403-equivalent). | Maps to `KindForbidden`. |
| `legal_block` | Destination geo-blocked at the legal layer (HTTP 451-equivalent). | Maps to `KindLegalBlock`. |
| `geo_block` | Soft geo-block: 200 with a "not available in your region" body. | Maps to `KindBodyMatch` (Phase 8 soft-rotate semantics). |
| `timeout` | App-detected stall (slow body, partial download). | Maps to `KindTimeout`. |
| `refused` | App-detected connection-level failure surfaced after CONNECT succeeded. | Maps to `KindRefused`. |

The Go SDK auto-reports `429` → `rate_limited`, `403` → `forbidden`, and `451` → `legal_block` for **HTTPS** requests with no app code. Other outcomes — including `ok` — require an explicit `client.Report(resp, outcome)` call.

### Response codes

| status | meaning | what to do |
|---|---|---|
| `204 No Content` | Recorded. | Continue. |
| `400 Bad Request` | Malformed JSON, missing field, unknown outcome, unknown JSON field, or trailing content after the object. | Fix the payload. The body explains the specific problem. |
| `404 Not Found` | `upstream` is not in the pool. Common causes: the upstream id was renamed in tunnelsmith's config, or the report points at an old id from before a hot-reload. | Refresh the upstream id from the next response and retry. |
| `405 Method Not Allowed` | You sent something other than `POST`. | Use `POST`. |
| `413 Payload Too Large` | Body exceeded 4 KiB. | Trim the payload. Required fields are tiny. |
| `503 Service Unavailable` | The scoreboard backend is not yet wired into the control listener (startup race or misconfiguration). | Retry after a short backoff; if persistent, check the operator's startup logs. |

### Trust boundary

There is no auth on `/v1/report`. The operator is responsible for:

- Binding the control listener to loopback or a private subnet (`control.bind` in the config).
- Trusting any client that can reach the listener.

If you need to expose the control endpoint to a public interface, wait for Phase 12 token auth or front it with a reverse proxy that enforces auth itself.

### Body size limit

Reports are capped at **4 KiB**. A well-formed report is well under 200 bytes; the cap is there to bound memory exposure if a misbehaving (or hostile) client streams unbounded JSON.

## Reference: minimal non-Go implementation

Roughly 30 lines in any language with an HTTP client. Pseudo-code:

```
upstream = read_response_header(response, "X-Tunnelsmith-Upstream")
if upstream:
    payload = json.dumps({
        "host":        request.host_port,
        "upstream":    upstream,
        "outcome":     classify(response),
        "http_status": response.status_code,
    })
    http.post(control_url + "/v1/report",
              body=payload,
              headers={"Content-Type": "application/json"},
              timeout=2.0)
    # Best-effort. Do not surface errors to the user-facing request.
```

## Metrics

The control endpoint emits two Prometheus counters under the `tunnelsmith_` namespace:

| metric | labels | meaning |
|---|---|---|
| `tunnelsmith_reports_received_total` | `outcome`, `upstream_id` | Reports the endpoint accepted (status 204). |
| `tunnelsmith_reports_rejected_total` | `reason` | Reports the endpoint refused. `reason` is one of `bad_json`, `missing_field`, `unknown_outcome`, `unknown_upstream`, `scoreboard_unavailable`. |

Watch these as you ship the integration. A spike in `tunnelsmith_reports_rejected_total{reason="unknown_upstream"}` after a config change usually means the app cached an old upstream id; restart the app to clear it.

## Health check

`GET /healthz` on the control listener returns `200 OK` with `ok` as the body when the listener is running. Use it for probes.
