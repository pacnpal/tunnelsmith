# Integration guide for container maintainers

If you maintain a container that benefits from outbound proxying (`*arr` apps, scrapers, RSS pollers, downloaders, federated services, anything that fetches a lot of HTTP), there are a few levels of Tunnelsmith integration you can ship. Pick the one that matches your project's complexity. The lowest level is "do nothing in code, document the env var" and that already gets your users 90% of the value.

This page is a checklist for app maintainers. The lifecycle of a single request is documented separately in [`docs/request-lifecycle.md`](request-lifecycle.md).

**Pick the upstream that fits your users.** Tunnelsmith ships with two `[[upstream_pool]]` providers out of the box and is designed to accept more via an adapter PR:

- **`provider = "mullvad"`** — public WireGuard relay list, no API key, multihop SOCKS5. Best for users who already pay for Mullvad and want geo-diverse exits without a per-IP cost. Walkthrough at [`docs/deployment.md#use-with-mullvad`](deployment.md#use-with-mullvad).
- **`provider = "webshare"`** — token-authenticated REST API, paginated HTTP (or SOCKS5) proxies with per-proxy Basic auth, on-demand list refresh via Tunnelsmith's control endpoint. Best for users who want a pool of IP addresses (not just countries) and may want to rotate them programmatically. Walkthrough at [`docs/providers.md#webshare`](providers.md#webshare).

Both providers expand into the same priority pool, so your container's HTTP/SOCKS5 client doesn't change shape based on which the operator picked. Document the env-var pattern below and let the operator decide.

If you're shipping support for a different upstream provider, [`docs/providers.md#adding-a-new-provider`](providers.md#adding-a-new-provider) is the adapter-author guide.

## Level 1: document the standard env-var pattern

Almost every HTTP library on every language honors `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` env vars. If yours does, you are already done. Just document it.

In your README, add a section like:

```markdown
## Using with an HTTP proxy (Tunnelsmith, mitmproxy, corporate proxy)

This container respects the standard `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY`
environment variables. Example with Tunnelsmith:

    services:
      myapp:
        image: example/myapp
        environment:
          HTTP_PROXY: "http://tunnelsmith:8080"
          HTTPS_PROXY: "http://tunnelsmith:8080"
          NO_PROXY: "localhost,127.0.0.1,*.internal"
```

That's it. Most apps need nothing else.

## Level 2: document the network_mode topology

For users who want the container's *entire* network stack to go through a VPN (with Tunnelsmith multiplexing), they need to share a network namespace. Document this pattern explicitly:

```markdown
## Routing all traffic through a VPN

To send all of this container's traffic through a VPN tunnel managed by Tunnelsmith,
share the network namespace of the Tunnelsmith container:

    services:
      tunnelsmith:
        image: ghcr.io/pacnpal/tunnelsmith:latest
        # Tunnelsmith config and Mullvad/Gluetun stack as documented at
        # https://github.com/pacnpal/tunnelsmith/blob/main/docs/deployment.md

      myapp:
        image: example/myapp
        network_mode: "service:tunnelsmith"
        # No `ports:` here. They belong on the tunnelsmith service.
        environment:
          HTTP_PROXY: "http://localhost:8080"
          HTTPS_PROXY: "http://localhost:8080"

When using `network_mode: "service:tunnelsmith"`, all of your container's
exposed ports must be declared on the `tunnelsmith` service instead. The
two containers share one network stack.
```

## Level 3: add a config flag or env var for proxy URL

If your app has its own config system that does not naturally read `HTTP_PROXY`, add an explicit `proxy_url` setting. Example:

```yaml
# myapp config
network:
  proxy_url:      "http://tunnelsmith:8080"   # accepts http, socks5, socks5h
  proxy_no_match: ["localhost", "*.internal"]
```

In code, translate to whatever your HTTP client expects:

```go
// Go example
proxyURL, _ := url.Parse(cfg.Network.ProxyURL)
transport := &http.Transport{
    Proxy: http.ProxyURL(proxyURL),
}
client := &http.Client{Transport: transport}
```

```python
# Python example (requests library)
proxies = {
    "http":  cfg.network.proxy_url,
    "https": cfg.network.proxy_url,
}
response = requests.get(url, proxies=proxies)
```

```javascript
// Node.js example (using undici)
import { ProxyAgent, fetch } from 'undici'
const agent = new ProxyAgent(cfg.network.proxyUrl)
const res = await fetch(url, { dispatcher: agent })
```

Document this in your README alongside the env-var pattern.

## Level 4: honor 429 backoff intelligently

If your app makes many automated requests (scraper, indexer poller, federation crawler), do this even without Tunnelsmith. With Tunnelsmith it composes well.

1. **Don't retry on 429 yourself if a proxy chain is configured.** Let Tunnelsmith handle the retry. Configure your client's retry logic to NOT retry on 429 when a proxy is in use, or set a low retry count (1 or 2) so you don't double-retry on top of Tunnelsmith's retries.
2. **Honor `Retry-After` headers** for the cases where Tunnelsmith bounces the 429 back to you (cascade failure or HTTPS opacity). If your app sees a 429 with `Retry-After`, wait that long before re-issuing.
3. **Log proxy failures distinctly from destination failures.** A 502 from your proxy chain is different from a 502 from the destination. Make this visible in your logs so users can tell which is which.

## Level 5: Tunnelsmith-aware retry hints

Optional and only worth doing for high-volume scrapers. Tunnelsmith adds two response headers to every plain-HTTP response it serves:

```http
X-Tunnelsmith-Upstream: mullvad-se-got
X-Tunnelsmith-Retries: 2
```

Apps can read these headers to:

- Surface "this request went through fallback exit X" in their UI.
- Adjust per-host request rate dynamically (if you needed 2 retries, slow down).
- Report exit-by-exit success rates back to users.

If you implement this, document your support in your README so users know your app is Tunnelsmith-aware.

## Level 6: respect cascade signals

When Tunnelsmith returns 502 with the response header `X-Tunnelsmith-Cascade: <host>`, every configured upstream failed for that host within the negative TTL window. If your app sees this, the right behavior is:

- Don't retry immediately.
- Back off for at least `cache.negative_ttl` (default 30s).
- If your app has user-facing surfaces, communicate "all configured exits are currently failing for `<host>`" rather than a generic error.

```python
if response.status_code == 502 and 'X-Tunnelsmith-Cascade' in response.headers:
    host = response.headers['X-Tunnelsmith-Cascade']
    log.warning(f"All exits failing for {host}, backing off 30s")
    time.sleep(30)
    return  # let the next scheduled run try again
```

## Level 7: ship an "official" docker-compose snippet

If you ship docker-compose templates with your app, ship one variant that includes a Tunnelsmith stack pre-configured:

```text
deploy/
├── docker-compose.yml                    # standard, no VPN
├── docker-compose.with-tunnelsmith.yml   # with full Tunnelsmith + Mullvad stack
└── README.md                             # explains both
```

Users who want VPN routing get a known-good baseline they can adapt.

## Level 8: report HTTPS outcomes back to Tunnelsmith (Phase 11)

Tunnelsmith cannot inspect HTTPS responses; the proxy carries TLS bytes blindly. Your app *can* see those responses — it terminates the TLS, so the cleartext is right there. The cooperative reporting protocol lets your app submit per-request outcomes back to Tunnelsmith so the scoreboard learns from HTTPS the same way it does from plain HTTP. This is the difference between "Tunnelsmith only sees dial failures on HTTPS" and "Tunnelsmith fully understands which exits are working for which destinations".

**Go: three lines.** Use the SDK at [`github.com/pacnpal/tunnelsmith/client`](../client). The wrapped `*http.Client` captures the chosen upstream id automatically, auto-reports HTTPS `429` / `403` / `451`, and lets you submit semantic outcomes (soft geo-block, app-detected timeout, custom signals) via `client.Report`.

```go
import "github.com/pacnpal/tunnelsmith/client"

c, err := client.New(client.Options{
    ProxyURL:   "http://tunnelsmith:8080",
    ControlURL: "http://tunnelsmith:9092",
})
resp, err := c.Get("https://example.com/api/things")
// HTTPS 429/403/451 auto-report. For semantic outcomes:
_ = client.Report(resp, "geo_block")
```

A runnable example lives at [`examples/integration/main.go`](../examples/integration/main.go). Pass `--token <value>` to test against an operator who has opted into Phase 12 bearer-token auth.

**Other languages: ~30 lines.** The wire protocol is documented in [`docs/cooperative-reporting.md`](cooperative-reporting.md) — read one HTTP header off the response, POST one JSON object to the control endpoint. Any HTTP client in any language can implement it.

**If the operator has Phase 12 auth on**, attach a bearer credential to every `POST /v1/report`. With the Go SDK, set `client.Options.Token`; it is added as `Authorization: Bearer <token>` to every report POST. For non-Go apps the wire change is one header. The operator-side knobs (`[control].auth_tokens`, `[control].auth_tokens_file`, SIGHUP rotation, the `401` + `WWW-Authenticate` response, and the `auth_missing` / `auth_failed` metric labels) are documented in [`docs/cooperative-reporting.md`](cooperative-reporting.md#auth-phase-12) and [`docs/configuration.md`](configuration.md#control). The SDK does not retry 401, since it always indicates a configuration mismatch rather than a transient fault.

**If the operator has Phase 14 TLS on (v1.2.0+)**, the control listener terminates HTTPS on the same port via `[control].tls_cert_file` + `[control].tls_key_file` (both absolute paths *inside* the Tunnelsmith container — mount them with `-v /host/certs:/etc/tunnelsmith/tls:ro`). Set `ControlURL: "https://tunnelsmith:9092"` in `client.Options` — the embedded `*http.Client` uses the system trust store, so no extra code is needed for a publicly trusted cert. For an internally issued cert, mount your CA bundle into the *app* container (e.g. `-v ./ca.crt:/etc/ssl/certs/ts-ca.crt:ro`) and either rely on the distro's CA refresh hook or set `client.Options.HTTPClient` to a `*http.Client` whose `Transport.TLSClientConfig.RootCAs` includes the bundle. Empty defaults preserve the Phase 11/12 plaintext wire shape byte-for-byte; the SDK auto-detects the scheme from `ControlURL` and does not need a separate "TLS on/off" flag. For non-Go apps, the only change is the `http://` → `https://` flip in the URL you POST to (plus the same CA-mount step on internal certs).

## Provider-specific notes: Webshare (v1.2.0+)

When the operator picks `provider = "webshare"` for an `[[upstream_pool]]` block, your container's wire shape does not change — your `HTTP_PROXY` / `HTTPS_PROXY` still points at one Tunnelsmith address and you still get a single priority pool. Three Webshare-specific patterns are worth knowing about so you do not bake vendor-specific code into your container by accident.

**Per-proxy auth is invisible to your app.** Webshare hands every proxy in your plan a unique HTTP-Basic or SOCKS5 user/pass pair. Tunnelsmith threads those credentials into the upstream handshake automatically from the operator's API token — your app never sees them and never sets them. Do **not** document a Webshare-specific "set proxy auth" instruction in your README; the env-var pattern from Level 1 is the only thing your users need.

**On-demand IP rotation is operator-side, not app-side.** If your app hits a destination that consistently bans a Webshare exit, the operator (or a sidecar container on the same Docker network) can `POST http://tunnelsmith:9092/v1/providers/<id_prefix>/refresh` to trigger Webshare's `/proxy/list/refresh/` and pull a fresh set of IPs without restarting the Tunnelsmith container. The simplest pattern is a one-shot `curlimages/curl` container fired from cron or your orchestrator; no app code change is needed. The route is bearer-gated when the operator has Phase 12 auth on (same `auth_tokens` set as `POST /v1/report`):

```yaml
# docker-compose.yml — rotation as a separate, restartable container
services:
  tunnelsmith:
    image: ghcr.io/pacnpal/tunnelsmith:1.2.0
    # ...as in the README's Use with Webshare example

  ts-rotate:
    image: curlimages/curl:latest
    profiles: ["manual"]    # `docker compose run ts-rotate` to fire on demand
    environment:
      TUNNELSMITH_CONTROL_TOKEN: ${TUNNELSMITH_CONTROL_TOKEN}
    command:
      - -fsS
      - -X
      - POST
      - -H
      - "Authorization: Bearer ${TUNNELSMITH_CONTROL_TOKEN}"
      - http://tunnelsmith:9092/v1/providers/ws/refresh
```

Your app should not call this on its own — it spends an operator's vendor-side rotation budget. If you want users to wire automated rotation into your container's failure path, document the route and let them opt in. The route shape, status codes, and 429 / 502 handling are in [`docs/control-api.md`](control-api.md#post-v1providersid_prefixrefresh).

**Webshare upstream ids land in the response header.** Tunnelsmith stamps every plain-HTTP response with `X-Tunnelsmith-Upstream: <id_prefix>-<proxy_id>` (e.g. `ws-1234567`). The Level 5 hint above already covers reading this header; on Webshare specifically, the `<proxy_id>` tail is the same id you see in the Webshare dashboard, so a "this request rotated through proxy X" surface in your UI is one click away from the vendor view. For HTTPS responses the SDK captures the same id automatically via the `X-Tunnelsmith-Upstream` header set on the CONNECT response (Phase 11).

**Mixing providers in one pool.** A single config can stack a Mullvad block and a Webshare block. The scoreboard learns per-host which exit works best across both. Your container does nothing differently — the priority pool the proxy listener reads is provider-agnostic. The provider-author guide at [`docs/providers.md#adding-a-new-provider`](providers.md#adding-a-new-provider) covers the contract a future provider adapter has to satisfy, and your existing integration will pick up new providers without code changes.

When this is worth doing:

- Your app fetches HTTPS resources where the upstream's behavior matters (rate limiting, regional content, soft geo-blocks served as `200 OK` with a "not available in your region" page).
- You already have the response in your app and can classify it.
- You want Tunnelsmith's per-(host, upstream) scoreboard to rotate exits intelligently for your specific traffic.

When to skip it:

- Your app's HTTPS requests are uniform and you only care about hard failures (refused, timeout). The dial path already covers those.
- You can't modify your app (third-party SDK, closed-source binary). MITM is the alternative; see [ADR-006](decisions.md) for why Tunnelsmith does not ship MITM.

## Common integration mistakes

These show up repeatedly and are worth calling out.

**Setting `HTTP_PROXY` but not `HTTPS_PROXY`.** Almost every modern API is HTTPS. Setting only `HTTP_PROXY` means Tunnelsmith only sees a tiny fraction of traffic. Set both. Set both. Set both.

**Forgetting `NO_PROXY` for internal services.** If your app talks to a sidecar database, message queue, or local API, those connections will be sent through Tunnelsmith too unless you exclude them:

```bash
NO_PROXY="localhost,127.0.0.1,db,redis,*.local,*.internal"
```

This is one of the most common deployment problems: an app's internal health check goes through the VPN and times out because the VPN can't route to a local docker DNS name. Always set `NO_PROXY`.

**Using `network_mode` AND `HTTP_PROXY` to a different address.** If your container is already inside Tunnelsmith's network namespace via `network_mode: "service:tunnelsmith"`, then `localhost:8080` IS Tunnelsmith. Setting `HTTP_PROXY=http://tunnelsmith:8080` instead of `http://localhost:8080` will fail because there is no `tunnelsmith` host inside that namespace; it is the container's own namespace. Use `localhost`.

Conversely, if your container is on a separate Docker network and reaches Tunnelsmith over Docker DNS, use `http://tunnelsmith:8080`. Don't mix the two.

**Forgetting that ports declared on a `network_mode`-shared container don't work.**

```yaml
myapp:
  image: example/myapp
  network_mode: "service:tunnelsmith"
  ports:
    - "9000:9000"   # silently ignored
```

When you share a network namespace, the shared container (Tunnelsmith) owns all port declarations. Move `myapp`'s exposed ports up to the Tunnelsmith service:

```yaml
tunnelsmith:
  image: ghcr.io/pacnpal/tunnelsmith:latest
  ports:
    - "9000:9000"   # myapp's port lives here

myapp:
  network_mode: "service:tunnelsmith"
  # no ports section
```

**Expecting status-code detection for HTTPS without integrating.** Tunnelsmith cannot see status codes inside an HTTPS tunnel on its own. If your app relies on Tunnelsmith handling 429s for HTTPS endpoints, it will not — unless you opt in to cooperative reporting (Level 8 above). Without that, configure your app to handle 429s itself for HTTPS, and let Tunnelsmith handle them for HTTP. See [`docs/request-lifecycle.md`](request-lifecycle.md) for the full explanation, and [`docs/cooperative-reporting.md`](cooperative-reporting.md) for the integration that lifts this restriction.

## Verifying your integration works

- App makes a request, Tunnelsmith logs show the request was routed through an upstream.
- Stop the upstream the app's request was using; the app's next request still succeeds (failover works).
- Set `NO_PROXY` to include a known-internal hostname; verify that traffic skips Tunnelsmith (check Tunnelsmith logs for absence).
- Configure a fake destination that returns 429 with `Retry-After: 5`; verify your app gets either a transparent retry success (Tunnelsmith handled it) or sees the 429 and respects `Retry-After` itself (HTTPS case or cascade).
- The `/metrics` endpoint (default `:9090`) exposes per-upstream counters. Watch `tunnelsmith_dial_attempts_total{upstream_id=...,outcome=...}` and `tunnelsmith_request_outcomes_total{outcome=...}` increment as your app makes requests; see [observability.md](observability.md) for the full metric reference.
