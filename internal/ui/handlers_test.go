package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
)

// fakeBackend is a Backend stub that records every mutation and returns
// canned snapshots. Goroutine-safe because the UI test server runs
// handlers in parallel.
type fakeBackend struct {
	mu sync.Mutex

	entries  []scoreboard.EntrySnapshot
	forces   []scoreboard.ForceSnapshotEntry
	poolIDs  []string
	cooled   map[string]int
	cascade  int
	forceErr error

	// now, when set, is the value Now() returns. Tests that need
	// deterministic timestamps in /api/scoreboard or that exercise the
	// duration / until math in /api/force pin this. Zero value means
	// fall back to the real wall clock so tests that do not care about
	// time keep working without changes.
	now time.Time

	forgotten   []string
	cleared     []string
	forced      []forceCall
	resetCalled int
}

type forceCall struct {
	Host       string
	UpstreamID string
	Until      time.Time
}

func (f *fakeBackend) Snapshot() []scoreboard.EntrySnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]scoreboard.EntrySnapshot(nil), f.entries...)
}

func (f *fakeBackend) ForceSnapshot() []scoreboard.ForceSnapshotEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]scoreboard.ForceSnapshotEntry(nil), f.forces...)
}

func (f *fakeBackend) CooledHostsByUpstream() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.cooled))
	for k, v := range f.cooled {
		out[k] = v
	}
	return out
}

func (f *fakeBackend) CascadeActiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cascade
}

func (f *fakeBackend) PoolIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.poolIDs...)
}

func (f *fakeBackend) Forget(host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, host)
	return true
}

func (f *fakeBackend) Force(host, upstreamID string, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forceErr != nil {
		return f.forceErr
	}
	f.forced = append(f.forced, forceCall{Host: host, UpstreamID: upstreamID, Until: until})
	return nil
}

func (f *fakeBackend) ClearForce(host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, host)
	return true
}

func (f *fakeBackend) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalled++
}

func (f *fakeBackend) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.now.IsZero() {
		return time.Now()
	}
	return f.now
}

func newTestServer(backend Backend) *httptest.Server {
	mux := http.NewServeMux()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	mountHandlers(mux, backend, logger)
	return httptest.NewServer(mux)
}

func TestServeIndex(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("<title>Tunnelsmith</title>")) {
		t.Errorf("index does not contain expected <title>; got first 200 bytes: %q", body[:min(len(body), 200)])
	}
}

func TestServeStaticAsset(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/static/app.js")
	if err != nil {
		t.Fatalf("GET /static/app.js: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestUnknownPath404(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/never")
	if err != nil {
		t.Fatalf("GET /never: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestScoreboardEndpointShape(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	backend := &fakeBackend{
		entries: []scoreboard.EntrySnapshot{
			{Host: "a.example.com", UpstreamID: "u1", Score: 3, LastSeen: now, GlobalSuccess: 7},
			{Host: "a.example.com", UpstreamID: "u2", Score: -1, CooldownUntil: now.Add(time.Minute), GlobalFailure: 2},
		},
		forces: []scoreboard.ForceSnapshotEntry{
			{Host: "b.example.com", UpstreamID: "u1", Until: now.Add(time.Hour)},
		},
		poolIDs: []string{"u1", "u2"},
		cooled:  map[string]int{"u2": 1},
		cascade: 1,
	}
	srv := newTestServer(backend)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/scoreboard")
	if err != nil {
		t.Fatalf("GET /api/scoreboard: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var got struct {
		PoolIDs       []string                        `json:"pool_ids"`
		Entries       []scoreboard.EntrySnapshot      `json:"entries"`
		Forces        []scoreboard.ForceSnapshotEntry `json:"forces"`
		CooledByID    map[string]int                  `json:"cooled_by_upstream"`
		CascadeActive int                             `json:"cascade_active"`
		GeneratedAt   time.Time                       `json:"generated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.PoolIDs) != 2 || got.PoolIDs[0] != "u1" {
		t.Errorf("pool_ids = %v, want [u1 u2]", got.PoolIDs)
	}
	if len(got.Entries) != 2 || got.Entries[0].Host != "a.example.com" {
		t.Errorf("entries = %+v, want first host a.example.com", got.Entries)
	}
	if got.CooledByID["u2"] != 1 {
		t.Errorf("cooled_by_upstream[u2] = %d, want 1", got.CooledByID["u2"])
	}
	if got.CascadeActive != 1 {
		t.Errorf("cascade_active = %d, want 1", got.CascadeActive)
	}
	if got.GeneratedAt.IsZero() {
		t.Error("generated_at is zero")
	}
	if len(got.Forces) != 1 || got.Forces[0].Host != "b.example.com" {
		t.Errorf("forces = %+v, want one entry for b.example.com", got.Forces)
	}
}

func TestScoreboardEndpointGeneratedAtFromBackendClock(t *testing.T) {
	// /api/scoreboard's generated_at must come from backend.Now(), not
	// time.Now(), so a manual-clock scoreboard test sees the same
	// timestamp through both layers (issue #18).
	pinned := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	backend := &fakeBackend{now: pinned}
	srv := newTestServer(backend)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/scoreboard")
	if err != nil {
		t.Fatalf("GET /api/scoreboard: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		GeneratedAt time.Time `json:"generated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.GeneratedAt.Equal(pinned) {
		t.Errorf("generated_at = %v, want %v (backend.Now())", got.GeneratedAt, pinned)
	}
}

func TestForceEndpointDurationUsesBackendClock(t *testing.T) {
	// POST /api/force with a duration must compute until on backend.Now(),
	// not time.Now() (issue #18). Pinning the backend clock to a fixed
	// instant lets us assert exact equality on the resolved Until.
	pinned := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	backend := &fakeBackend{now: pinned}
	srv := newTestServer(backend)
	defer srv.Close()

	body := `{"host":"a.example.com","upstream_id":"u1","duration":"30m"}`
	resp, err := http.Post(srv.URL+"/api/force", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/force: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(backend.forced) != 1 {
		t.Fatalf("backend.forced = %v, want 1 entry", backend.forced)
	}
	want := pinned.Add(30 * time.Minute)
	if !backend.forced[0].Until.Equal(want) {
		t.Errorf("until = %v, want %v (pinned + 30m)", backend.forced[0].Until, want)
	}
}

func TestForceEndpointUntilCheckedAgainstBackendClock(t *testing.T) {
	// "until in the past" must be evaluated against backend.Now(), not
	// time.Now(). Send an until that is in the wall-clock future but
	// before the pinned backend clock; the handler should reject it.
	pinned := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	backend := &fakeBackend{now: pinned}
	srv := newTestServer(backend)
	defer srv.Close()

	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]string{
		"host": "a.example.com", "upstream_id": "u1", "until": until,
	})
	resp, err := http.Post(srv.URL+"/api/force", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (until in past relative to pinned now)", resp.StatusCode)
	}
}

func TestScoreboardRejectsNonGet(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/scoreboard", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/scoreboard: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestForgetEndpointCallsBackend(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(backend)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/forget", "application/json", strings.NewReader(`{"host":"a.example.com"}`))
	if err != nil {
		t.Fatalf("POST /api/forget: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body["removed"] {
		t.Error("response.removed = false, want true")
	}
	if len(backend.forgotten) != 1 || backend.forgotten[0] != "a.example.com" {
		t.Errorf("backend.forgotten = %v, want [a.example.com]", backend.forgotten)
	}
}

func TestForgetEndpointRejectsEmptyHost(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/forget", "application/json", strings.NewReader(`{"host":""}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestForceEndpointCallsBackendWithDuration(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(backend)
	defer srv.Close()
	body := `{"host":"a.example.com","upstream_id":"u1","duration":"30m"}`
	before := time.Now()
	resp, err := http.Post(srv.URL+"/api/force", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/force: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(backend.forced) != 1 {
		t.Fatalf("backend.forced = %v, want 1 entry", backend.forced)
	}
	got := backend.forced[0]
	if got.Host != "a.example.com" || got.UpstreamID != "u1" {
		t.Errorf("forced = %+v, want a.example.com / u1", got)
	}
	wantMin := before.Add(29 * time.Minute)
	wantMax := time.Now().Add(31 * time.Minute)
	if got.Until.Before(wantMin) || got.Until.After(wantMax) {
		t.Errorf("until = %v, want roughly 30m from now", got.Until)
	}
}

func TestForceEndpointAcceptsRFC3339Until(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(backend)
	defer srv.Close()
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]string{
		"host": "a.example.com", "upstream_id": "u1", "until": until,
	})
	resp, err := http.Post(srv.URL+"/api/force", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestForceEndpointRejectsBothFields(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()
	body := `{"host":"a.example.com","upstream_id":"u1","duration":"30m","until":"2030-01-01T00:00:00Z"}`
	resp, err := http.Post(srv.URL+"/api/force", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestForceEndpointSurfacesUnknownUpstream(t *testing.T) {
	backend := &fakeBackend{forceErr: scoreboard.ErrUnknownUpstream}
	srv := newTestServer(backend)
	defer srv.Close()
	body := `{"host":"a.example.com","upstream_id":"nope","duration":"30m"}`
	resp, err := http.Post(srv.URL+"/api/force", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(got, []byte("unknown upstream")) {
		t.Errorf("response body = %q, want to mention unknown upstream", got)
	}
}

func TestForceClearEndpoint(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(backend)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/force/clear", "application/json", strings.NewReader(`{"host":"a.example.com"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if len(backend.cleared) != 1 || backend.cleared[0] != "a.example.com" {
		t.Errorf("backend.cleared = %v, want [a.example.com]", backend.cleared)
	}
}

func TestResetEndpointCallsBackend(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(backend)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/reset", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if backend.resetCalled != 1 {
		t.Errorf("Reset called %d times, want 1", backend.resetCalled)
	}
}

func TestEndpointsRejectMalformedJSON(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/forget", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServerLifecycle(t *testing.T) {
	backend := &fakeBackend{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := NewServer("127.0.0.1:0", backend, logger)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(context.Background()) }()
	<-srv.Ready()
	if srv.Addr() == nil {
		t.Fatal("Addr is nil after Ready")
	}
	resp, err := http.Get("http://" + srv.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}
	if err := srv.Shutdown(timeoutCtx(t, time.Second)); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve returned %v", err)
	}
}

func timeoutCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
