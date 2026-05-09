package ui

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
)

// staticFS is the compiled-in copy of the static HTML/JS/CSS the UI
// serves at /. embed.FS preserves directory structure so the handler
// strips the "static/" prefix when serving requests.
//
//go:embed static/*
var staticFS embed.FS

// Backend is the surface mountHandlers needs from the rest of the
// binary. Splitting it out lets tests pass a fake without dragging in
// the live scoreboard. The methods match the scoreboard's public
// admin surface 1:1; cmd/tunnelsmith adapts the live scoreboard via
// scoreboardBackend below.
type Backend interface {
	Snapshot() []scoreboard.EntrySnapshot
	ForceSnapshot() []scoreboard.ForceSnapshotEntry
	CooledHostsByUpstream() map[string]int
	CascadeActiveCount() int
	PoolIDs() []string

	Forget(host string) bool
	Force(host, upstreamID string, until time.Time) error
	ClearForce(host string) bool
	Reset()
}

// mountHandlers wires the four action endpoints, the GET scoreboard
// read, the embedded static index, and a /healthz probe onto mux.
//
// Endpoint shapes (kept boring on purpose; docs/ui.md is the user-
// facing reference):
//
//   - GET  /                  -> static/index.html
//   - GET  /static/<file>     -> the embedded asset
//   - GET  /healthz           -> 200 "ok\n"
//   - GET  /api/scoreboard    -> JSON {pool_ids, entries, forces, cooled, cascade_active}
//   - POST /api/forget        -> JSON {host} -> {removed}
//   - POST /api/force         -> JSON {host, upstream_id, duration|until} -> 204
//   - POST /api/force/clear   -> JSON {host} -> {removed}
//   - POST /api/reset         -> {} -> 204
func mountHandlers(mux *http.ServeMux, backend Backend, logger *slog.Logger) {
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Building with go:embed makes this unreachable; staticFS is
		// guaranteed to contain "static/". Panic so a bad embed config
		// surfaces at startup rather than at first request.
		panic(err)
	}
	indexHTML, err := fs.ReadFile(staticSub, "index.html")
	if err != nil {
		panic(err)
	}
	staticServer := http.FileServer(http.FS(staticSub))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve index.html bytes directly. http.FileServer issues a
		// redirect from /index.html back to / for the canonical path,
		// which loops when the request started at /. Reading the bytes
		// from embed.FS sidesteps that.
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(indexHTML)
			return
		}
		http.NotFound(w, r)
	})
	mux.Handle("/static/", http.StripPrefix("/static/", staticServer))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/api/scoreboard", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeScoreboard(w, backend)
	})

	mux.HandleFunc("/api/forget", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Host string `json:"host"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body.Host = strings.TrimSpace(body.Host)
		if body.Host == "" {
			http.Error(w, "host is required", http.StatusBadRequest)
			return
		}
		removed := backend.Forget(body.Host)
		logger.Info("ui forget", "host", body.Host, "removed", removed)
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
	})

	mux.HandleFunc("/api/force", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Host       string `json:"host"`
			UpstreamID string `json:"upstream_id"`
			Duration   string `json:"duration"`
			Until      string `json:"until"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body.Host = strings.TrimSpace(body.Host)
		body.UpstreamID = strings.TrimSpace(body.UpstreamID)
		if body.Host == "" || body.UpstreamID == "" {
			http.Error(w, "host and upstream_id are required", http.StatusBadRequest)
			return
		}
		until, err := resolveForceUntil(body.Duration, body.Until)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := backend.Force(body.Host, body.UpstreamID, until); err != nil {
			if errors.Is(err, scoreboard.ErrUnknownUpstream) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info("ui force",
			"host", body.Host,
			"upstream_id", body.UpstreamID,
			"until", until.UTC().Format(time.RFC3339),
		)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/force/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Host string `json:"host"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body.Host = strings.TrimSpace(body.Host)
		if body.Host == "" {
			http.Error(w, "host is required", http.StatusBadRequest)
			return
		}
		removed := backend.ClearForce(body.Host)
		logger.Info("ui force clear", "host", body.Host, "removed", removed)
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
	})

	mux.HandleFunc("/api/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Body is optional. Accept and discard so a client that posts a
		// JSON object does not see a 400.
		var body map[string]any
		if r.ContentLength > 0 {
			_ = decodeJSONBody(r, &body)
		}
		backend.Reset()
		logger.Warn("ui reset", "remote_addr", r.RemoteAddr)
		w.WriteHeader(http.StatusNoContent)
	})
}

// scoreboardResponse is the wire shape /api/scoreboard returns. Field
// order is documented in docs/ui.md and the test asserts on it.
type scoreboardResponse struct {
	PoolIDs       []string        `json:"pool_ids"`
	Entries       []entryResponse `json:"entries"`
	Forces        []forceResponse `json:"forces"`
	CooledByID    map[string]int  `json:"cooled_by_upstream"`
	CascadeActive int             `json:"cascade_active"`
	GeneratedAt   time.Time       `json:"generated_at"`
}

type entryResponse struct {
	Host          string    `json:"host"`
	UpstreamID    string    `json:"upstream_id"`
	Score         float64   `json:"score"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	LastSeen      time.Time `json:"last_seen,omitempty"`
	GlobalSuccess uint64    `json:"global_success"`
	GlobalFailure uint64    `json:"global_failure"`
}

type forceResponse struct {
	Host       string    `json:"host"`
	UpstreamID string    `json:"upstream_id"`
	Until      time.Time `json:"until"`
}

func writeScoreboard(w http.ResponseWriter, backend Backend) {
	snap := backend.Snapshot()
	entries := make([]entryResponse, len(snap))
	for i, s := range snap {
		entries[i] = entryResponse{
			Host:          s.Host,
			UpstreamID:    s.UpstreamID,
			Score:         s.Score,
			CooldownUntil: s.CooldownUntil,
			LastSeen:      s.LastSeen,
			GlobalSuccess: s.GlobalSuccess,
			GlobalFailure: s.GlobalFailure,
		}
	}
	fsnap := backend.ForceSnapshot()
	forces := make([]forceResponse, len(fsnap))
	for i, f := range fsnap {
		forces[i] = forceResponse{
			Host:       f.Host,
			UpstreamID: f.UpstreamID,
			Until:      f.Until,
		}
	}
	resp := scoreboardResponse{
		PoolIDs:       backend.PoolIDs(),
		Entries:       entries,
		Forces:        forces,
		CooledByID:    backend.CooledHostsByUpstream(),
		CascadeActive: backend.CascadeActiveCount(),
		GeneratedAt:   time.Now().UTC(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveForceUntil parses the "duration" / "until" pair POST /api/force
// accepts. Exactly one must be set; passing both is an error so a
// client cannot post conflicting values. duration is a Go time string
// ("30m", "2h"); until is RFC3339.
func resolveForceUntil(durationStr, untilStr string) (time.Time, error) {
	durationStr = strings.TrimSpace(durationStr)
	untilStr = strings.TrimSpace(untilStr)
	if durationStr == "" && untilStr == "" {
		return time.Time{}, errors.New("either duration or until is required")
	}
	if durationStr != "" && untilStr != "" {
		return time.Time{}, errors.New("set duration or until, not both")
	}
	if durationStr != "" {
		d, err := time.ParseDuration(durationStr)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid duration %q: %s", durationStr, err.Error())
		}
		if d <= 0 {
			return time.Time{}, fmt.Errorf("duration must be > 0, got %v", d)
		}
		return time.Now().Add(d), nil
	}
	t, err := time.Parse(time.RFC3339, untilStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid until %q: %s", untilStr, err.Error())
	}
	if !t.After(time.Now()) {
		return time.Time{}, fmt.Errorf("until %q must be in the future", untilStr)
	}
	return t, nil
}

// decodeJSONBody reads up to 1 MiB of JSON from the request body and
// decodes into dst. The cap protects the binary from a client that
// streams an unbounded body; the real payload here is a few hundred
// bytes at most.
func decodeJSONBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	limited := http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %s", err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
