package webshare

import "net/http"

// RetargetExpanderForTest swaps the underlying Client's BaseURL and
// HTTPClient so an httptest-driven E2E test can drive a real Expander
// without making the field exported in production code. Only used by
// tests in the parallel _test package.
func RetargetExpanderForTest(e *Expander, baseURL string, hc *http.Client) {
	e.client.BaseURL = baseURL
	if hc != nil {
		e.client.HTTPClient = hc
	}
}
