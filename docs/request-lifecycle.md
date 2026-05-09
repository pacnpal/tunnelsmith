# Request lifecycle

What happens, step by step, when a containerized app sends a request through Tunnelsmith. The example uses three Mullvad SOCKS5 upstreams, but the same flow holds for any upstream mix (direct, HTTP CONNECT, SOCKS5, or auto-discovered relays from `[[upstream_pool]]`).

The companion docs are [`architecture.md`](architecture.md) (scoreboard internals) and [`integration-guide.md`](integration-guide.md) (advice for app maintainers).

## Setup

```yaml
services:
  tunnelsmith:
    image: ghcr.io/pacnpal/tunnelsmith:latest
    # listeners: HTTP on :8080, SOCKS5 on :1080

  myapp:
    image: example/myapp
    network_mode: "service:tunnelsmith"
    environment:
      HTTP_PROXY: "http://localhost:8080"
      HTTPS_PROXY: "http://localhost:8080"
```

`myapp` shares Tunnelsmith's network namespace, so `localhost:8080` reaches Tunnelsmith's HTTP listener. Tunnelsmith has three upstreams configured for this example: `mullvad-nl-ams`, `mullvad-se-got`, `mullvad-us-nyc`.

## Step by step: a plain HTTP request that gets rate-limited

**1. myapp issues the request.**

`curl http://api.example.com/data`. Because `HTTP_PROXY` is set, curl sends:

```http
GET http://api.example.com/data HTTP/1.1
Host: api.example.com
```

to `localhost:8080`.

**2. Tunnelsmith's HTTP listener accepts the connection.**

It parses the request, extracts `host = api.example.com`. This is plain HTTP (not CONNECT), so Tunnelsmith *can see the response status code* later. This matters for failure detection.

**3. Scoreboard lookup.**

First time seeing `api.example.com`. No cached winner. Tunnelsmith picks the highest-scored upstream that is not in cooldown for this host. Say it picks `mullvad-nl-ams`.

**4. Tunnelsmith dials through `mullvad-nl-ams`.**

That upstream is a SOCKS5 proxy at `nl-ams-wg-socks5-001.relays.mullvad.net:1080` (per ADR-004). SOCKS5 handshake completes, then the HTTP request is forwarded.

**5. The destination responds:**

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 47
Content-Type: application/json

{"error": "rate limit exceeded"}
```

**6. Status detector inspects the response.**

- 429 matches a `[[failure.status]]` rule, classified as `KindRateLimit`.
- `Retry-After: 47` parsed via RFC 7231 §7.1.3 (integer-seconds form).
- `honor_retry_after = true` in config, so the cooldown is exactly 47 seconds rather than the config default.

**7. Scoreboard records the failure for THIS host only.**

```text
RecordFailure("api.example.com", "mullvad-nl-ams", KindRateLimit, 47s)
```

- Score for `(api.example.com, mullvad-nl-ams)` drops by the configured penalty.
- Cooldown for that exact pair is set to `now + 47s`.
- `mullvad-nl-ams` is still fully usable for every other host. Only this `(host, upstream)` pair is in cooldown.
- A 429 does not flag the upstream as broken globally; it is just IP-throttled at this destination.

**8. Tunnelsmith retries through the next-best upstream.**

`Scoreboard.Pick("api.example.com", tried)` is called again. `mullvad-nl-ams` is filtered out (in cooldown for this host). Next highest score: `mullvad-se-got`.

The original request is NOT yet returned to myapp. Tunnelsmith is retrying transparently. myapp's `curl` is still blocked on the original single request.

**9. Tunnelsmith dials through `mullvad-se-got` and forwards the same request bytes.**

Different exit IP, fresh rate-limit budget at the destination.

**10. Two possible outcomes:**

**Outcome A (success):**

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"data": "..."}
```

Tunnelsmith records success for `(api.example.com, mullvad-se-got)`, score bumps up. Two response headers are added before forwarding to the client:

```http
X-Tunnelsmith-Upstream: mullvad-se-got
X-Tunnelsmith-Retries: 1
```

The 200 is forwarded to myapp. myapp's curl returns success. **myapp never knew there was a 429 or a retry.** From its perspective: one request, one response, one 200.

**Outcome B (another 429):**

Tunnelsmith records failure on `mullvad-se-got` for this host, increments retry counter, picks the next-best upstream (`mullvad-us-nyc`). Loop continues.

**11. The retry loop has a hard cap.**

`max_retries_per_request = 5` (default). If five upstreams in a row fail:

- Tunnelsmith returns the *last* upstream's response (the 429) to myapp, so myapp gets a real HTTP response and can decide what to do.
- The host is marked as cascade-cooling for `cache.negative_ttl` (default 30 seconds).
- A single structured log line records the cascade event with the host and the chain of upstreams tried.
- The response carries `X-Tunnelsmith-Cascade: api.example.com`.

The next request from myapp to `api.example.com` within the negative TTL window receives a 502 immediately without burning through the upstream pool again.

**12. Subsequent requests benefit from the scoreboard.**

5 seconds later, myapp does `curl http://api.example.com/something-else`. The scoreboard now has state:

- Cached winner for `api.example.com` is `mullvad-se-got` (assuming Outcome A).
- Tunnelsmith picks it immediately, no retry needed.
- The configured probe chance (default 5%) might pick a non-top upstream as a recovery test. If the probe wins anyway, score climbs.

**13. 60 seconds later, `mullvad-nl-ams`'s cooldown for this host has expired.**

But its score is now lower than `mullvad-se-got`'s, so it isn't picked first. It only re-enters via:

- The probe roll, or
- `mullvad-se-got` itself starting to fail (cooldown lifts the floor).

## Behavioral notes

### HTTPS is opaque to Tunnelsmith

When myapp does `curl https://api.example.com/data`, curl sends:

```http
CONNECT api.example.com:443 HTTP/1.1
```

Tunnelsmith opens a tunnel through the chosen upstream and from that point it is just bytes. **Tunnelsmith cannot see HTTP status codes inside the encrypted tunnel.** Rate-limit detection (429), forbidden detection (403), legal-block detection (451), and body-regex matching (Phase 8) only fire for plain HTTP.

For HTTPS, Tunnelsmith only sees:

- TCP-level failures: connection refused, reset, timeout.
- The total bytes transferred and the connection duration.

A 429 from the destination over HTTPS is invisible to Tunnelsmith and is passed straight to the client as part of the encrypted stream.

This is a real limitation. Workarounds:

- Use plain HTTP wherever the client can (many internal APIs and tracker endpoints support both).
- Rely on application-level retry logic (most libraries handle 429 with backoff already).
- Accept that HTTPS-only sites only get TCP-level failover, which still helps with hard blocks and outages.

A future v2 could add a mitmproxy-style TLS interceptor in front of Tunnelsmith, but that requires CA trust distribution to every client and significantly increases complexity.

### SOCKS5 hostname mode matters

When myapp uses `curl --socks5-hostname` (or any SOCKS5 client that lets the proxy resolve), the destination hostname is passed through SOCKS5 and Tunnelsmith uses it as the scoreboard key. Per-host scoring works as designed.

When myapp uses `curl --socks5` (DNS resolved locally before the proxy connect), Tunnelsmith only sees the resolved IP. Per-host scoring then keys on the IP, which is less useful: different hosts can resolve to the same CDN IP, and the same hostname can resolve differently through different exits.

Recommendation: prefer `socks5h://` URLs (the `h` is for "hostname") or `--socks5-hostname` when configuring SOCKS5 clients. For HTTP proxies, the host header is always sent, so this concern does not apply.

### Retry latency adds up

myapp issues one request, Tunnelsmith may dial 1-5 upstreams in sequence. The total response latency from myapp's perspective is the sum of all upstream attempts. A request that needs three retries through SOCKS5 proxies might take 2-5 seconds total instead of 200ms.

Apps with aggressive client-side timeouts may abort before Tunnelsmith finishes its retry loop. This is not strictly a bug, but it means:

- Apps should set client timeouts of at least `(per-upstream timeout) × max_retries_per_request + buffer`.
- Apps that cannot tolerate retry latency should configure smaller `max_retries_per_request` or stricter `[[rule]]` blocks (Phase 8) that limit upstream selection.

### Concurrent requests can pile-on penalties

If myapp fires 10 parallel requests to `api.example.com` and they all hit `mullvad-nl-ams` at the same instant, all 10 will each independently get 429, each will penalize the upstream, each will retry. The scoreboard's mutex serializes the bookkeeping but you may over-penalize a single rate-limit event into ten penalties.

Tunnelsmith collapses multiple `RecordFailure` calls for the same `(host, upstream, kind)` within a 100ms window into a single penalty event. The window is configurable via `failure.scoring.debounce_window`.

### Per-request retry vs per-host caching

**Per-request retry** is what happens within a single client request: Tunnelsmith tries upstream A, fails, tries B, fails, tries C, succeeds, returns C's response. The client sees one outcome.

**Per-host caching** is what makes the next request to the same host fast: the scoreboard now knows C worked for this host, so request #2 starts with C and doesn't re-discover the answer.

Both are needed. Without per-request retry, the first request to any host that hits a bad cached upstream would fail. Without per-host caching, every request would re-probe the upstream pool.
