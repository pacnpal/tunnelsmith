# Deployment

This page is the operator-facing guide for running Tunnelsmith in front of a Mullvad VPN account using `docker compose`. The file `deploy/docker-compose.mullvad.yml` is the reference stack; this page explains why it looks the way it does, how to fill in the secrets, and what to check when something goes wrong.

The companion ADRs are:

- [ADR-001](decisions.md#adr-001-docker-images-are-built-in-ci-not-on-developer-hosts): Tunnelsmith images are built in CI, not on developer hosts. The Mullvad stack is brought up only in CI or on a real deployment target.
- [ADR-003](decisions.md#adr-003-mullvad-integration-uses-wireguard-not-openvpn): Mullvad removed OpenVPN on 2026-01-15. Tunnelsmith uses gluetun in WireGuard mode.
- [ADR-004](decisions.md#adr-004-mullvad-socks5-hostname-pattern-is-per-server-multihop-not-the-form-in-the-original-plan): the SOCKS5 hostname Tunnelsmith dials is derived from each WireGuard relay hostname per a documented transformation rule.

## Topology

```
+----------------------+                        +-------------------+
|        client        |                        |      gluetun      |
| (Sonarr, yt-dlp, ..) |   HTTP / SOCKS5        |  (Mullvad WG)     |
+----------+-----------+ -------------------->  | network_mode      |
           |                                    | shared by         |
           |                                    | tunnelsmith       |
           |                                    |        |          |
           |                                    |        v          |
           |                                    |  +-------------+  |
           |                                    |  | tunnelsmith |--+--> Mullvad SOCKS5 mesh
           |                                    |  +-------------+  |    (one endpoint per relay)
           |                                    +-------------------+
```

Two containers in one network namespace. `gluetun` makes the WireGuard tunnel. `tunnelsmith` runs inside that namespace and dials the per-relay SOCKS5 endpoints documented in ADR-004 (`<location>-wg-socks5-<NNN>.relays.mullvad.net:1080`). The mesh is reachable only from inside a Mullvad WG tunnel, which is why the namespace sharing is required.

The `[[upstream_pool]]` block in `deploy/tunnelsmith.mullvad.toml` does the per-server fan-out: at startup tunnelsmith fetches the live relay list from `api.mullvad.net/public/relays/wireguard/v2/`, filters by the listed countries, and registers one synthetic socks5 upstream per active relay. The scoreboard then learns per-host which relay works best.

## Prerequisites

- A Mullvad subscription (any tier; all plans include unlimited bandwidth).
- A WireGuard keypair generated against your account. Each generated key counts against the **5-device cap**. If you are at the cap, revoke a key you no longer use before adding one for Tunnelsmith.
- Docker and Docker Compose on the host.

## Step 1: get the WireGuard secrets

Mullvad does not expose the WireGuard private key after key generation, so this is a one-time copy step. Lose the values and you regenerate (which costs you a slot from the 5-device cap until you revoke the old key).

1. Sign in at [mullvad.net/en/account](https://mullvad.net/en/account).
2. Open [mullvad.net/en/account/wireguard-config](https://mullvad.net/en/account/wireguard-config) and click **Generate key**.
3. Pick any country (gluetun re-resolves the actual server later), then **Download zip**. You will get one or more `.conf` files.
4. Open one `.conf` file in a text editor. The interesting lines are:
   ```
   [Interface]
   PrivateKey = wK7n...long base64 string...=
   Address    = 10.65.123.45/32,fc00:bbbb:bbbb:bb01::1:6b2d/128
   ```
5. Copy the value of `PrivateKey` into `MULLVAD_WIREGUARD_PRIVATE_KEY`.
6. Copy the value of `Address` into `MULLVAD_WIREGUARD_ADDRESSES`. Keep the comma-separated form so both v4 and v6 are present.

Both values are sensitive. Treat them like passwords. Never commit the populated `.env` to git; the project's `.gitignore` already covers it.

## Step 2: populate `deploy/.env`

```sh
cp deploy/.env.example deploy/.env
$EDITOR deploy/.env
```

Set `MULLVAD_WIREGUARD_PRIVATE_KEY` and `MULLVAD_WIREGUARD_ADDRESSES`. The file's other variables have safe defaults.

## Step 3: pick the countries

Edit `deploy/tunnelsmith.mullvad.toml` and adjust the `countries` list to whatever subset of Mullvad's exit countries you want available. The relay-list refresh is 12 hours by default; if Mullvad adds or removes servers in those countries you pick them up automatically on the next refresh.

```toml
[[upstream_pool]]
provider  = "mullvad"
id_prefix = "mvd"
countries = ["Sweden", "Netherlands", "Switzerland"]
```

Country names are matched case-insensitively against the `country` field returned by the relay API. Common values: `Sweden`, `Netherlands`, `Switzerland`, `Germany`, `USA`, `United Kingdom`, `Australia`, `Canada`, `Japan`, `Singapore`. Find the full list at [mullvad.net/en/servers](https://mullvad.net/en/servers).

## Step 4: bring it up

```sh
docker compose -f deploy/docker-compose.mullvad.yml up -d
```

gluetun comes up first, establishes the WG tunnel, and reports healthy via its built-in `am.i.mullvad.net/connected` check. tunnelsmith waits on the health check via `depends_on: condition: service_healthy`, then starts serving on `:8080` (HTTP CONNECT) and `:1080` (SOCKS5).

Smoke test:

```sh
curl --socks5-hostname localhost:1080 https://am.i.mullvad.net/json | jq .
```

Expected output: `mullvad_exit_ip: true` plus a `country` field that matches one of your configured countries.

For an end-to-end multi-country verifier, run `scripts/verify-mullvad.sh` (this is what CI runs). It expects the secrets to be set in the environment.

## Mullvad's terms of service

Two clauses matter for this deployment.

**Five-device cap.** Mullvad caps each account at five active WireGuard keys. Tunnelsmith uses one slot. If you are deploying Tunnelsmith on multiple machines, each one needs its own keypair, and each one occupies one of the five slots. Generate keys per host and revoke them when you decommission a host.

**No resale or VPN-as-a-feature.** Mullvad's ToS prohibits "utilizing this service to provide a service similar to that provided by Mullvad or other services where VPN constitutes a significant part of the service." This deployment is for personal use of your own Mullvad account in front of your own apps. Running Tunnelsmith as a hosted offering for paying users would put you on the wrong side of that clause; do not do it.

**Other things to know.** Mullvad blocks SMTP (port 25) and Microsoft NetBIOS (137-139, 445) at the egress. Port forwarding has been removed. Heavy automated scraping through Mullvad's IPs hurts other Mullvad users and can get the IPs added to public block lists faster, so keep usage moderate and considerate.

## Troubleshooting

**gluetun never goes healthy.** Likely cause: bad WireGuard secrets. Run `docker compose -f deploy/docker-compose.mullvad.yml logs gluetun | tail -50`. If the logs show a key parse error or "Unauthorized", the `PrivateKey` value is wrong or the key has been revoked. If the logs show "no servers matched", `MULLVAD_SERVER_CITIES` names a city Mullvad does not run a server in.

**tunnelsmith starts but every request errors with `connection refused` from a SOCKS5 host.** Likely cause: gluetun is up but the WG tunnel is silently dropping mesh traffic. Reproduce inside the gluetun container:

```sh
docker compose -f deploy/docker-compose.mullvad.yml exec gluetun \
  wget -qO- https://am.i.mullvad.net/connected
```

If that returns `Mullvad with WireGuard: false` you are not actually inside the tunnel. Restart gluetun.

**tunnelsmith logs `mullvad expander: fetch: ... no such host` at startup.** Likely cause: gluetun's DNS-over-TLS interfering with name resolution inside the namespace. The reference compose sets `DOT: "off"` to avoid this; if you have changed that, change it back.

**"upstream_pool expanded; upstreams=0".** Likely cause: the country names in `tunnelsmith.mullvad.toml` do not match anything Mullvad publishes. Open `https://api.mullvad.net/public/relays/wireguard/v2/` in a browser and confirm the spelling.

**One country verifies but another times out.** Likely cause: that country has a small number of relays and they are all temporarily under load. Add more countries to your pool, or accept that the scoreboard will route around the bad ones automatically once it has tried each one.

**`scripts/verify-mullvad.sh` reports a country mismatch.** That means tunnelsmith routed your request through a relay whose country header does not match the one you asked for. The most common cause is a stale relay list. Restart tunnelsmith to force a fresh fetch:

```sh
docker compose -f deploy/docker-compose.mullvad.yml restart tunnelsmith
```

If the mismatch persists, file an issue with `mullvad_exit_ip_hostname` from the curl output included.

## Where to look next

- [`docs/configuration.md`](configuration.md) - every TOML key, default, and what it does.
- [`docs/request-lifecycle.md`](request-lifecycle.md) - end-to-end trace of a single request through Tunnelsmith.
- [`docs/integration-guide.md`](integration-guide.md) - for maintainers of other containers who want to add Tunnelsmith support.
- [`docs/architecture.md`](architecture.md) - scoring, cooldowns, decay, probing, cascade.
- [`docs/decisions.md`](decisions.md) - architecture decision records.
