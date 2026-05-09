# Web UI

Tunnelsmith ships a small read-and-act web UI on a separate port. The page renders the live scoreboard, the upstream pool, and the active force pins, plus four JSON action endpoints for managing state by hand.

The UI is enabled by default. `[ui] bind = ":9091"` is the out-of-the-box value; set `bind = ""` to turn the listener off entirely. The bind address must parse as `host:port` with a port in `1-65535`.

## The security boundary is the network, not the port

The UI port has **no authentication**. There is no login, no token, no rate limit. Every endpoint, including the four mutating actions, accepts any caller that can reach the port.

This is on purpose. Tunnelsmith is a homelab-scale tool that sits behind the same Docker bridge or private network as the apps it proxies. Adding an auth layer at this scale tends to produce one of two outcomes: an operator picks a weak password and ships it as a default, or the auth layer becomes the thing every operator has to disable on day one. Neither is better than "bind to a network you already trust."

The contract is: bind the UI listener to a loopback address (`127.0.0.1:9091`) or to a private subnet that only your trusted clients can reach. If you need wider access, put your own auth layer in front (an SSH tunnel, a reverse proxy with HTTP basic auth, a Tailscale-only ingress, etc.).

If you ever find yourself binding the UI to `0.0.0.0:9091` on a public network, you have already lost. Anyone can issue `POST /api/reset` and wipe every learned (host, upstream) score on the box.

## Views

The single-page UI shows three tables and a small form, all rendered from the JSON returned by `GET /api/scoreboard` (the page polls every 5 seconds, plus on demand via the Refresh button).

**Upstream pool.** One row per upstream id known to the binary. The "Cooled hosts" column is the count of hosts currently on cooldown for that upstream, derived from the same scoreboard map the `tunnelsmith_upstream_cooled_hosts` Prometheus gauge serves. A single number under the table shows how many hosts are in cascade-failure cooldown.

**Scoreboard.** One row per (host, upstream) entry. Columns: host, upstream id, score, cooldown expiry (rendered red while active), last-seen timestamp, lifetime success count, lifetime failure count, and a "Forget" action button per row.

Sort order is host ascending, then score descending, so the top row per host is the current winner the scoreboard would pick if asked right now. Score is positive (green) for upstreams that have been working, negative (red) for upstreams that have been failing for this host. Cooldown column shows both the absolute timestamp and a relative "in 12s" hint.

**Active force pins.** One row per host that an operator has explicitly pinned to a specific upstream via `POST /api/force` (or via the form below the table). Each row shows host, upstream id, pin expiry, and a "Clear" button that drops the pin without waiting for it to expire. An empty table here is the normal state; force pins are an operator override, not part of the learning loop.

## Action buttons

The UI exposes four mutating actions, each backed by a JSON endpoint on the same port. Each button confirms before sending the request; reset uses the loudest confirmation prompt because it is the only one that affects every host.

- **Forget** (per scoreboard row): drops every (host, upstream) entry for the row's host, clears the host's cascade flag, and wipes the host's debounce window. Does not touch the host's force pin (use Clear or `POST /api/force/clear` for that). Use this when a host's score has settled on the wrong upstream and the easiest fix is to start that host fresh.
- **Pin** (the form below the force-pins table): pins one host to one upstream for the named duration. While the pin is active, every request for the host routes through the pinned upstream regardless of score; if the pin's upstream is exhausted in mid-request retries, normal scoring picks up the slack. Useful for "this destination needs a Swiss exit, period," especially while you wait to write a permanent `[[rule]]`.
- **Clear** (per force-pins row): drops the pin without affecting any other state.
- **Reset all state** (top of page): wipes every entry, every cascade, every force pin, and every debounce key. The pool is left intact; the scoreboard goes back to the empty state it would have on first boot. You almost never want this; it is here for "I changed my entire upstream layout and the scoreboard's previous opinions are now noise."

## Endpoints

The page polls `GET /api/scoreboard`; the four action buttons POST to the four endpoints below. Schema is intentionally boring so curl-based automation works the same as the UI.

`GET /healthz` -> `200 ok`. Liveness probe. Same shape as the metrics server's healthz.

`GET /api/scoreboard` -> `200 application/json`:

```json
{
  "pool_ids": ["direct-a", "direct-b"],
  "entries": [
    {
      "host": "example.com",
      "upstream_id": "direct-a",
      "score": 3.0,
      "cooldown_until": "2026-05-09T12:00:00Z",
      "last_seen": "2026-05-09T11:45:00Z",
      "global_success": 7,
      "global_failure": 2
    }
  ],
  "forces": [
    {"host": "swiss-only.example.com", "upstream_id": "direct-b", "until": "2026-05-09T13:00:00Z"}
  ],
  "cooled_by_upstream": {"direct-a": 0, "direct-b": 1},
  "cascade_active": 0,
  "generated_at": "2026-05-09T11:50:00Z"
}
```

`pool_ids` is the live pool order. `entries` is one element per (host, upstream); zero-value timestamps are omitted from the wire payload entirely, so a missing `cooldown_until` or `last_seen` field means "never set" (an upstream that has never failed for the host has no `cooldown_until`, for example). `forces` lists every active pin, sorted by host. `cooled_by_upstream` mirrors the `tunnelsmith_upstream_cooled_hosts` gauge. `cascade_active` is the count of hosts currently in cascade-failure cooldown. `generated_at` is the wall-clock instant the snapshot was taken (for "is this stale?" checks).

`POST /api/forget` -> `200 application/json`. Body: `{"host": "..."}`. Returns `{"removed": true|false}`; `false` means the host had no scoreboard footprint and the call was a no-op.

`POST /api/force` -> `204 No Content`. Body: `{"host": "...", "upstream_id": "...", "duration": "30m"}` or `{"host": "...", "upstream_id": "...", "until": "2026-05-09T13:00:00Z"}`. Set exactly one of `duration` or `until`. Duration uses Go's `time.ParseDuration` syntax (`30m`, `2h`, `90s`); until is RFC3339. `400` if `upstream_id` is not in the live pool.

`POST /api/force/clear` -> `200 application/json`. Body: `{"host": "..."}`. Returns `{"removed": true|false}`. Always idempotent.

`POST /api/reset` -> `204 No Content`. Body is ignored. Wipes everything.

Every JSON request is capped at 1 MiB; payloads are a few hundred bytes at most, so the cap is a safety net against a misbehaving client. `Content-Type: application/json` is recommended but not required.

## A normal day

Most operators leave the UI port bound and never click anything. The page is mostly useful when a single host is misbehaving and you want to know which upstream the scoreboard has settled on, or when a third-party service has switched your assigned exit to "blocked" and you want to nudge a different one before the scoreboard's normal failure-detection paths catch up.

A representative session:

1. App developer reports `news.example.com` is timing out from one of your apps.
2. You open the UI, sort by host, and see `news.example.com` has score `-4` against `direct-a` and no entry against `direct-b` yet.
3. You click "Forget" on `news.example.com` to clear the bad-state, and "Pin" the host to `direct-b` for 30 minutes so the next request lands on the upstream you want.
4. You watch `cooled_by_upstream.direct-a` tick back to zero on the next refresh and call it done.

For long-lived "this host always uses that exit" decisions, prefer a `[[rule]]` block in the config file. Force pins are for the in-the-moment fix; rules survive restarts, force pins do not (Phase 7's persistence layer keeps the scoreboard, not the pins).

## See also

- [Configuration: `[ui]`](configuration.md#ui)
- [Observability](observability.md) for the metric surface that backs the same counts the UI shows
- [Architecture](architecture.md) for what the score numbers actually mean
