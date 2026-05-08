package failure_test

import (
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
)

func TestParseRetryAfterSeconds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"0", 0},
		{"1", time.Second},
		{"30", 30 * time.Second},
		{"  47 ", 47 * time.Second},
		{"3600", time.Hour},
	}
	for _, tc := range cases {
		got, ok := failure.ParseRetryAfter(tc.in, now)
		if !ok {
			t.Errorf("ParseRetryAfter(%q) ok=false, want true", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{
			name: "IMF-fixdate 30 seconds in the future",
			in:   "Fri, 08 May 2026 12:00:30 GMT",
			want: 30 * time.Second,
		},
		{
			name: "RFC 850 form 90 seconds in the future",
			in:   "Friday, 08-May-26 12:01:30 GMT",
			want: 90 * time.Second,
		},
		{
			name: "asctime form 5 seconds in the future",
			in:   "Fri May  8 12:00:05 2026",
			want: 5 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := failure.ParseRetryAfter(tc.in, now)
			if !ok {
				t.Fatalf("ParseRetryAfter(%q) ok=false, want true", tc.in)
			}
			if got != tc.want {
				t.Fatalf("ParseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseRetryAfterRejectsGarbage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	cases := []string{
		"",
		"   ",
		"-1",
		"30s",
		"thirty",
		"Fri, 33 Pug 2026 99:99:99 GMT",
	}
	for _, in := range cases {
		if d, ok := failure.ParseRetryAfter(in, now); ok {
			t.Errorf("ParseRetryAfter(%q) ok=true, want false (got %v)", in, d)
		}
	}
}

func TestParseRetryAfterPastDateReturnsFalse(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	in := "Fri, 08 May 2026 11:59:00 GMT" // 60 seconds before now
	if d, ok := failure.ParseRetryAfter(in, now); ok {
		t.Fatalf("ParseRetryAfter(%q) ok=true, want false for past date (got %v)", in, d)
	}
}
