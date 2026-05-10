// Command integration is the smallest possible example of integrating
// an app with Tunnelsmith's cooperative outcome reporting (Phase 11).
//
// Usage:
//
//	# Start tunnelsmith somewhere reachable, then:
//	go run ./examples/integration --proxy http://tunnelsmith:8080 --control http://tunnelsmith:9092 --url https://example.com/
//
// What this demonstrates:
//
//   - One client.New call configures everything.
//   - Plain http.Client.Get works through the proxy.
//   - Status codes 429 / 403 / 451 auto-report.
//   - Soft geo-blocks (HTTP 200 with a "not available in your region"
//     body) only the app can detect; client.Report submits the outcome.
//
// docs/cooperative-reporting.md is the full reference.
package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pacnpal/tunnelsmith/client"
)

func main() {
	proxyURL := flag.String("proxy", "http://tunnelsmith:8080", "Tunnelsmith proxy listener")
	controlURL := flag.String("control", "http://tunnelsmith:9092", "Tunnelsmith control endpoint")
	target := flag.String("url", "https://example.com/", "URL to fetch")
	flag.Parse()

	c, err := client.New(client.Options{
		ProxyURL:   *proxyURL,
		ControlURL: *controlURL,
		Timeout:    2 * time.Second,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "configure tunnelsmith client:", err)
		os.Exit(1)
	}

	resp, err := c.Get(*target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	fmt.Printf("status=%d bytes=%d\n", resp.StatusCode, len(body))

	// App-driven outcome: soft geo-block detection a regex inside the
	// proxy could not see because this is HTTPS.
	outcome := classifyForExample(resp.StatusCode, body)
	if outcome == "" {
		return
	}
	if err := client.Report(resp, outcome); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return
	}
	fmt.Printf("reported outcome=%s\n", outcome)
}

// classifyForExample returns a semantic outcome for the response, or
// empty string if no app-driven report is needed (the SDK's auto-report
// already covers 429 / 403 / 451). Real apps put their own domain
// logic here.
func classifyForExample(status int, body []byte) string {
	if status == http.StatusOK && bodyContainsGeoBlockHint(body) {
		return "geo_block"
	}
	if status == http.StatusOK {
		return "ok"
	}
	return ""
}

// bodyContainsGeoBlockHint is the dumbest possible content-based check.
// Real apps look for the destination's specific block pages.
func bodyContainsGeoBlockHint(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "not available in your region") ||
		strings.Contains(s, "region restricted") ||
		strings.Contains(s, "geo-blocked")
}
