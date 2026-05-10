// Phase 12: opt-in bearer-token auth on the control endpoint. See
// docs/decisions.md ADR-007 for the rationale and docs/cooperative-
// reporting.md for the wire-protocol contract.
//
// The package exposes a small TokenSource interface (Allow + Enabled)
// so the server and the handlers depend on an abstraction that tests
// can fake; the concrete tokenSet stores the operator-configured tokens
// (precomputed as []byte to keep the request path allocation-light)
// behind an atomic.Pointer so Allow stays lock-free and ReplaceTokens
// is the only writer (driven by SIGHUP through the reloader). An empty
// token set short-circuits to "permit everything", preserving Phase 11
// behaviour for operators who do not opt in.

package control

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

// TokenSource is the auth surface mountHandlers consumes. Allow returns
// true when the supplied token is accepted; the empty set must return
// true so the no-auth default is observable through this one method.
type TokenSource interface {
	// Allow returns true when token is a member of the live token set
	// or when the set is empty. Empty token strings never match unless
	// the set is empty.
	Allow(token string) bool

	// Enabled reports whether the source has any tokens. Callers use it
	// to skip the Authorization header check entirely on the no-auth
	// path so unauthenticated clients keep working byte-for-byte the
	// way they did in Phase 11.
	Enabled() bool
}

// tokenSet is the production TokenSource. The stored snapshot holds
// each token as a precomputed []byte so Allow can call
// subtle.ConstantTimeCompare without a per-token allocation on the
// request path; the snapshot is swapped atomically so SIGHUP rotation
// does not block in-flight requests, and readers grab the pointer once
// and use that snapshot for the whole comparison loop.
type tokenSet struct {
	tokens atomic.Pointer[[][]byte]
}

// NewTokenSet builds a TokenSource from a tokens slice. Pass an empty
// (or nil) slice for the no-auth default. The caller is expected to
// have validated the slice (no empty strings, dedup) before calling.
func NewTokenSet(tokens []string) *tokenSet {
	s := &tokenSet{}
	s.store(tokens)
	return s
}

// Replace atomically swaps the live token set. Used by the SIGHUP
// reload path; safe to call concurrently with Allow.
func (s *tokenSet) Replace(tokens []string) {
	s.store(tokens)
}

func (s *tokenSet) store(tokens []string) {
	cp := make([][]byte, len(tokens))
	for i, t := range tokens {
		cp[i] = []byte(t)
	}
	s.tokens.Store(&cp)
}

func (s *tokenSet) snapshot() [][]byte {
	p := s.tokens.Load()
	if p == nil {
		return nil
	}
	return *p
}

// Allow implements TokenSource. The comparison loop intentionally
// avoids a short-circuit `break` on match so total work is independent
// of which token in the set matched. subtle.ConstantTimeCompare
// returns 0 on length mismatch (an inherent timing leak of input length
// vs. stored token length); ADR-007 accepts that for v1 since
// operator-chosen tokens are usually fixed-length high-entropy
// strings. Tokens are stored pre-converted to []byte so this hot path
// allocates exactly once per request (the input → []byte conversion).
func (s *tokenSet) Allow(input string) bool {
	tokens := s.snapshot()
	if len(tokens) == 0 {
		return true
	}
	if input == "" {
		return false
	}
	in := []byte(input)
	var ok int
	for _, t := range tokens {
		ok |= subtle.ConstantTimeCompare(in, t)
	}
	return ok == 1
}

// Enabled reports whether the live snapshot has any tokens.
func (s *tokenSet) Enabled() bool {
	return len(s.snapshot()) > 0
}

// authStatus enumerates how an inbound request's Authorization header
// parsed. The control handler maps each value to the right HTTP status
// + reports_rejected_total{reason} label.
type authStatus int

const (
	authPresent   authStatus = iota // header parsed cleanly; token still needs Allow()
	authMissing                     // no Authorization header at all
	authMalformed                   // header present but not parseable as RFC 6750 Bearer
)

// extractBearer parses the Authorization header per RFC 6750 §2.1.
// Returns the token and a status. RFC 7235 says at most one credential
// per request; multiple Authorization headers are treated as malformed
// rather than silently picking the first. The Bearer scheme keyword is
// case-insensitive; the token is taken verbatim and must be non-empty.
func extractBearer(r *http.Request) (token string, status authStatus) {
	headers := r.Header.Values("Authorization")
	switch len(headers) {
	case 0:
		return "", authMissing
	case 1:
		// single header is the well-formed case; parse below
	default:
		return "", authMalformed
	}
	h := headers[0]
	const prefix = "Bearer "
	if len(h) <= len(prefix) {
		return "", authMalformed
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", authMalformed
	}
	t := strings.TrimSpace(h[len(prefix):])
	if t == "" {
		return "", authMalformed
	}
	return t, authPresent
}

// LoadTokensFile reads an auth_tokens_file. Format: one token per line,
// blank lines ignored, lines beginning with `#` (after optional leading
// whitespace) treated as comments. Trailing whitespace is stripped.
// Returns the parsed tokens (de-duplicated, order preserved) and any
// per-line error. Returns (nil, nil) for an empty file. The caller is
// responsible for the missing-file policy: at startup a missing file
// is warned and treated as empty (ADR-007); on SIGHUP a missing file
// keeps the previous set.
func LoadTokensFile(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open auth_tokens_file %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var (
		out  []string
		seen = make(map[string]struct{})
	)
	sc := bufio.NewScanner(f)
	// 4 MiB cap: token files are tiny in practice but big enough that
	// the default 64 KiB scanner buffer doesn't surprise an operator
	// who keeps thousands of rotating tokens.
	sc.Buffer(make([]byte, 0, 4096), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := strings.TrimRight(sc.Text(), " \t\r")
		trim := strings.TrimLeft(raw, " \t")
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if _, dup := seen[trim]; dup {
			continue
		}
		seen[trim] = struct{}{}
		out = append(out, trim)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read auth_tokens_file %q: %w (line %d)", path, err, lineNo)
	}
	return out, nil
}

// MergeTokens combines the inline TOML list and the file-loaded list
// into the final dedup'd token set. Order is inline-first, file-second
// (so an operator who lists a token both ways sees the inline ordering
// in any future debug log). Empty strings are filtered defensively even
// though Validate already rejects them inline; the file loader strips
// blanks too.
func MergeTokens(inline, fromFile []string) []string {
	if len(inline) == 0 && len(fromFile) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(inline)+len(fromFile))
	out := make([]string, 0, len(inline)+len(fromFile))
	for _, t := range inline {
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range fromFile {
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
