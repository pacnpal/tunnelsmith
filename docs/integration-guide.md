# Integration guide for container maintainers

If you maintain a container that benefits from outbound proxying (`*arr` apps, scrapers, RSS pollers, downloaders, federated services, anything that fetches a lot of HTTP), there are a few levels of Tunnelsmith integration you can ship. Pick the one that matches your project's complexity. The lowest level is "do nothing in code, document the env var" and that already gets your users 90% of the value.

This page is a checklist for app maintainers. The lifecycle of a single request is documented separately in [`docs/request-lifecycle.md`](request-lifecycle.md).

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

Use these composition rules so your example is copy/paste-safe:

1. **Keep the app service unchanged except proxy wiring.** Your "with-tunnelsmith" variant should only change network/proxy settings and add the Tunnelsmith stack.
2. **Ship both topologies and label them clearly.**
   - Docker DNS topology (`HTTP_PROXY=http://tunnelsmith:8080`) when app and tunnelsmith are separate services on the same bridge network.
   - Shared netns topology (`network_mode: "service:tunnelsmith"` + `HTTP_PROXY=http://localhost:8080`) when all app traffic must traverse the VPN namespace.
3. **Document where app ports live.** In shared netns mode, app `ports:` belong on the shared service (`tunnelsmith` or VPN parent), not the app service.
4. **Pin image tags in docs.** Prefer semver tags in published examples so users get reproducible behavior.
5. **Include a one-command verification step.** Example: call `https://am.i.mullvad.net/json` and show expected `"mullvad_exit_ip": true`.

Example "with-tunnelsmith" compose variant maintainers can adapt:

```yaml
services:
  gluetun:
    image: qmcgaw/gluetun:v3.41.1
    cap_add: [NET_ADMIN]
    devices:
      - /dev/net/tun:/dev/net/tun
    environment:
      VPN_SERVICE_PROVIDER: mullvad
      VPN_TYPE: wireguard
      WIREGUARD_PRIVATE_KEY: ${MULLVAD_WIREGUARD_PRIVATE_KEY}
      WIREGUARD_ADDRESSES: ${MULLVAD_WIREGUARD_ADDRESSES}
    ports:
      - "8080:8080" # tunnelsmith proxy
      - "7878:7878" # app UI port (shared netns => publish here)

  tunnelsmith:
    image: ghcr.io/pacnpal/tunnelsmith:1.1.0
    network_mode: "service:gluetun"
    volumes:
      - ./tunnelsmith.mullvad.toml:/etc/tunnelsmith/config.toml:ro
    command: ["--config=/etc/tunnelsmith/config.toml"]

  myapp:
    image: example/myapp:1.2.3
    network_mode: "service:gluetun"
    environment:
      HTTP_PROXY: "http://localhost:8080"
      HTTPS_PROXY: "http://localhost:8080"
      NO_PROXY: "localhost,127.0.0.1,*.internal"
```

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

## Maintainer documentation checklist (what to publish for users)

When you add Tunnelsmith support to your container image, publish these items in your own docs/README:

1. **Feature statement:** "This image supports outbound proxying via `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`."
2. **Topology choice:** one short section for Docker DNS mode and one for shared-netns mode, including which proxy URL to use in each (`tunnelsmith:8080` vs `localhost:8080`).
3. **Complete compose example:** a full, runnable file (not snippets only) including your app + tunnelsmith (+ VPN sidecar if used).
4. **User-configurable knobs:** env vars, config path, and where to place secrets (`.env`, Docker secrets, etc.).
5. **Verification command:** one command users can run to confirm traffic actually routes through the intended exit.
6. **Known pitfalls:** include at least `NO_PROXY`, shared-netns port publishing, and HTTPS status visibility limits without cooperative reporting.

Treat this checklist as the minimum bar for "official Tunnelsmith support" documentation.
