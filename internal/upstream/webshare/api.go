// Package webshare integrates Webshare's proxy API as a Tunnelsmith
// [[upstream_pool]] provider. Webshare exposes a paginated list of HTTP
// proxies behind a token-authenticated REST API at
// https://proxy.webshare.io/api/v2/. The Expander fans the list into
// one config.UpstreamConfig per proxy; the API surface forwards
// operator-triggered POST /api/v2/proxy/list/refresh through Tunnelsmith's
// control endpoint.
//
// Authentication is token-style: Authorization: Token <key>. The package
// reads the token from cfg.APIToken or cfg.APITokenFile (absolute path,
// one token per file). A token gives full read access to the account's
// proxy list, so the token file should sit on a private path.
//
// Webshare's API returns paginated JSON with predictable error codes
// (https://apidocs.webshare.io/). Pagination is followed automatically
// using the "next" cursor. Two independent guards bound memory: at
// most maxResponsePages pages are followed (500 by default) and each
// page response body is capped at maxBodyBytes (4 MiB by default).
// The per-page entry count is whatever Webshare returns; pageSize is
// a request hint, not a server-enforced ceiling.
//
// Wire-protocol references (Webshare apidocs):
//
//	GET  /api/v2/profile/                — validates the API key
//	GET  /api/v2/proxy/list/?mode=direct  — paginated proxy list
//	POST /api/v2/proxy/list/refresh/      — on-demand list refresh
package webshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// BaseURL is Webshare's production REST endpoint. Tests override this
// to point at an httptest server.
const BaseURL = "https://proxy.webshare.io/api/v2"

// DefaultPageSize matches Webshare's documented default. Setting it
// explicitly makes the client's behavior independent of whatever the
// server defaults to today.
const DefaultPageSize = 100

// maxResponsePages caps how many pages ListProxies follows before
// returning an error. With page_size=100 this allows up to 50,000
// proxies — far more than any realistic plan and a hard ceiling so a
// pathological server response cannot pin unbounded memory.
const maxResponsePages = 500

// maxBodyBytes caps a single HTTP response body. Webshare's proxy list
// pages are a few KB; 4 MiB is orders of magnitude of headroom so a
// hostile/misconfigured server cannot OOM the binary.
const maxBodyBytes = 4 * 1024 * 1024

// ErrUnauthorized is returned when Webshare answers 401. Surfacing this
// as a distinct error lets the control endpoint and operator logs say
// "your token is wrong" instead of a generic upstream failure.
var ErrUnauthorized = errors.New("webshare: unauthorized (check api_token)")

// ErrForbidden is returned when Webshare answers 403. Usually means the
// API key is correct but the plan does not allow the requested action
// (for example, an on-demand refresh when on_demand_refreshes_available
// is zero).
var ErrForbidden = errors.New("webshare: forbidden")

// ErrRateLimited is returned when Webshare answers 429. The caller
// should back off and try again later. The apiAdapter in provider.go
// wraps this with provider.ErrAPIRateLimited so the control listener's
// generic dispatcher can map it to HTTP 429 without importing this
// package.
var ErrRateLimited = errors.New("webshare: rate limited")

// HTTPStatusError is returned by Client.do for any response whose
// status code wasn't in the recognised set (2xx success, 204 success,
// 401/403/429 mapped to their typed sentinels). The wrapped
// StatusCode lets ListProxies' cache-fallback policy distinguish a
// 5xx (transient — fall back to disk so the running pool keeps
// serving) from a 4xx (client/config error — propagate so the
// operator sees the misconfig instead of stale data masking it).
type HTTPStatusError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string // truncated preview of the response body
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("webshare: %s %s: unexpected status %d: %s",
		e.Method, e.Path, e.StatusCode, e.Body)
}

// Proxy is a single entry in Webshare's proxy list. Field names track
// the JSON shape so the json package handles decoding without custom
// hooks. Comments here document which fields the expander actually
// uses; the rest are kept so a future feature (filtering, logging) has
// access without a schema change.
type Proxy struct {
	ID               string    `json:"id"`
	Username         string    `json:"username"`
	Password         string    `json:"password"`
	ProxyAddress     string    `json:"proxy_address"`
	Port             int       `json:"port"`
	Valid            bool      `json:"valid"`
	LastVerification time.Time `json:"last_verification,omitempty"`
	CountryCode      string    `json:"country_code"`
	CityName         string    `json:"city_name"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
}

// HostPort returns the proxy's "host:port" form. Centralised so a
// future migration to a different address field has one update site.
func (p Proxy) HostPort() string {
	return p.ProxyAddress + ":" + strconv.Itoa(p.Port)
}

// Profile mirrors GET /api/v2/profile/ — the minimum subset that proves
// the API key is valid. Extra fields the API may add are preserved as
// raw JSON in the Raw field so log lines / future features can pull
// them without a schema bump.
type Profile struct {
	ID    int    `json:"id"`
	Email string `json:"email"`
	Raw   json.RawMessage
}

// ListProxiesOptions controls pagination, filtering, and mode for the
// proxy-list endpoint. PageSize and Page both have defaults; Mode is
// required and defaults to "direct" when zero.
type ListProxiesOptions struct {
	Mode         string   // "direct" (default) or "backbone"
	PlanID       string   // optional
	CountryCodes []string // ISO-3166-1 alpha-2; joined with ","
	Search       string   // optional, ignored in backbone mode
	PageSize     int      // 0 = DefaultPageSize
}

// Client talks to Webshare's REST API. Construct one with NewClient,
// override BaseURL/HTTPClient/Cache/Logger for tests, and call the
// method that maps to the endpoint you need.
type Client struct {
	BaseURL    string
	APIToken   string
	HTTPClient *http.Client
	Cache      *Cache       // optional on-disk fallback for ListProxies
	Logger     *slog.Logger // optional; cache-write warnings go here
}

// NewClient returns a Client wired to the production endpoint. The
// caller must set APIToken (and may override BaseURL/HTTPClient/Cache/
// Logger) before any request method runs.
func NewClient() *Client {
	return &Client{
		BaseURL:    BaseURL,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// LoadTokenFile reads a single token from path. Outer whitespace and a
// trailing newline are stripped; empty files return an explicit error.
// Files containing more than one whitespace-separated field are also
// rejected: a file with an accidental second line (logrotate footer,
// editor scratch) would otherwise become a single "token\nnoise"
// Authorization header that Webshare quietly rejects at request time,
// which is harder to diagnose than the load-time error this guard
// produces.
//
// The path must be absolute (config.Validate enforces this) so a
// relative path in development cannot silently resolve under a CWD the
// operator did not intend.
func LoadTokenFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("webshare: api_token_file %q must be absolute", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("webshare: read api_token_file: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("webshare: api_token_file %q is empty", path)
	}
	if len(strings.Fields(tok)) != 1 {
		return "", fmt.Errorf("webshare: api_token_file %q must contain exactly one token (no embedded whitespace or extra lines)", path)
	}
	return tok, nil
}

// Profile fetches the current account's profile. Useful as a "ping"
// from the control endpoint to verify the API key is valid before any
// list operation.
func (c *Client) Profile(ctx context.Context) (*Profile, error) {
	body, err := c.do(ctx, http.MethodGet, "/profile/", nil)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("webshare: decode profile: %w", err)
	}
	p.Raw = body
	return &p, nil
}

// ListProxies follows pagination and returns the full list in one
// slice. The Cache, if configured, is written on success so a later
// network outage can still produce a usable list; cached reads only
// happen when the live fetch fails AND the failure is transient.
//
// Auth-class failures (401, 403) and rate-limit (429) deliberately
// do NOT fall back to the cache: a revoked token, an expired plan,
// or a quota cap should not be silently masked by a stale list,
// because the operator would keep dialing through proxies the vendor
// no longer authorises. Network-class failures (DNS, TCP, timeout,
// 5xx) still fall back so a transient API hiccup doesn't drop the
// running pool.
func (c *Client) ListProxies(ctx context.Context, opts ListProxiesOptions) ([]Proxy, error) {
	out, fetchErr := c.listProxiesOnline(ctx, opts)
	if fetchErr == nil {
		if c.Cache != nil {
			if err := c.Cache.Write(out); err != nil && c.Logger != nil {
				c.Logger.Warn("webshare: proxy-list cache write failed",
					slog.String("path", c.Cache.Path),
					slog.Any("err", err),
				)
			}
		}
		return out, nil
	}
	if cacheFallbackAllowed(fetchErr) {
		if cached, ok := c.tryCacheFallback(); ok {
			return cached, nil
		}
	}
	return nil, fetchErr
}

// cacheFallbackAllowed reports whether a stale cached list is a safe
// substitute for the failed live fetch.
//
//   - Auth (401) / forbidden (403) / rate-limit (429): never fall back.
//     These represent a vendor-side intentional reject the operator
//     should see, not paper over with stale data.
//   - Other 4xx (e.g. 400 from a bad plan_id): never fall back.
//     Client-side misconfig is operator-actionable and masking it with
//     yesterday's cached list keeps the binary serving against a
//     mistake the operator can fix today.
//   - 5xx: fall back. Server-side failures are usually transient; a
//     stale list is better than dropping the running pool.
//   - Transport errors (DNS, TCP, timeout): fall back for the same
//     reason — they're transient and unrelated to operator config.
func cacheFallbackAllowed(err error) bool {
	switch {
	case errors.Is(err, ErrUnauthorized):
		return false
	case errors.Is(err, ErrForbidden):
		return false
	case errors.Is(err, ErrRateLimited):
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		// Any 4xx is operator-actionable; only 5xx counts as
		// transient. The 401/403/429 cases were already caught
		// above via the typed sentinels.
		return statusErr.StatusCode >= 500
	}
	return true
}

// listProxiesOnline performs the actual paginated walk. Separate so
// the cache fallback in ListProxies can call this exactly once.
func (c *Client) listProxiesOnline(ctx context.Context, opts ListProxiesOptions) ([]Proxy, error) {
	mode := opts.Mode
	if mode == "" {
		mode = "direct"
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}

	q := url.Values{}
	q.Set("mode", mode)
	q.Set("page_size", strconv.Itoa(pageSize))
	if opts.PlanID != "" {
		q.Set("plan_id", opts.PlanID)
	}
	if len(opts.CountryCodes) > 0 {
		// Normalize to uppercase before sending. Webshare's API
		// matches ISO codes case-sensitively in some plan flavours;
		// the provider already validated that each entry is two
		// ASCII letters, so ToUpper is safe and deterministic.
		upper := make([]string, len(opts.CountryCodes))
		for i, c := range opts.CountryCodes {
			upper[i] = strings.ToUpper(c)
		}
		q.Set("country_code__in", strings.Join(upper, ","))
	}
	if opts.Search != "" && mode == "direct" {
		// Search is documented as ignored in backbone mode. Pass it
		// through only when it would actually be honored to avoid
		// generating a query that silently differs from what the API
		// receives.
		q.Set("search", opts.Search)
	}

	path := "/proxy/list/?" + q.Encode()
	var all []Proxy
	for page := 1; page <= maxResponsePages; page++ {
		body, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, fmt.Errorf("webshare: list page %d: %w", page, err)
		}
		var resp struct {
			Count    int     `json:"count"`
			Next     string  `json:"next"`
			Previous string  `json:"previous"`
			Results  []Proxy `json:"results"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("webshare: decode list page %d: %w", page, err)
		}
		all = append(all, resp.Results...)
		if resp.Next == "" {
			return all, nil
		}
		// Webshare's "next" is an absolute URL. Trust the path portion
		// only; the host comes from c.BaseURL so a hostile (or
		// httptest-rewritten) Next field cannot redirect to a third
		// party. Same reasoning the cache lookup uses below.
		next, err := url.Parse(resp.Next)
		if err != nil {
			return nil, fmt.Errorf("webshare: parse next %q: %w", resp.Next, err)
		}
		// Strip the documented "/api/v2/" prefix so the rebuilt URL
		// matches the BaseURL+path convention used by c.do.
		//
		// Reject any path that doesn't start with the prefix —
		// in production Webshare's "next" cursor is always rooted
		// under /api/v2/. A substring search like
		// strings.Index(nextPath, "/api/v2") would also fire on
		// "/api/v2evil/..." and strip the leading characters away,
		// leaving "evil/..." which c.do would then prepend the
		// BaseURL to. That gives a hostile or malformed "next" the
		// ability to silently rewrite the request URL away from
		// /api/v2. Anchoring on "/api/v2/" and refusing anything
		// else closes that footgun.
		const apiPrefix = "/api/v2/"
		if !strings.HasPrefix(next.Path, apiPrefix) {
			return nil, fmt.Errorf("webshare: next path %q must be absolute and under %q", next.Path, apiPrefix)
		}
		// Keep one leading slash so c.do's `BaseURL+path`
		// concatenation forms a clean URL.
		path = "/" + next.Path[len(apiPrefix):]
		if next.RawQuery != "" {
			path += "?" + next.RawQuery
		}
	}
	return nil, fmt.Errorf("webshare: list exceeded %d pages (likely runaway server response)", maxResponsePages)
}

// RefreshProxyList triggers the on-demand list refresh. planID is
// optional ("" = use the account's default plan). The endpoint returns
// 204 No Content on success.
func (c *Client) RefreshProxyList(ctx context.Context, planID string) error {
	path := "/proxy/list/refresh/"
	if planID != "" {
		path += "?plan_id=" + url.QueryEscape(planID)
	}
	_, err := c.do(ctx, http.MethodPost, path, nil)
	return err
}

// do is the shared request executor. method is "GET"/"POST"; path is
// the URL path after BaseURL. body is the optional request body
// (typically nil for Webshare's GET/POST-empty endpoints). Returns the
// full response body, capped at maxBodyBytes.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	if c.APIToken == "" {
		return nil, fmt.Errorf("webshare: APIToken is empty")
	}
	base := c.BaseURL
	if base == "" {
		base = BaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+c.APIToken)
	req.Header.Set("Accept", "application/json")
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webshare: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	limited := io.LimitReader(resp.Body, maxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("webshare: read body: %w", err)
	}
	if int64(len(data)) > maxBodyBytes {
		return nil, fmt.Errorf("webshare: response body exceeds %d bytes", maxBodyBytes)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return data, nil
	case http.StatusNoContent:
		return nil, nil
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case http.StatusForbidden:
		return nil, ErrForbidden
	case http.StatusTooManyRequests:
		return nil, ErrRateLimited
	default:
		return nil, &HTTPStatusError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       snippet(data),
		}
	}
}

// snippet returns a short preview of a response body for error
// messages. Long bodies (JSON dumps from server-side errors) are
// truncated so an error log line stays readable.
func snippet(b []byte) string {
	const max = 256
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

func (c *Client) tryCacheFallback() ([]Proxy, bool) {
	if c.Cache == nil {
		return nil, false
	}
	cached, err := c.Cache.Read()
	if err != nil {
		return nil, false
	}
	return cached, true
}
