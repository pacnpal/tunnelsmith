# Observability

Tunnelsmith exposes Prometheus metrics, structured JSON logs, and on-disk
scoreboard snapshots. This page covers the metric surface, the log
schema, and how to wire Tunnelsmith into a typical Prometheus + Grafana
stack.

## Endpoints

When `metrics.bind` is set (default `:9090`), Tunnelsmith serves two HTTP
endpoints on that address:

- `GET /metrics` - Prometheus exposition format.
- `GET /healthz` - returns `200 OK` with the body `ok`. Suitable for a
  Docker `HEALTHCHECK` or a Kubernetes liveness probe.

Setting `metrics.bind` to the empty string disables the listener
entirely. The metrics endpoint is unauthenticated; bind it to an
internal interface only.

## Scrape config

Minimal Prometheus job:

```yaml
scrape_configs:
  - job_name: tunnelsmith
    scrape_interval: 15s
    static_configs:
      - targets: ["tunnelsmith:9090"]
```

Use the container or host name that resolves on the same network
Tunnelsmith binds to.

## Metric reference

All names use the `tunnelsmith_` prefix. Cardinality is bounded on
purpose: per-host labels are not emitted because thousands of
destinations across 60+ Mullvad relays would blow out a small
Prometheus instance. The web UI on `:9091/` carries the per-host
detail.

### Counters

| Name | Labels | Meaning |
| --- | --- | --- |
| `tunnelsmith_dial_attempts_total` | `upstream_id`, `outcome` | One increment per upstream dial attempt. Emitted by the scoreboard's `DialFor` (CONNECT and SOCKS5 paths) and by the HTTP plain-HTTP forward path. A response that the status detector flagged as a failure (429 / 403 / 451) still records `outcome=success` here because the connection itself succeeded; the failure dimension lives on `status_failures_total`. Outcome is `success`, `refused`, `timeout`, or `other`. |
| `tunnelsmith_status_failures_total` | `upstream_id`, `code` | One increment per HTTP response the listener treated as a failure. Code is the upstream's HTTP status as a decimal string (`429`, `403`, `451`). |
| `tunnelsmith_request_outcomes_total` | `outcome` | One increment per terminal client-request outcome. Outcome is `success`, `cascade`, or `exhausted`. |
| `tunnelsmith_cascade_trips_total` | none | Number of times the scoreboard tripped cascade for a host after every upstream failed. |
| `tunnelsmith_probe_picks_total` | none | Number of times the probe roll picked a non-top eligible candidate. |
| `tunnelsmith_persistence_writes_total` | `result` | Snapshot-write outcomes. Result is `success` or `error`. |
| `tunnelsmith_config_reloads_total` | `result` | SIGHUP-driven reload outcomes. Result is `success` or `error`. |

### Histograms

| Name | Labels | Meaning |
| --- | --- | --- |
| `tunnelsmith_dial_latency_seconds` | `upstream_id`, `outcome` | Wall-clock latency of dial attempts. On the scoreboard's `DialFor` it measures the TCP / SOCKS5 handshake; on the HTTP plain-HTTP forward path it measures the full RoundTrip (handshake plus request / response headers), so its values run higher than the scoreboard path. Buckets are 12 exponentially-spaced bins from 5ms to ~10s. Outcome matches the dial attempts counter. |

### Gauges

| Name | Labels | Meaning |
| --- | --- | --- |
| `tunnelsmith_upstream_pool_size` | none | Number of upstreams currently in the pool. Updates at startup and after a SIGHUP reload. |
| `tunnelsmith_scoreboard_entries` | none | Total number of (host, upstream) entries the scoreboard is tracking. Refreshes every 5 seconds. |
| `tunnelsmith_upstream_cooled_hosts` | `upstream_id` | Number of hosts currently on cooldown for the labelled upstream. Refreshes every 5 seconds. |
| `tunnelsmith_cascade_active_hosts` | none | Number of hosts currently in cascade-failure cooldown. Refreshes every 5 seconds. |

## Useful queries

Per-upstream success rate over the last 5 minutes:

```promql
sum by (upstream_id) (rate(tunnelsmith_dial_attempts_total{outcome="success"}[5m]))
/
sum by (upstream_id) (rate(tunnelsmith_dial_attempts_total[5m]))
```

Top 5 upstreams by 429 rate:

```promql
topk(5, sum by (upstream_id) (rate(tunnelsmith_status_failures_total{code="429"}[5m])))
```

Cascade-failure rate (hosts entering cascade per second):

```promql
rate(tunnelsmith_cascade_trips_total[5m])
```

Probe-pick share of all picks:

```promql
rate(tunnelsmith_probe_picks_total[5m])
/
sum(rate(tunnelsmith_dial_attempts_total[5m]))
```

p95 dial latency by upstream and outcome:

```promql
histogram_quantile(
  0.95,
  sum by (upstream_id, outcome, le) (rate(tunnelsmith_dial_latency_seconds_bucket[5m]))
)
```

## Grafana dashboard

`deploy/grafana-dashboard.json` is an importable dashboard that visualizes
the queries above. In Grafana, click **Dashboards → Import → Upload JSON
file**, point at `deploy/grafana-dashboard.json`, and pick the Prometheus
data source that scrapes Tunnelsmith.

## Persistence

When `cache.persist_path` is set, the scoreboard snapshots its state to
that path on every `cache.persist_interval` tick (default 30s) and once
at shutdown. The format is a 4-byte magic header (`TSB1`) followed by a
big-endian uint32 version and a gob-encoded snapshot struct. Writes go
through a temp file plus `os.Rename` so a crash mid-write cannot leave
the path holding a half-written file.

Set `cache.persist_path` to a path on a writable volume the container
can persist across restarts (e.g. `/data/scoreboard.gob`). Setting
`cache.persist_interval = "0s"` disables periodic writes; the shutdown
flush still runs.

The snapshot tick also runs the prune pass: zero-score entries whose
`lastSeen` is older than `failure.scoring.prune_after` get dropped, and
empty per-host maps are removed. Expired cascade entries and stale
debounce keys are evicted alongside.

## Hot-reload

Sending `SIGHUP` to the running Tunnelsmith process re-reads the config
file from the same path it was loaded from. The reload is best-effort:
if the new config fails to parse or validate, the binary logs a warning
and keeps running on the old config. The reload outcome is reported on
`tunnelsmith_config_reloads_total{result="success" | "error"}`.

What hot-reload changes:

- `[[upstream]]` list (rebuilds the priority pool, swaps it into the
  scoreboard, drops cached transports for upstreams that disappeared)
- `[failure]` retry cap and status rules (HTTP listener swaps in a new
  detector and retry budget)
- `[failure.scoring]` penalty weights, cooldowns, probe chance, cascade
  TTL, debounce window, prune-after
- `[[rule]]` block list (parsed, validated against the live pool, and applied to the request path)

What hot-reload does NOT change (restart required):

- `[listener]` bindings - the HTTP and SOCKS5 listen addresses are bound
  once at startup
- `metrics.bind` - same reason as above
- `cache.persist_path` and `cache.persist_interval` - the persistence
  loop captures these at startup
- `failure.scoring.decay_interval` - the decay goroutine reads its
  ticker once at start; retuning mid-flight is more risk than benefit
  for v1
- `[[upstream_pool]]` (Mullvad refresh schedule) - pool blocks are
  expanded once at startup; the periodic refresh inside each block uses
  its own ticker

## Logs

Tunnelsmith writes structured JSON to stderr through `log/slog`. Level
is read from the `TUNNELSMITH_LOG_LEVEL` env var (`debug` / `info` /
`warn` / `error`; default `info`).

Events worth tagging in a log forwarder:

- `msg=upstream dial outcome=failure` - one upstream failed a single
  dial attempt. `kind` carries the classification.
- `msg=cascade tripped` - a host went into cascade-failure cooldown.
- `msg=forward status failure` - the HTTP plain-HTTP path treated a
  response as a failure and rotated to the next upstream.
- `msg=config reloaded` - a SIGHUP reload landed cleanly. `msg=config
  reload failed` covers the error path.
- `msg=scoreboard snapshot written` (DEBUG) - one persistence-tick flush
  succeeded. The error path is `msg=scoreboard snapshot failed`
  (WARN).
