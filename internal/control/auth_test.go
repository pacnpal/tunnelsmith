package control

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hasReject reports whether m recorded the named reject reason at least
// once. Used by every auth test that asserts metric side-effects.
func (f *fakeMetrics) hasReject(reason string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rejected {
		if r == reason {
			return true
		}
	}
	return false
}

func TestTokenSetAllowEmptyPermitsAnything(t *testing.T) {
	t.Parallel()
	ts := NewTokenSet(nil)
	if !ts.Allow("anything") {
		t.Error("empty token set should permit any input")
	}
	if !ts.Allow("") {
		t.Error("empty token set should permit empty input (no-auth default)")
	}
	if ts.Enabled() {
		t.Error("empty token set Enabled() = true, want false")
	}
}

func TestTokenSetAllowAcceptsKnown(t *testing.T) {
	t.Parallel()
	ts := NewTokenSet([]string{"alpha", "bravo", "charlie"})
	if !ts.Enabled() {
		t.Fatal("non-empty token set Enabled() = false")
	}
	for _, ok := range []string{"alpha", "bravo", "charlie"} {
		if !ts.Allow(ok) {
			t.Errorf("Allow(%q) = false, want true", ok)
		}
	}
}

func TestTokenSetAllowRejectsUnknownAndEmpty(t *testing.T) {
	t.Parallel()
	ts := NewTokenSet([]string{"alpha"})
	for _, bad := range []string{"", "ALPHA", "alpha ", " alpha", "beta", "alphax", "alph"} {
		if ts.Allow(bad) {
			t.Errorf("Allow(%q) = true, want false", bad)
		}
	}
}

func TestTokenSetReplaceIsAtomic(t *testing.T) {
	t.Parallel()
	ts := NewTokenSet([]string{"old"})
	if !ts.Allow("old") {
		t.Fatal("pre-Replace: Allow(old) = false")
	}
	ts.Replace([]string{"new"})
	if ts.Allow("old") {
		t.Error("post-Replace: Allow(old) = true, want false")
	}
	if !ts.Allow("new") {
		t.Error("post-Replace: Allow(new) = false, want true")
	}
	ts.Replace(nil)
	if ts.Enabled() {
		t.Error("post-Replace(nil): Enabled() = true, want false (no-auth default restored)")
	}
	if !ts.Allow("anything") {
		t.Error("post-Replace(nil): empty set should permit anything")
	}
}

func TestExtractBearerVariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      func(*http.Request)
		wantToken  string
		wantStatus authStatus
	}{
		{
			name:       "absent",
			setup:      func(r *http.Request) {},
			wantStatus: authMissing,
		},
		{
			name:       "well-formed Bearer",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer tok-1") },
			wantToken:  "tok-1",
			wantStatus: authPresent,
		},
		{
			name:       "lowercase scheme",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "bearer tok-2") },
			wantToken:  "tok-2",
			wantStatus: authPresent,
		},
		{
			name:       "wrong scheme",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "Basic dXNlcjpwYXNz") },
			wantStatus: authMalformed,
		},
		{
			name:       "no space between scheme and token",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "Bearertok") },
			wantStatus: authMalformed,
		},
		{
			name:       "empty token after scheme",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
			wantStatus: authMalformed,
		},
		{
			name:       "whitespace-only token",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer    ") },
			wantStatus: authMalformed,
		},
		{
			name: "duplicate headers",
			setup: func(r *http.Request) {
				r.Header.Add("Authorization", "Bearer tok-a")
				r.Header.Add("Authorization", "Bearer tok-b")
			},
			wantStatus: authMalformed,
		},
		{
			name:       "embedded space in token",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer abc def") },
			wantStatus: authMalformed,
		},
		{
			name:       "embedded tab in token",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer abc\tdef") },
			wantStatus: authMalformed,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodPost, "/v1/report", strings.NewReader(""))
			tc.setup(r)
			token, status := extractBearer(r)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if token != tc.wantToken {
				t.Errorf("token = %q, want %q", token, tc.wantToken)
			}
		})
	}
}

func TestReportRejectsMissingAuthorization(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"u1"}}
	m := &fakeMetrics{}
	srv := newTestServerWithAuth(t, backend, m, []string{"tok"}, false)

	resp, err := http.Post(srv.URL+"/v1/report", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="tunnelsmith"` {
		t.Errorf("WWW-Authenticate = %q, want bearer realm header", got)
	}
	if !m.hasReject("auth_missing") {
		t.Errorf("rejected reasons = %v, want auth_missing", m.rejected)
	}
}

func TestReportRejectsMalformedAuthorization(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"u1"}}
	m := &fakeMetrics{}
	srv := newTestServerWithAuth(t, backend, m, []string{"tok"}, false)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/report", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if !m.hasReject("auth_failed") {
		t.Errorf("rejected reasons = %v, want auth_failed", m.rejected)
	}
}

func TestReportRejectsUnknownToken(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"u1"}}
	m := &fakeMetrics{}
	srv := newTestServerWithAuth(t, backend, m, []string{"good"}, false)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/report", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer bad")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if !m.hasReject("auth_failed") {
		t.Errorf("rejected reasons = %v, want auth_failed", m.rejected)
	}
}

// TestReportAuthChecksBeforeBodyRead proves the auth gate runs before
// the 4 KiB body buffer. Without auth a 5 KiB body would return 413
// (request entity too large); with auth missing it must return 401 and
// leave the oversize-body branch unreached.
func TestReportAuthChecksBeforeBodyRead(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"u1"}}
	m := &fakeMetrics{}
	srv := newTestServerWithAuth(t, backend, m, []string{"tok"}, false)

	oversize := strings.Repeat("x", 5*1024)
	resp, err := http.Post(srv.URL+"/v1/report", "application/json", strings.NewReader(oversize))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (auth must fail before body read)", resp.StatusCode)
	}
	if m.hasReject("bad_json") {
		t.Error("bad_json was ticked: body got read despite missing auth")
	}
}

func TestReportAcceptsKnownToken(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"u1"}}
	m := &fakeMetrics{}
	srv := newTestServerWithAuth(t, backend, m, []string{"tok"}, false)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/report",
		strings.NewReader(`{"host":"example.com:443","upstream":"u1","outcome":"ok"}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if m.hasReject("auth_missing") || m.hasReject("auth_failed") {
		t.Errorf("rejected reasons should be empty, got %v", m.rejected)
	}
}

func TestHealthzUngatedByDefault(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"u1"}}
	m := &fakeMetrics{}
	srv := newTestServerWithAuth(t, backend, m, []string{"tok"}, false)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 (default ungated)", resp.StatusCode)
	}
}

func TestHealthzGatedWhenConfigured(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"u1"}}
	m := &fakeMetrics{}
	srv := newTestServerWithAuth(t, backend, m, []string{"tok"}, true)

	// No header → 401.
	resp1, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusUnauthorized {
		t.Errorf("healthz status (no auth) = %d, want 401", resp1.StatusCode)
	}

	// With token → 200.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET healthz auth: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("healthz status (auth) = %d, want 200", resp2.StatusCode)
	}
}

func TestServerReplaceTokensSwapsLive(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{poolIDs: []string{"u1"}}
	s := NewServer("127.0.0.1:0", backend, nil, ServerOptions{Tokens: []string{"old"}}, quietLogger())

	// "old" passes pre-swap.
	if !s.tokens.Allow("old") {
		t.Fatal("pre-swap: Allow(old) = false")
	}

	s.ReplaceTokens([]string{"new"})

	if s.tokens.Allow("old") {
		t.Error("post-swap: Allow(old) = true, want false")
	}
	if !s.tokens.Allow("new") {
		t.Error("post-swap: Allow(new) = false, want true")
	}

	// Replace with nil restores no-auth.
	s.ReplaceTokens(nil)
	if s.tokens.Enabled() {
		t.Error("post-Replace(nil): Enabled() = true, want false")
	}
}

func TestLoadTokensFileParsesCommentsAndDedups(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	contents := "# rotated 2026-05-10\nalpha\n\n  # indented comment\nbravo\nalpha\n  charlie  \n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadTokensFile(path)
	if err != nil {
		t.Fatalf("LoadTokensFile: %v", err)
	}
	want := []string{"alpha", "bravo", "charlie"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestLoadTokensFileRejectsEmbeddedWhitespace pins the round-5 rule:
// a token file with internal whitespace inside a token is rejected
// with a load-time error rather than silently producing a token that
// can never match what extractBearer accepts.
func TestLoadTokensFileRejectsEmbeddedWhitespace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	if err := os.WriteFile(path, []byte("good-token\nbad token with space\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadTokensFile(path); err == nil {
		t.Fatal("LoadTokensFile: want error for embedded whitespace, got nil")
	} else if !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("error %q should mention whitespace", err.Error())
	}
}

func TestLoadTokensFileMissingReturnsError(t *testing.T) {
	t.Parallel()
	_, err := LoadTokensFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("missing file: want error, got nil")
	}
}

func TestMergeTokensInlineFirstDedup(t *testing.T) {
	t.Parallel()
	got := MergeTokens([]string{"a", "b"}, []string{"b", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}
