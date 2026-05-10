package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/metrics"
)

// Backend is the small surface the report handler calls into. The real
// implementation is *scoreboard.Scoreboard; tests pass a fake.
type Backend interface {
	// HasUpstream reports whether an upstream id exists in the live pool.
	// Used to reject reports for unknown ids (404) without needing to
	// allocate/copy the full pool id list on every request.
	HasUpstream(id string) bool

	// RecordSuccess applies success bookkeeping for one (host, upstreamID)
	// pair. Latency is informational only on this path; the control
	// endpoint passes a zero duration because it has no transport-side
	// measurement to report.
	RecordSuccess(host, upstreamID string, latency time.Duration)

	// RecordFailure applies a penalty for one (host, upstreamID, kind).
	// cooldownOverride is always nil from the control endpoint; outcome
	// reports do not carry Retry-After semantics in v1.
	RecordFailure(host, upstreamID string, kind failure.Kind, cooldownOverride *time.Duration)
}

// MetricsSink is the subset of metrics.Registry the handlers emit
// through. nil is a no-op.
type MetricsSink interface {
	ObserveReportReceived(outcome, upstreamID string)
	ObserveReportRejected(reason string)
}

// outcomeOK is the one outcome that maps to RecordSuccess. Every other
// known outcome maps to a failure.Kind via outcomeMap.
const outcomeOK = "ok"

// outcomeMap is the closed vocabulary the control endpoint accepts. It
// is the only place outcome strings are interpreted; any unknown value
// is rejected with 400 so a client-side typo surfaces immediately rather
// than being silently dropped.
//
// Keep this synchronized with docs/cooperative-reporting.md.
var outcomeMap = map[string]failure.Kind{
	"rate_limited": failure.KindRateLimit,
	"forbidden":    failure.KindForbidden,
	"legal_block":  failure.KindLegalBlock,
	"geo_block":    failure.KindBodyMatch,
	"timeout":      failure.KindTimeout,
	"refused":      failure.KindRefused,
}

// reportRequest is the wire shape POST /v1/report accepts. All three of
// host / upstream / outcome are required. http_status is optional and
// logged but not used for routing decisions in v1.
type reportRequest struct {
	Host       string `json:"host"`
	Upstream   string `json:"upstream"`
	Outcome    string `json:"outcome"`
	HTTPStatus *int   `json:"http_status,omitempty"`
}

// maxReportBytes caps POST body reads so a misbehaving (or hostile)
// client cannot exhaust memory by streaming unbounded JSON. 4 KiB is far
// more than any well-formed report needs (~200 bytes typical).
const maxReportBytes = 4 * 1024

// mountHandlers attaches POST /v1/report and GET /healthz to mux. The
// metrics sink is optional; when nil, counters are no-ops.
func mountHandlers(mux *http.ServeMux, backend Backend, metricsSink MetricsSink, logger *slog.Logger) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/report", func(w http.ResponseWriter, r *http.Request) {
		handleReport(w, r, backend, metricsSink, logger)
	})
}

// handleReport implements the POST /v1/report contract documented in
// docs/cooperative-reporting.md.
func handleReport(w http.ResponseWriter, r *http.Request, backend Backend, m MetricsSink, logger *slog.Logger) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxReportBytes+1))
	_ = r.Body.Close()
	if err != nil {
		rejectReport(w, m, metrics.ReportRejectBadJSON, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) > maxReportBytes {
		rejectReport(w, m, metrics.ReportRejectBadJSON, http.StatusRequestEntityTooLarge,
			"body exceeds 4 KiB cap")
		return
	}

	var req reportRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		rejectReport(w, m, metrics.ReportRejectBadJSON, http.StatusBadRequest, "parse json: "+err.Error())
		return
	}
	// Reject extra content after the first JSON object. Decode a second
	// value and require EOF so malformed tails (for example an extra '}')
	// are also rejected.
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		rejectReport(w, m, metrics.ReportRejectBadJSON, http.StatusBadRequest, "trailing content after json object")
		return
	}

	if backend == nil {
		rejectReport(w, m, metrics.ReportRejectScoreboardNotStarted, http.StatusServiceUnavailable,
			"scoreboard not ready")
		return
	}

	if missing := missingFields(req); missing != "" {
		rejectReport(w, m, metrics.ReportRejectMissingField, http.StatusBadRequest, "missing required field: "+missing)
		return
	}
	normalizedHost, err := normalizeReportHost(req.Host)
	if err != nil {
		rejectReport(w, m, metrics.ReportRejectBadJSON, http.StatusBadRequest, "invalid host: "+err.Error())
		return
	}

	kind, ok := resolveOutcome(req.Outcome)
	if !ok {
		rejectReport(w, m, metrics.ReportRejectUnknownOutcome, http.StatusBadRequest,
			fmt.Sprintf("unknown outcome %q (allowed: %s)", req.Outcome, allowedOutcomes()))
		return
	}

	if !backend.HasUpstream(req.Upstream) {
		rejectReport(w, m, metrics.ReportRejectUnknownUpstream, http.StatusNotFound,
			fmt.Sprintf("unknown upstream %q", req.Upstream))
		return
	}

	// Apply.
	if req.Outcome == outcomeOK {
		backend.RecordSuccess(normalizedHost, req.Upstream, 0)
	} else {
		backend.RecordFailure(normalizedHost, req.Upstream, kind, nil)
	}

	if m != nil {
		m.ObserveReportReceived(req.Outcome, req.Upstream)
	}

	logArgs := []any{
		"host", req.Host,
		"upstream", req.Upstream,
		"outcome", req.Outcome,
	}
	if req.HTTPStatus != nil {
		logArgs = append(logArgs, "http_status", *req.HTTPStatus)
	}
	logger.Debug("report accepted", logArgs...)

	w.WriteHeader(http.StatusNoContent)
}

var hostLabelRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func normalizeReportHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("host is empty")
	}
	if strings.ContainsAny(host, "/?#") {
		return "", fmt.Errorf("host %q contains URL delimiters", host)
	}

	if splitHost, splitPort, err := net.SplitHostPort(host); err == nil {
		normalizedHost, err := normalizeReportHostPart(splitHost)
		if err != nil {
			return "", err
		}
		if err := validateReportPort(splitPort); err != nil {
			return "", err
		}
		return net.JoinHostPort(normalizedHost, splitPort), nil
	}

	if strings.Contains(host, ":") {
		return "", fmt.Errorf("host %q must be hostname or host:port", host)
	}
	return normalizeReportHostPart(host)
}

func normalizeReportHostPart(host string) (string, error) {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	}
	if host == "" {
		return "", errors.New("host is empty")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	host = strings.ToLower(host)
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return "", fmt.Errorf("invalid hostname %q", host)
	}
	for _, label := range strings.Split(host, ".") {
		if !hostLabelRe.MatchString(label) {
			return "", fmt.Errorf("invalid hostname label %q", label)
		}
	}
	return host, nil
}

func validateReportPort(port string) error {
	p, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("port %q is not numeric: %w", port, err)
	}
	if p < 1 || p > 65535 {
		return fmt.Errorf("port %d is outside the 1-65535 range", p)
	}
	return nil
}

// resolveOutcome maps an outcome string to its failure.Kind. The boolean
// is false when the outcome is not in the closed vocabulary; "ok" is
// recognized but its returned Kind is the zero value because the success
// path bypasses the kind table.
func resolveOutcome(outcome string) (failure.Kind, bool) {
	if outcome == outcomeOK {
		return "", true
	}
	k, ok := outcomeMap[outcome]
	return k, ok
}

// missingFields returns the name of the first required field that is
// empty, or the empty string when all are present.
func missingFields(req reportRequest) string {
	switch {
	case req.Host == "":
		return "host"
	case req.Upstream == "":
		return "upstream"
	case req.Outcome == "":
		return "outcome"
	}
	return ""
}

// allowedOutcomes returns a sorted, comma-joined list of the accepted
// outcome strings. Used in 400 responses so the client sees the
// vocabulary inline.
func allowedOutcomes() string {
	out := make([]string, 0, len(outcomeMap)+1)
	out = append(out, outcomeOK)
	for k := range outcomeMap {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// rejectReport writes the rejection response, increments the rejection
// counter (if metrics is non-nil), and discards the request. status is
// the HTTP status to write; reason is the metrics label; msg is the
// human-readable plain-text body.
func rejectReport(w http.ResponseWriter, m MetricsSink, reason string, status int, msg string) {
	if m != nil {
		m.ObserveReportRejected(reason)
	}
	http.Error(w, msg, status)
}
