# Roadmap

What's deliberately out of scope for v1, in rough priority order. Items listed here are not commitments; they're things we considered, decided not to ship in v1.0.0, and want to evaluate after we have real-world v1 deployment data.

## v1.x maintenance

These are incremental fixes against the v1 surface and may land in a v1.x point release rather than waiting for v2.

- **`failure.connection_refused` opt-out actually works.** The config field is parsed and defaulted to `true`; the runtime does not yet consult it. Tracked in [#20](https://github.com/pacnpal/tunnelsmith/issues/20).
- **Hot-reload of `[[upstream_pool]]` Mullvad relay churn.** Today the refresh tick logs added/removed upstream ids but the running pool is whatever startup produced. SIGHUP also leaves the pool frozen on purpose so a reload does not block on Mullvad's API. A follow-up should swap the pool on the refresh tick directly so a relay rotation lands without a restart.

## v2 candidates

Bigger design decisions worth their own ADR before any code lands.

- **Global degradation detection.** "Upstream X is failing for every host, demote it globally." Tunnelsmith's per-(host, upstream) scoreboard intentionally keeps every host's view independent; a global signal would require either a second aggregated table or a periodic sweep. The proposal lists this as a refinement; the v1 alternative (cascade cooling per host) covers most of the value.
- **UDP support.** v1 is HTTP and SOCKS5 (TCP) only. UDP-over-SOCKS5 is in the SOCKS5 spec; the failure-detection model would have to change since UDP has no connection-refused.
- **Transparent HTTPS interception.** v1 sees plain-HTTP responses fully and tunnels HTTPS opaquely. Interception would let the body-regex and status-code paths cover HTTPS too, at the cost of a CA-on-the-clients deployment story.
- **Custom failure-detection hooks.** Lua or Starlark scripts that decide failure from the response. The Phase 8 `[[rule]].body_regex` handles the most common case; full scripting is more flexibility than v1 needed.
- **Multi-instance clustering.** Two Tunnelsmith binaries sharing one scoreboard. v1 is single-process; a second instance starts cold. Useful for HA but adds a coordination dependency.
- **Authentication on the proxy listeners.** v1 binds to loopback or a private network and trusts the network boundary. A token or basic-auth gate would let it run on a public interface; matches Squid's model.
- **Active health-checking of upstreams.** v1 only learns from real traffic. Active probing would catch a quiet upstream that has been broken since startup, at the cost of touching exits the user has not asked for.

## How to propose changes

Open a GitHub issue with a `roadmap:` prefix in the title. If the change has design tradeoffs, draft an ADR in `docs/decisions.md` first; if it's a fix or a small extension, an issue is enough.
