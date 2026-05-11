package webshare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

func newListServer(t *testing.T, proxies []Proxy) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":    len(proxies),
			"next":     nil,
			"previous": nil,
			"results":  proxies,
		})
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	c.APIToken = "tok"
	c.HTTPClient = srv.Client()
	return c, srv
}

func TestExpanderSnapshotTransforms(t *testing.T) {
	c, _ := newListServer(t, []Proxy{
		{ID: "d-10", Username: "u", Password: "p", ProxyAddress: "1.1.1.1", Port: 8001, Valid: true, CountryCode: "US"},
		{ID: "d-20", Username: "u", Password: "p", ProxyAddress: "2.2.2.2", Port: 8002, Valid: false, CountryCode: "DE"},
		{ID: "d-30", Username: "u", Password: "p", ProxyAddress: "3.3.3.3", Port: 8003, Valid: true, CountryCode: "GB"},
	})
	priority := 200
	_ = priority
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 200}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Invalid proxy d-20 must be skipped.
	if len(got) != 2 {
		t.Fatalf("expected 2 valid upstreams, got %d: %+v", len(got), got)
	}
	if got[0].ID != "ws-d-10" || got[1].ID != "ws-d-30" {
		t.Fatalf("id transform: %+v", got)
	}
	if got[0].Addr != "1.1.1.1:8001" || got[1].Addr != "3.3.3.3:8003" {
		t.Fatalf("addr transform: %+v", got)
	}
	if got[0].Username != "u" || got[0].Password != "p" {
		t.Fatalf("auth lost: %+v", got[0])
	}
	if got[0].Kind != config.KindHTTP {
		t.Fatalf("default kind not http: %v", got[0].Kind)
	}
	if got[0].Priority == nil || *got[0].Priority != 200 {
		t.Fatalf("priority not threaded: %+v", got[0])
	}
}

func TestExpanderHonorsKindSOCKS5(t *testing.T) {
	c, _ := newListServer(t, []Proxy{
		{ID: "d-1", ProxyAddress: "1.1.1.1", Port: 1080, Valid: true},
	})
	exp, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 200, Kind: "socks5"}, c, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 1 || got[0].Kind != config.KindSOCKS5 {
		t.Fatalf("expected socks5, got %+v", got)
	}
}

func TestExpanderRejectsBadMode(t *testing.T) {
	c := NewClient()
	c.APIToken = "x"
	if _, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Mode: "wireguard"}, c, nil); err == nil {
		t.Fatal("expected error for bad mode")
	}
}

func TestExpanderRejectsBadKind(t *testing.T) {
	c := NewClient()
	c.APIToken = "x"
	if _, err := NewExpander(ExpanderConfig{IDPrefix: "ws", Kind: "wireguard"}, c, nil); err == nil {
		t.Fatal("expected error for bad kind")
	}
}

func TestExpanderRejectsEmptyIDPrefix(t *testing.T) {
	c := NewClient()
	c.APIToken = "x"
	if _, err := NewExpander(ExpanderConfig{}, c, nil); err == nil {
		t.Fatal("expected error for empty id_prefix")
	}
}

// TestExpanderSnapshotIsSortedByID locks the determinism contract the
// pool composer's diff relies on. Webshare returns proxies in
// account-creation order; the expander must re-sort so a stable input
// produces a stable slice (and the no-op shortcut in poolComposer.Update
// fires when nothing changed).
func TestExpanderSnapshotIsSortedByID(t *testing.T) {
	c, _ := newListServer(t, []Proxy{
		{ID: "d-30", ProxyAddress: "3.3.3.3", Port: 1, Valid: true},
		{ID: "d-10", ProxyAddress: "1.1.1.1", Port: 1, Valid: true},
		{ID: "d-20", ProxyAddress: "2.2.2.2", Port: 1, Valid: true},
	})
	exp, _ := NewExpander(ExpanderConfig{IDPrefix: "ws", Priority: 1}, c, nil)
	got, err := exp.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	want := []string{"ws-d-10", "ws-d-20", "ws-d-30"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}
