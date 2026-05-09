// Package mullvad fetches Mullvad's WireGuard relay list and exposes
// per-server SOCKS5 proxy entries that Tunnelsmith can mount as upstreams.
//
// The schema and hostname pattern are documented in ADR-004.
package mullvad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RelaysURL is Mullvad's public WireGuard relay list endpoint. No auth
// required; the response is a few hundred KB and changes slowly.
const RelaysURL = "https://api.mullvad.net/public/relays/wireguard/v2/"

// Relay is one Mullvad WireGuard relay after the API response has been
// joined with its location metadata. The SOCKS5 endpoint is derived from
// Hostname via SOCKS5Address per ADR-004.
type Relay struct {
	Hostname string
	Country  string
	City     string
	Active   bool
}

// Client fetches and parses the Mullvad relay list. Construct one with
// NewClient and call Fetch. If Cache is non-nil, successful responses are
// written to disk and read back when the API is unreachable. If Logger is
// non-nil, cache-write failures (which are otherwise non-fatal) are logged
// at WARN so a misconfigured cache_path surfaces before a real outage.
type Client struct {
	URL        string
	HTTPClient *http.Client
	Cache      *Cache
	Logger     *slog.Logger
}

// NewClient returns a Client wired to the production endpoint and a small
// HTTP timeout. Override URL, HTTPClient, Cache, or Logger on the returned
// struct for tests or to surface cache write failures to operators.
func NewClient() *Client {
	return &Client{
		URL:        RelaysURL,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Fetch returns the current relay list. On HTTP failure it falls back to
// the disk cache when one is configured; if both fail the network error
// is returned.
func (c *Client) Fetch(ctx context.Context) ([]Relay, error) {
	body, fetchErr := c.fetchOnline(ctx)
	if fetchErr == nil {
		relays, parseErr := parseResponse(body)
		if parseErr != nil {
			if cached, ok := c.tryCacheFallback(); ok {
				return cached, nil
			}
			return nil, parseErr
		}
		if c.Cache != nil {
			if err := c.Cache.Write(body); err != nil {
				// Cache writes are best-effort: a failed write should not
				// fail the live request. Surface the failure through the
				// optional logger so a misconfigured cache_path is visible
				// before an outage exposes it.
				if c.Logger != nil {
					c.Logger.Warn("mullvad: relay-list cache write failed",
						slog.String("path", c.Cache.Path),
						slog.Any("err", err),
					)
				}
			}
		}
		return relays, nil
	}
	if cached, ok := c.tryCacheFallback(); ok {
		return cached, nil
	}
	return nil, fetchErr
}

func (c *Client) tryCacheFallback() ([]Relay, bool) {
	if c.Cache == nil {
		return nil, false
	}
	cached, err := c.Cache.Read()
	if err != nil {
		return nil, false
	}
	relays, err := parseResponse(cached)
	if err != nil {
		return nil, false
	}
	return relays, true
}

func (c *Client) fetchOnline(ctx context.Context) ([]byte, error) {
	url := c.URL
	if url == "" {
		url = RelaysURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mullvad: get relays: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mullvad: get relays: unexpected status %d", resp.StatusCode)
	}
	// Cap the response body so a runaway server (or a hostile redirect that
	// somehow bypasses TLS verification) cannot OOM the binary. Mullvad's
	// real response is around 158 KB at the time of this code; 8 MiB is
	// orders of magnitude more headroom than we expect to ever need.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("mullvad: read relays body: %w", err)
	}
	return body, nil
}

// maxResponseBytes caps the relay-list body. See fetchOnline for rationale.
const maxResponseBytes = 8 * 1024 * 1024

type rawResponse struct {
	Locations map[string]rawLocation `json:"locations"`
	WireGuard struct {
		Relays []rawRelay `json:"relays"`
	} `json:"wireguard"`
}

type rawLocation struct {
	Country string `json:"country"`
	City    string `json:"city"`
}

type rawRelay struct {
	Hostname string `json:"hostname"`
	Location string `json:"location"`
	Active   bool   `json:"active"`
}

// parseResponse decodes Mullvad's API JSON and joins each WG relay against
// the locations map so the returned Relay slice carries country and city.
// Output is sorted by hostname for stability.
func parseResponse(data []byte) ([]Relay, error) {
	var raw rawResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mullvad: decode relay list: %w", err)
	}
	out := make([]Relay, 0, len(raw.WireGuard.Relays))
	for _, r := range raw.WireGuard.Relays {
		loc := raw.Locations[r.Location]
		out = append(out, Relay{
			Hostname: r.Hostname,
			Country:  loc.Country,
			City:     loc.City,
			Active:   r.Active,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

// Cache persists the raw API response to a file so the relay list survives
// transient API outages. The body is stored verbatim; parsing is redone on
// read so a schema change is caught the same way as for a live response.
type Cache struct {
	Path string
}

// ErrNoCache is returned when Cache.Read is called against a cache that
// has never been populated.
var ErrNoCache = errors.New("mullvad: cache empty")

// Read returns the raw JSON body that was last written to the cache.
func (c *Cache) Read() ([]byte, error) {
	if c == nil || c.Path == "" {
		return nil, ErrNoCache
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoCache
		}
		return nil, err
	}
	return data, nil
}

// Write persists the raw JSON body to disk via a tmp-and-rename so an
// interrupted write cannot leave a half-written cache file behind.
//
// The temp file is created with a unique suffix via os.CreateTemp so two
// concurrent writers cannot race on the same tmp path. The deferred Remove
// is a no-op once the rename has succeeded (the file no longer exists at
// the tmp path).
func (c *Cache) Write(data []byte) error {
	if c == nil || c.Path == "" {
		return ErrNoCache
	}
	dir := filepath.Dir(c.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(c.Path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, c.Path)
}
