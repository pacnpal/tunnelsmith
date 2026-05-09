# Tunnelsmith v1.0.0: a per-destination egress router that learns which exit works

Most proxies route the same way no matter what you're talking to. HAProxy round-robins or weights. Squid runs static rules. `scrapy-rotating-proxies` keeps an alive/dead bit per proxy, globally.

That's not how the world actually breaks. In the real world, exit A is fine for `news.example.com` and useless for `api.example.com`. Exit B has the opposite problem. The right exit is per destination, and which exit is right changes minute to minute.

Tunnelsmith ships in front of your apps as an HTTP and SOCKS5 proxy. For every request, it looks up the destination host in a per-(host, upstream) scoreboard, picks the highest-scored upstream that isn't in cooldown, tries the request, and updates the scoreboard based on what happened. When an exit starts failing for a specific host, Tunnelsmith demotes it for that host only and tries the next-best candidate. When the exit recovers, Tunnelsmith probes it occasionally and lets it climb back up.

The result is a proxy that learns, per destination, which exit works.

## What's in the box for v1.0.0

- HTTP CONNECT, HTTP forward, and SOCKS5 listeners. Pick whichever your client speaks.
- A per-(host, upstream) scoreboard with score, cooldowns, time decay, cascade cooling for hosts where every upstream just failed, and a probe chance so previously penalized exits get a fair shot at recovery.
- HTTP status detection (429, 403, 451) on the plain-HTTP forward path, with `Retry-After` honored when the rate-limit rule says so.
- Body-regex inspection on plain-HTTP responses for the soft-block case where a server returns a 200 with a "we don't serve your region" page in the body.
- Per-host `[[rule]]` blocks for `prefer` lists and `force` routing when you want to override the learning loop on a specific host.
- A Mullvad WireGuard pool integration that fans `[[upstream_pool]]` blocks out into one SOCKS5 upstream per active relay, so a flaky exit can be cooled down without taking out an entire city's pool.
- Prometheus metrics on `:9090`, scoreboard persistence to disk, and SIGHUP hot-reload for scoring tunings, status detector, retry cap, rules, and body-buffer cap.
- A web UI on `:9091/` for inspection and admin actions: forget a host, force-pin one to a specific upstream, reset everything.
- A Community Apps template for one-click install on Unraid.

## How the per-host learning loop actually works

The scoreboard keeps one entry per `(host, upstream)` pair. Each entry has a score, a cooldown-until timestamp, a last-seen time, and lifetime success/failure counts. When a request comes in:

1. The scoreboard checks whether the destination host is in cascade cooling. If yes, the listener fails fast.
2. It picks the best non-cooled upstream for the host. "Best" is `(score desc, base priority asc)` after the prefer-rule tiebreak. A small probe chance occasionally picks a non-top candidate so cooled exits get a chance to recover.
3. It dials through that upstream. On success, the score climbs by `success_weight` (clamped at `score_cap`) and the entry's last-seen advances. On failure, the score drops by the failure-kind's penalty and the cooldown extends.
4. Time decay drifts every entry's score toward zero on a configurable interval, so the scoreboard responds to current conditions rather than yesterday's.

If a single request burns through every upstream, Tunnelsmith trips cascade for that host: subsequent requests fail fast for a short TTL instead of stampeding the pool.

The scoreboard persists to disk on a tick, so a restart doesn't throw away what the binary just learned. The persistence loop also runs a prune pass that drops zero-score entries older than `prune_after`, evicts expired cascade entries, and clears stale debounce keys.

## Mullvad: WireGuard, not OpenVPN

If you point Tunnelsmith at Mullvad, you'll be using WireGuard. Mullvad removed OpenVPN entirely on 15 January 2026 (this is documented in [ADR-003](decisions.md#adr-003-mullvad-integration-uses-wireguard-not-openvpn)). The integration runs gluetun in WireGuard mode beside Tunnelsmith and gets a pool of per-relay SOCKS5 endpoints. You generate a Mullvad WireGuard keypair once, drop the private key and addresses into `deploy/.env`, pick the countries you want, and start the stack.

A few things to know about Mullvad's terms before you deploy:

- Each WireGuard keypair counts against Mullvad's 5-device cap. If you already have 5 keys in use, you'll need to revoke one first.
- Mullvad's terms forbid resale. Tunnelsmith is fine for your own apps; if you're planning to charge customers for access through your Mullvad account, read the ToS first.

## Prior art and how Tunnelsmith differs

| Tool | Per-host memory | Learning | Standalone |
|---|---|---|---|
| HAProxy | no | no | yes |
| Squid | static rules | no | yes |
| `scrapy-rotating-proxies` | global alive/dead | partial | no (Scrapy-only) |
| Tunnelsmith | yes (per-(host, upstream)) | yes (score + decay) | yes |

If you're already running HAProxy for L7 routing, Tunnelsmith doesn't replace it. It sits behind it (or in front, depending on your topology) and picks the egress when the destination matters. If your scraping pipeline lives inside Scrapy, `scrapy-rotating-proxies` may be all you need. If you have a mix of apps, none of which speak Scrapy, and you want one proxy in front of all of them, Tunnelsmith is the answer.

## Where to start

- [README quick start](../README.md) - the binary boots HTTP and SOCKS5 listeners on `:8080` and `:1080`.
- [docs/configuration.md](configuration.md) - every config key with defaults and validation rules.
- [docs/deployment.md](deployment.md) - the Mullvad WireGuard recipe.
- [docs/architecture.md](architecture.md) - what the scoreboard tracks and why each rule is there.
- [docs/request-lifecycle.md](request-lifecycle.md) - end-to-end trace of a single request through Tunnelsmith.
- [docs/integration-guide.md](integration-guide.md) - levels 1 through 7 for container maintainers who want to ship Tunnelsmith support.

## Status

v1.0.0 is the first public release. The scaffold went up phase by phase, every phase ended on a green CI build, and every PR went through three automated reviewers (Copilot, Gemini, CodeRabbit) before merge. The image lives at `ghcr.io/pacnpal/tunnelsmith` and is published for `linux/amd64` and `linux/arm64`. Source is MIT-licensed.

Bug reports, feature ideas, and operational war stories are welcome on the [issue tracker](https://github.com/pacnpal/tunnelsmith/issues).
