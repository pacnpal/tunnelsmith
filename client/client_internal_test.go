package client

import (
	"net/http/httptest"
	"testing"
)

func TestHostForReportAddsDefaultHTTPSPort(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "https://example.com/path", nil)
	if got := hostForReport(req); got != "example.com:443" {
		t.Fatalf("hostForReport = %q, want %q", got, "example.com:443")
	}
}

func TestHostForReportAddsDefaultHTTPPort(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://example.com/path", nil)
	if got := hostForReport(req); got != "example.com:80" {
		t.Fatalf("hostForReport = %q, want %q", got, "example.com:80")
	}
}

func TestHostForReportKeepsExplicitPort(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "https://example.com:8443/path", nil)
	if got := hostForReport(req); got != "example.com:8443" {
		t.Fatalf("hostForReport = %q, want %q", got, "example.com:8443")
	}
}
