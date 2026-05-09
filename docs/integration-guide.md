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

```
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

```
deploy/
├── docker-compose.yml                    # standard, no VPN
├── docker-compose.with-tunnelsmith.yml   # with full Tunnelsmith + Mullvad stack
└── README.md                             # explains both
```

Users who want VPN routing get a known-good baseline they can adapt.

## Common integration mistakes

These show up repeatedly and are worth calling out.

**Setting `HTTP_PROXY` but not `HTTPS_PROXY`.** Almost every modern API is HTTPS. Setting only `HTTP_PROXY` means Tunnelsmith only sees a tiny fraction of traffic. Set both. Set both. Set both.

**Forgetting `NO_PROXY` for internal services.** If your app talks to a sidecar database, message queue, or local API, those connections will be sent through Tunnelsmith too unless you exclude them:

```
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

**Expecting status-code detection for HTTPS.** Tunnelsmith cannot see status codes inside an HTTPS tunnel. If your app relies on Tunnelsmith handling 429s for HTTPS endpoints, it will not. Configure your app to handle 429s itself for HTTPS, and let Tunnelsmith handle them for HTTP. See [`docs/request-lifecycle.md`](request-lifecycle.md) for the full explanation.

## Verifying your integration works

- App makes a request, Tunnelsmith logs show the request was routed through an upstream.
- Stop the upstream the app's request was using; the app's next request still succeeds (failover works).
- Set `NO_PROXY` to include a known-internal hostname; verify that traffic skips Tunnelsmith (check Tunnelsmith logs for absence).
- Configure a fake destination that returns 429 with `Retry-After: 5`; verify your app gets either a transparent retry success (Tunnelsmith handled it) or sees the 429 and respects `Retry-After` itself (HTTPS case or cascade).
- Phase 7 will add a `/metrics` endpoint that exposes per-upstream counters; once that lands, watch `tunnelsmith_requests_total{upstream=...}` increment as your app makes requests.
