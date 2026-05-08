package failure_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
	"github.com/pacnpal/tunnelsmith/internal/failure"
)

func defaultRules() []config.StatusRule {
	return []config.StatusRule{
		{Code: 429, Penalty: 4, Cooldown: config.Duration(120 * time.Second), HonorRetryAfter: true},
		{Code: 403, Penalty: 6, Cooldown: config.Duration(30 * time.Minute)},
		{Code: 451, Penalty: 8, Cooldown: config.Duration(6 * time.Hour)},
	}
}

func TestDetectKnownStatusCodes(t *testing.T) {
	t.Parallel()
	d := failure.NewStatusDetector(defaultRules())

	cases := []struct {
		status   int
		wantKind failure.Kind
	}{
		{429, failure.KindRateLimit},
		{403, failure.KindForbidden},
		{451, failure.KindLegalBlock},
	}
	for _, tc := range cases {
		got, ok := d.Detect(tc.status, http.Header{})
		if !ok {
			t.Errorf("Detect(%d) ok=false, want true", tc.status)
			continue
		}
		if got.Kind != tc.wantKind {
			t.Errorf("Detect(%d).Kind = %q, want %q", tc.status, got.Kind, tc.wantKind)
		}
		if got.RetryAfter != 0 {
			t.Errorf("Detect(%d) without Retry-After header set RetryAfter = %v, want 0", tc.status, got.RetryAfter)
		}
	}
}

func TestDetectIgnoresUnknownStatusCodes(t *testing.T) {
	t.Parallel()
	d := failure.NewStatusDetector(defaultRules())

	codes := []int{200, 204, 301, 400, 401, 404, 500, 502, 503, 504}
	for _, code := range codes {
		if _, ok := d.Detect(code, http.Header{}); ok {
			t.Errorf("Detect(%d) ok=true, want false (no rule for this code)", code)
		}
	}
}

func TestDetectHonorsRetryAfterSeconds(t *testing.T) {
	t.Parallel()
	d := failure.NewStatusDetector(defaultRules())

	header := http.Header{}
	header.Set("Retry-After", "30")
	got, ok := d.Detect(429, header)
	if !ok {
		t.Fatalf("Detect(429) ok=false, want true")
	}
	if got.Kind != failure.KindRateLimit {
		t.Fatalf("Detect(429).Kind = %q, want %q", got.Kind, failure.KindRateLimit)
	}
	if got.RetryAfter != 30*time.Second {
		t.Fatalf("Detect(429) RetryAfter = %v, want 30s", got.RetryAfter)
	}
}

func TestDetectHonorsRetryAfterHTTPDate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	d := failure.NewStatusDetector(defaultRules()).WithClock(func() time.Time { return now })

	header := http.Header{}
	header.Set("Retry-After", "Fri, 08 May 2026 12:01:00 GMT")
	got, ok := d.Detect(429, header)
	if !ok {
		t.Fatalf("Detect(429) ok=false, want true")
	}
	if got.RetryAfter != time.Minute {
		t.Fatalf("Detect(429) RetryAfter = %v, want 60s", got.RetryAfter)
	}
}

func TestDetectIgnoresRetryAfterWhenRuleDisablesIt(t *testing.T) {
	t.Parallel()
	// The default 403 rule has HonorRetryAfter=false. A 403 with a
	// Retry-After header must not surface that header in Detection.
	d := failure.NewStatusDetector(defaultRules())

	header := http.Header{}
	header.Set("Retry-After", "30")
	got, ok := d.Detect(403, header)
	if !ok {
		t.Fatalf("Detect(403) ok=false, want true")
	}
	if got.RetryAfter != 0 {
		t.Errorf("Detect(403) RetryAfter = %v, want 0 (rule does not honor Retry-After)", got.RetryAfter)
	}
}

func TestDetectIgnoresUnparsableRetryAfter(t *testing.T) {
	t.Parallel()
	d := failure.NewStatusDetector(defaultRules())

	header := http.Header{}
	header.Set("Retry-After", "tomorrow-ish")
	got, ok := d.Detect(429, header)
	if !ok {
		t.Fatalf("Detect(429) ok=false, want true")
	}
	if got.RetryAfter != 0 {
		t.Errorf("Detect(429) RetryAfter = %v, want 0 (header value did not parse)", got.RetryAfter)
	}
}

func TestDetectDoesNotMatchCodeWithoutConfiguredRule(t *testing.T) {
	t.Parallel()
	// A user can disable a code by removing it from [[failure.status]].
	// Build a detector with only 451 wired; 429 and 403 must be reported as
	// success.
	rules := []config.StatusRule{
		{Code: 451, Penalty: 8, Cooldown: config.Duration(6 * time.Hour)},
	}
	d := failure.NewStatusDetector(rules)

	if _, ok := d.Detect(429, http.Header{}); ok {
		t.Error("Detect(429) ok=true with no 429 rule configured, want false")
	}
	if _, ok := d.Detect(403, http.Header{}); ok {
		t.Error("Detect(403) ok=true with no 403 rule configured, want false")
	}
	if _, ok := d.Detect(451, http.Header{}); !ok {
		t.Error("Detect(451) ok=false with 451 rule configured, want true")
	}
}

func TestDetectSkipsUnsupportedCodesInRules(t *testing.T) {
	t.Parallel()
	// A user-added rule for an unsupported code (e.g. 503) is silently
	// skipped at construction. Detect must not match it.
	rules := []config.StatusRule{
		{Code: 503, Penalty: 1, Cooldown: config.Duration(time.Minute)},
	}
	d := failure.NewStatusDetector(rules)
	if _, ok := d.Detect(503, http.Header{}); ok {
		t.Error("Detect(503) ok=true; 503 has no Kind mapping, want false")
	}
}

func TestDetectNilDetectorReportsSuccess(t *testing.T) {
	t.Parallel()
	var d *failure.StatusDetector
	if _, ok := d.Detect(429, http.Header{}); ok {
		t.Error("nil StatusDetector.Detect ok=true, want false")
	}
}
