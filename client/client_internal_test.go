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

func TestHostForReportHTTPReturnsHostnameOnly(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://example.com/path", nil)
	if got := hostForReport(req); got != "example.com" {
		t.Fatalf("hostForReport = %q, want %q", got, "example.com")
	}
}

func TestHostForReportKeepsExplicitPort(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "https://example.com:8443/path", nil)
	if got := hostForReport(req); got != "example.com:8443" {
		t.Fatalf("hostForReport = %q, want %q", got, "example.com:8443")
	}
}

func TestHostForReportHTTPDropsExplicitPort(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://example.com:8080/path", nil)
	if got := hostForReport(req); got != "example.com" {
		t.Fatalf("hostForReport = %q, want %q", got, "example.com")
	}
}

func TestAutoOutcomeForRequiresHTTPS(t *testing.T) {
	t.Parallel()
	httpsReq := httptest.NewRequest("GET", "https://example.com/path", nil)
	if got := autoOutcomeFor(httpsReq, 429); got != "rate_limited" {
		t.Fatalf("autoOutcomeFor(https,429) = %q, want %q", got, "rate_limited")
	}
	httpReq := httptest.NewRequest("GET", "http://example.com/path", nil)
	if got := autoOutcomeFor(httpReq, 429); got != "" {
		t.Fatalf("autoOutcomeFor(http,429) = %q, want empty", got)
	}
}
