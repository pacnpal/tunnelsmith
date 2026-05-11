# Providers

> Reference for the `[[upstream_pool]]` provider abstraction and a step-by-step guide for adding a new provider via fork + PR.

Tunnelsmith expands `[[upstream_pool]]` blocks into synthetic `[[upstream]]` entries by handing the block to a **provider**. A provider is a small Go package that knows:

1. **How to turn a config block into a list of upstreams** (the `Expander`).
2. **Optionally, how to drive the vendor's REST API** (the `API` — what the control endpoint at `POST /v1/providers/{id_prefix}/refresh` calls into).
3. **How to validate its own config**, so a typo fails at `config.Load` instead of at the first dial.

The contracts live in [`internal/upstream/provider/provider.go`](../internal/upstream/provider/provider.go). The two in-tree providers are:

| Name        | Package                          | Notes |
|-------------|----------------------------------|-------|
| `mullvad`   | `internal/upstream/mullvad`      | Public relay list, no vendor API (`BuildAPI` returns `ErrAPINotSupported`) |
| `webshare`  | `internal/upstream/webshare`     | Token-authenticated REST API, paginated list + on-demand refresh |

## Webshare

The Webshare provider wraps Webshare's `https://proxy.webshare.io/api/v2/` REST API.

### What it does

- Fetches the proxy list via `GET /api/v2/proxy/list/?mode={direct|backbone}` (paginated, follows `next` automatically up to 500 pages).
- Filters out invalid proxies (`valid = false`) so the priority pool never carries a probe-known-dead entry.
- Materialises one `[[upstream]]` per proxy with `kind = http` (default) or `kind = socks5`, threading the per-proxy `username` / `password` through so the dialer authenticates on every CONNECT.
- Sorts the resulting slice by upstream id so a stable Webshare list produces a stable Tunnelsmith pool (the hot-swap diff is then a no-op).
- Caches the raw proxy list to `cache_path` (when set) and falls back to the cache when the API is unreachable.
- Exposes `POST /api/v2/proxy/list/refresh/` through the control endpoint so an operator can rotate IPs on demand.

### Config

See [`docs/configuration.md`](configuration.md#provider--webshare) for the full key reference. Minimum config:

```toml
[[upstream_pool]]
provider       = "webshare"
id_prefix      = "ws"
api_token_file = "/etc/tunnelsmith/webshare.token"
```

Defaults (`mode = "direct"`, `kind = "http"`, `country_codes = []`, `refresh = "12h"`) match the most common Webshare deployment.

### Tokens

Webshare's API token is account-wide. Two options:

- **`api_token`** — inline in the TOML. Cleartext in `--print-config` and in any logs that dump the resolved config. Use only for throwaway tokens or short-lived debugging.
- **`api_token_file`** — absolute path to a one-token-per-file file (whitespace and a trailing newline are stripped). Recommended for production. The file must exist at startup; SIGHUP re-reads it.

### Vendor API

When the Webshare provider is configured, `POST /v1/providers/{id_prefix}/refresh` on the control endpoint forwards to `POST /api/v2/proxy/list/refresh/`. See [`docs/control-api.md`](control-api.md) for the route shape, status codes, and auth requirements.

---

## How the abstraction works

`config.Parse` builds a `[]config.UpstreamPoolConfig`, one entry per `[[upstream_pool]]` block. For each block:

1. **Validation** — `config.Validate` calls into a registry-backed validator that delegates to the provider's `ValidateConfig`. An unknown `provider` value fails fast with the list of registered names; provider-specific rules (token presence, country format, mode/kind enum) are checked here.
2. **Startup expansion** — `cmd/tunnelsmith` looks up the provider in `provider.Default()`, calls `BuildExpander` plus `BuildAPI`, then `Snapshot` to seed the priority pool.
3. **Refresh ticker** — the expander's `RunRefresh` runs in a goroutine and hands every successful snapshot to the pool composer, which hot-swaps the running `*upstream.Pool` (Phase 11.1).
4. **Control endpoint** — providers whose `BuildAPI` returned a non-nil `API` register a binding with the control listener. `POST /v1/providers/{id_prefix}/refresh` dispatches through the binding.

Three files own the abstraction:

| File | What it owns |
|------|---------------|
| `internal/upstream/provider/provider.go` | The `Provider`, `Expander`, and `API` interfaces plus `ErrAPINotSupported` |
| `internal/upstream/provider/registry.go` | The `Registry` type and the `provider.Default()` global |
| `internal/upstream/providers/providers.go` | Blank imports every provider package so their `init()` runs, plus binds `config.SetProviderValidator` |

---

## Adding a new provider

The whole change fits in one new package plus a one-line blank import. The Webshare provider's commit history is the canonical worked example.

### 1. Fork the repository

```sh
gh repo fork pacnpal/tunnelsmith --clone --remote
cd tunnelsmith
git checkout -b feat/provider-<your-vendor-name>
```

You will work entirely on your fork until the final PR step.

### 2. Create the package

```sh
mkdir -p internal/upstream/<name>
```

Use lowercase, ASCII-only, no whitespace. The directory name **must** match the `Name()` return value below.

A minimal provider needs four files:

- `api.go` — vendor REST client (or whatever wire protocol the vendor speaks).
- `expander.go` — turns a vendor response into `[]config.UpstreamConfig`.
- `provider.go` — implements `provider.Provider`; registers itself via `init()`.
- `provider_test.go` + `expander_test.go` + `api_test.go` — unit tests covering happy path, auth failure, pagination/quirks, and config validation.

### 3. Implement `provider.Provider`

```go
package <name>

import (
    "context"
    "errors"
    "log/slog"

    "github.com/pacnpal/tunnelsmith/internal/config"
    "github.com/pacnpal/tunnelsmith/internal/upstream/provider"
)

const ProviderName = "<name>"

type Provider struct{}

func NewProvider() *Provider { return &Provider{} }
func (p *Provider) Name() string { return ProviderName }

func (p *Provider) ValidateConfig(cfg config.UpstreamPoolConfig) error {
    // Reject what your provider cannot handle. Return *all* invariant
    // violations through one error per call so the operator sees them
    // at config-load time.
    if cfg.APIToken == "" && cfg.APITokenFile == "" {
        return errors.New("<name>: one of api_token or api_token_file is required")
    }
    return nil
}

func (p *Provider) BuildExpander(cfg config.UpstreamPoolConfig, logger *slog.Logger) (provider.Expander, error) {
    // ... build your client + expander.
    return nil, errors.New("not yet implemented")
}

func (p *Provider) BuildAPI(cfg config.UpstreamPoolConfig, logger *slog.Logger) (provider.API, error) {
    // Return ErrAPINotSupported if your vendor has no operator-callable API.
    return nil, provider.ErrAPINotSupported
}

func init() { provider.MustRegister(NewProvider()) }
```

### 4. Implement `provider.Expander`

The expander is the path the priority pool reads:

```go
type Expander struct { /* ... */ }

func (e *Expander) Snapshot(ctx context.Context) ([]config.UpstreamConfig, error) {
    // Fetch from your vendor, transform each entry into a
    // config.UpstreamConfig (with Kind, Addr, Priority, and optional
    // Username/Password), and return a slice sorted by upstream id.
}

func (e *Expander) RunRefresh(
    ctx context.Context,
    prev []config.UpstreamConfig,
    onChange func(prev, next []config.UpstreamConfig),
) error {
    // Tick on e.cfg.Refresh; call onChange(prev, next) on every
    // successful re-fetch. Return nil on ctx.Done() or when refresh <= 0.
}
```

Determinism matters: the pool composer compares snapshots with `reflect.DeepEqual` and short-circuits a no-op tick. **Sort the slice by id**, treat `nil` and an empty slice consistently, and reuse pointer values (`*int` priority) across the slice — copy the existing Mullvad and Webshare expanders.

### 5. (Optional) Implement `provider.API`

If your vendor has an operator-callable surface (rotate IPs, regenerate the pool, validate a key), implement it here:

```go
type apiAdapter struct {
    client *Client
    planID string
}

func (a *apiAdapter) RefreshProxyList(ctx context.Context, opts provider.RefreshOptions) error {
    planID := opts.PlanID
    if planID == "" {
        planID = a.planID
    }
    return a.client.RefreshProxyList(ctx, planID)
}
```

Return it from `Provider.BuildAPI`. The control endpoint route `POST /v1/providers/{id_prefix}/refresh` will pick it up automatically; you do not edit any control-package code.

If you need new control-API routes (`/v1/providers/{name}/profile`, `/v1/providers/{name}/subscription`), extend `provider.API` with the new method and update `internal/control/providers.go` in the same PR — the interface lives in `internal/upstream/provider` precisely so adding capabilities is a single-file change.

### 6. Register the provider package

Add a blank import to [`internal/upstream/providers/providers.go`](../internal/upstream/providers/providers.go):

```go
import (
    _ "github.com/pacnpal/tunnelsmith/internal/upstream/mullvad"
    _ "github.com/pacnpal/tunnelsmith/internal/upstream/webshare"
    _ "github.com/pacnpal/tunnelsmith/internal/upstream/<name>" // <-- your provider
)
```

This is the single line that wires the binary to your provider. `cmd/tunnelsmith` already imports the `providers` package via blank import, so nothing else changes.

### 7. Write tests

Every in-tree provider has the same test surface:

- **`api_test.go`** — `httptest.NewServer` driving the vendor wire protocol. Cover: auth header sent, happy path, 401/403/429 surface as typed errors, pagination, response-size cap, refresh round-trip.
- **`expander_test.go`** — pass a stubbed Client, assert the transformation produces deterministic, sorted, `UpstreamConfig` slices. Cover: invalid proxies skipped, kind/mode selection, country filter, id-prefix prepending, priority threading.
- **`provider_test.go`** — exercise `ValidateConfig`, `BuildExpander`, `BuildAPI`, plus a small E2E that round-trips a fake API server through `Snapshot` to prove the credentials are threaded into the resulting `UpstreamConfig`.
- **`e2e_test.go`** — full integration: fake vendor API + auth-required upstream proxy + Tunnelsmith listener + destination. Prove a CONNECT round-trips with the right auth header. The Webshare `e2e_test.go` is the template.

Aim for the same coverage Webshare ships with. PRs that drop the E2E test will be asked to add one.

### 8. Update the docs

Required:

- **[`docs/configuration.md`](configuration.md)** — add a `provider = "<name>"` subsection under `[[upstream_pool]]` with the full key reference, defaults, and an example block.
- **[`docs/providers.md`](providers.md)** — add a top-level section describing what your provider does, how the operator gets a token, and any vendor quirks.
- **[`docs/control-api.md`](control-api.md)** — if you added `provider.API` routes, document them here.
- **[`docs/deployment.md`](deployment.md)** — add a "Use with `<vendor>`" section with a compose snippet if there's a recommended deployment topology.
- **[`README.md`](../README.md)** — add your provider to the supported-providers list.
- **[`CHANGELOG.md`](../CHANGELOG.md)** — note the new provider under the next release.

Optional but appreciated:

- **ADR** in [`docs/decisions.md`](decisions.md) — record any non-obvious design choices (port selection, address transformation, etc.). Mullvad's ADR-004 is the model.

### 9. Lint and test

```sh
make build
make test
make lint
```

The CI workflow runs all three and a few golangci-lint checks; make sure they're clean locally before pushing.

### 10. Submit the PR

```sh
git push --set-upstream origin feat/provider-<your-vendor-name>
gh pr create \
    --repo pacnpal/tunnelsmith \
    --base main \
    --head <your-github-username>:feat/provider-<your-vendor-name> \
    --title "feat: add <vendor> upstream-pool provider" \
    --body-file - <<'EOF'
## What

Adds a new `[[upstream_pool]]` provider for <vendor>.

## Why

<one paragraph: what the provider unlocks, who the user is, what the trade-offs are versus existing providers>

## How

- New package `internal/upstream/<name>` with `Provider`, `Expander`, and (optional) `API` implementations.
- Registered in `internal/upstream/providers/providers.go`.
- Documented in `docs/configuration.md` and `docs/providers.md`.

## Test plan

- [x] Unit tests for client, expander, provider validation, and (where applicable) API.
- [x] End-to-end test that drives a CONNECT through a fake vendor proxy and asserts the response from the destination.
- [x] `make build`, `make test`, `make lint` clean locally.

## Out of scope

<anything you deliberately did not add — auth on additional routes, plan-id discovery, etc.>
EOF
```

### Review expectations

PRs that follow the structure of the in-tree Mullvad and Webshare providers merge quickly. Common ask-for-changes items:

- **No vendored SDKs.** Tunnelsmith uses `net/http` directly. A vendor SDK pulls extra dependencies, complicates supply-chain review, and rarely matches the small surface we actually need.
- **Bound every response read.** Use `io.LimitReader(body, maxBodyBytes+1)` and reject oversize bodies explicitly. Unbounded `io.ReadAll` is grounds for a request-changes.
- **Bound pagination.** The `maxResponsePages` guard in Webshare is the template — set a hard cap on how many pages your client follows.
- **Surface typed errors for the auth and rate-limit cases.** `ErrUnauthorized` / `ErrForbidden` / `ErrRateLimited` are the names already in use; reuse them for consistency or add your own that wrap them.
- **Sort the snapshot.** A non-deterministic snapshot order spuriously fails the pool composer's no-op short-circuit, which results in pool churn on every refresh tick.
- **Don't log secrets.** API tokens, usernames, and passwords must not appear in any log line. The Webshare provider's logger field is `slog.Logger` so structured log review can grep for known secret-shaped fields.

If you're not sure whether something is in scope, open a draft PR early and ask.
