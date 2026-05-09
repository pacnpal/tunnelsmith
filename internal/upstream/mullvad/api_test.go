package mullvad

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseResponseGoldenFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "relays.json"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	relays, err := parseResponse(data)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	want := []Relay{
		{Hostname: "au-syd-wg-001", Country: "Australia", City: "Sydney", Active: true},
		{Hostname: "au-syd-wg-302", Country: "Australia", City: "Sydney", Active: false},
		{Hostname: "nl-ams-wg-001", Country: "Netherlands", City: "Amsterdam", Active: true},
		{Hostname: "se-sto-wg-001", Country: "Sweden", City: "Stockholm", Active: true},
		{Hostname: "se-sto-wg-002", Country: "Sweden", City: "Stockholm", Active: true},
		{Hostname: "us-nyc-wg-301", Country: "USA", City: "New York, NY", Active: true},
	}
	if !reflect.DeepEqual(relays, want) {
		t.Fatalf("parseResponse mismatch.\n got:  %#v\n want: %#v", relays, want)
	}
}

func TestParseResponseInvalidJSON(t *testing.T) {
	_, err := parseResponse([]byte("not json"))
	if err == nil {
		t.Fatal("expected error on garbage input, got nil")
	}
}

func TestParseResponseAllowsMissingLocation(t *testing.T) {
	// A relay that points at a location code the API did not include is
	// still returned, with empty country and city. The locations map is the
	// source of truth for metadata; an absent key is treated as "no metadata
	// available" rather than as a hard error, so a single rare orphaned
	// relay does not break the entire fetch.
	body := []byte(`{
		"locations": {"se-sto": {"country": "Sweden", "city": "Stockholm"}},
		"wireguard": {
			"relays": [
				{"hostname": "se-sto-wg-001", "location": "se-sto", "active": true},
				{"hostname": "xx-xxx-wg-001", "location": "xx-xxx", "active": true}
			]
		}
	}`)
	got, err := parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 relays, got %d", len(got))
	}
	if got[1].Country != "" || got[1].City != "" {
		t.Fatalf("want empty country/city for unknown location, got %+v", got[1])
	}
}

func TestClientFetchHappyPath(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "relays.json"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, HTTPClient: srv.Client()}
	relays, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(relays) != 6 {
		t.Fatalf("want 6 relays, got %d", len(relays))
	}
}

func TestClientFetchFallsBackToCache(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "relays.json"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "relays.json")
	if err := os.WriteFile(cachePath, body, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	c := &Client{URL: srv.URL, HTTPClient: srv.Client(), Cache: &Cache{Path: cachePath}}
	relays, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(relays) != 6 {
		t.Fatalf("want 6 relays from cache, got %d", len(relays))
	}
}

func TestClientFetchWithoutCacheReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, HTTPClient: srv.Client()}
	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error when API fails and no cache configured")
	}
}

func TestClientFetchWritesCacheOnSuccess(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "relays.json"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "relays.json")
	c := &Client{URL: srv.URL, HTTPClient: srv.Client(), Cache: &Cache{Path: cachePath}}
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
}

func TestClientFetchLogsCacheWriteFailure(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "relays.json"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Point the cache at a path inside a regular file so MkdirAll fails and
	// Cache.Write returns an error. Fetch must still succeed (the live
	// fetch worked) but the failure must be logged through the wired
	// logger so an operator can see the misconfiguration.
	regularFile := filepath.Join(t.TempDir(), "blocking-file")
	if err := os.WriteFile(regularFile, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}
	cachePath := filepath.Join(regularFile, "cache.json")

	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := &Client{
		URL:        srv.URL,
		HTTPClient: srv.Client(),
		Cache:      &Cache{Path: cachePath},
		Logger:     logger,
	}
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v (cache write failure must not fail the live fetch)", err)
	}
	logged := sb.String()
	if !strings.Contains(logged, "cache write failed") {
		t.Fatalf("expected log line to mention cache write failure, got: %q", logged)
	}
}

func TestCacheReadEmptyReturnsErrNoCache(t *testing.T) {
	c := &Cache{Path: filepath.Join(t.TempDir(), "missing.json")}
	_, err := c.Read()
	if !errors.Is(err, ErrNoCache) {
		t.Fatalf("expected ErrNoCache, got %v", err)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	c := &Cache{Path: filepath.Join(t.TempDir(), "subdir", "relays.json")}
	want := []byte(`{"hello":"world"}`)
	if err := c.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := c.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-trip mismatch: got %q want %q", string(got), string(want))
	}
}
