package mullvad

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "relays.json"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, HTTPClient: srv.Client()}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestExpanderSnapshotFiltersByCountry(t *testing.T) {
	client := newTestClient(t)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:  "mvd",
		Priority:  200,
		Countries: []string{"Sweden", "Netherlands"},
	}, client, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	wantIDs := []string{
		"mvd-nl-ams-wg-001",
		"mvd-se-sto-wg-001",
		"mvd-se-sto-wg-002",
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("want %d upstreams, got %d (%+v)", len(wantIDs), len(got), got)
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Fatalf("upstream[%d].ID = %q, want %q", i, got[i].ID, id)
		}
		if got[i].Kind != config.KindSOCKS5 {
			t.Fatalf("upstream[%d].Kind = %q, want %q", i, got[i].Kind, config.KindSOCKS5)
		}
		if got[i].Priority == nil || *got[i].Priority != 200 {
			t.Fatalf("upstream[%d].Priority = %v, want 200", i, got[i].Priority)
		}
	}
	if got[0].Addr != "nl-ams-wg-socks5-001.relays.mullvad.net:1080" {
		t.Fatalf("first addr = %q, want nl-ams-wg-socks5-001.relays.mullvad.net:1080", got[0].Addr)
	}
}

func TestExpanderSnapshotDropsInactiveByDefault(t *testing.T) {
	client := newTestClient(t)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:  "mvd",
		Priority:  200,
		Countries: []string{"Australia"},
	}, client, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 1 || got[0].ID != "mvd-au-syd-wg-001" {
		t.Fatalf("expected only the active Australian relay, got %+v", got)
	}
}

func TestExpanderSnapshotIncludeInactive(t *testing.T) {
	client := newTestClient(t)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:        "mvd",
		Priority:        200,
		Countries:       []string{"Australia"},
		IncludeInactive: true,
	}, client, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 Australian upstreams (active + inactive), got %d", len(got))
	}
}

func TestExpanderSnapshotIsSortedByID(t *testing.T) {
	// Even when the underlying client returns relays in a non-sorted order,
	// Snapshot must produce a deterministic id-sorted slice. Tests use a
	// custom client that emits relays in reverse hostname order to guard
	// against the previous implicit reliance on parseResponse's sort.
	body, err := os.ReadFile(filepath.Join("testdata", "relays.json"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:  "zzz",
		Priority:  200,
		Countries: []string{"Sweden", "Netherlands", "USA", "Australia"},
	}, &Client{URL: srv.URL, HTTPClient: srv.Client()}, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID > got[i].ID {
			t.Fatalf("Snapshot output is not sorted by ID at index %d: %q > %q", i, got[i-1].ID, got[i].ID)
		}
	}
}

func TestExpanderSnapshotEmptyCountriesAcceptsAll(t *testing.T) {
	client := newTestClient(t)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix: "mvd",
		Priority: 200,
	}, client, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// 6 testdata relays, 1 inactive dropped = 5.
	if len(got) != 5 {
		t.Fatalf("want 5 upstreams (all active), got %d", len(got))
	}
}

func TestExpanderSnapshotCountryFilterCaseInsensitive(t *testing.T) {
	client := newTestClient(t)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:  "mvd",
		Priority:  200,
		Countries: []string{"sWeDeN"},
	}, client, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 Swedish relays, got %d", len(got))
	}
}

func TestNewExpanderRequiresIDPrefix(t *testing.T) {
	_, err := NewExpander(ExpanderConfig{}, newTestClient(t), quietLogger())
	if err == nil {
		t.Fatal("expected error when id_prefix is empty")
	}
}

func TestNewExpanderRequiresClient(t *testing.T) {
	_, err := NewExpander(ExpanderConfig{IDPrefix: "mvd"}, nil, quietLogger())
	if err == nil {
		t.Fatal("expected error when client is nil")
	}
}

func TestRunDeliversFirstSnapshotAndStopsOnContext(t *testing.T) {
	client := newTestClient(t)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:  "mvd",
		Priority:  200,
		Countries: []string{"Sweden"},
		Refresh:   50 * time.Millisecond,
	}, client, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exp.Run(ctx, func(snap []config.UpstreamConfig) {
			if len(snap) != 2 {
				t.Errorf("snapshot len = %d, want 2", len(snap))
			}
			calls.Add(1)
		})
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for calls.Load() < 2 {
		select {
		case <-deadline.C:
			cancel()
			t.Fatalf("timed out waiting for refresh tick; calls=%d", calls.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestRunWithoutRefreshDeliversOneSnapshotAndExits(t *testing.T) {
	client := newTestClient(t)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:  "mvd",
		Priority:  200,
		Countries: []string{"Sweden"},
	}, client, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	var calls atomic.Int64
	err = exp.Run(context.Background(), func(snap []config.UpstreamConfig) {
		calls.Add(1)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestRunReturnsErrorOnInitialFetchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "mvd"}, &Client{URL: srv.URL, HTTPClient: srv.Client()}, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	if err := exp.Run(context.Background(), func([]config.UpstreamConfig) {}); err == nil {
		t.Fatal("expected error on initial fetch failure")
	}
}

func TestRunRefreshDeliversPrevAndNextEachTick(t *testing.T) {
	client := newTestClient(t)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:  "mvd",
		Priority:  200,
		Countries: []string{"Sweden"},
		Refresh:   50 * time.Millisecond,
	}, client, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	seed, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exp.RunRefresh(ctx, seed, func(prev, next []config.UpstreamConfig) {
			calls.Add(1)
			if len(prev) != 2 || len(next) != 2 {
				t.Errorf("prev=%d next=%d, want 2 and 2", len(prev), len(next))
			}
		})
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for calls.Load() < 1 {
		select {
		case <-deadline.C:
			cancel()
			t.Fatalf("RunRefresh never fired; calls=%d", calls.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunRefresh: %v", err)
	}
}

func TestRunRefreshExitsImmediatelyWhenRefreshDisabled(t *testing.T) {
	client := newTestClient(t)
	exp, err := NewExpander(ExpanderConfig{
		IDPrefix:  "mvd",
		Priority:  200,
		Countries: []string{"Sweden"},
	}, client, quietLogger())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	if err := exp.RunRefresh(context.Background(), nil, func(prev, next []config.UpstreamConfig) {
		t.Error("RunRefresh should not invoke onChange when refresh is 0")
	}); err != nil {
		t.Fatalf("RunRefresh: %v", err)
	}
}
