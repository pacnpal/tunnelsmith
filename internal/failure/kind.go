package failure

import (
	"errors"
	"fmt"
)

// ErrProxyAuth is the sentinel a dial path wraps when an upstream HTTP
// proxy answered the CONNECT handshake with 407 Proxy Authentication
// Required. Wrap with %w so errors.Is(err, ErrProxyAuth) matches across
// the chain (the listener's aggregated errors.Join from
// Scoreboard.DialFor preserves the chain). ClassifyDialError consults
// this sentinel to return KindProxyAuth without an import cycle through
// the upstream package.
var ErrProxyAuth = errors.New("upstream proxy returned 407 Proxy Authentication Required")

// Kind tags the shape of an upstream-side failure so the scoreboard can apply
// the right penalty and cooldown. Values are stable strings so they survive
// log lines and metric labels without churn when callers add new kinds.
//
// Phase 4 fires KindRefused and KindTimeout from the dial path. KindRateLimit,
// KindForbidden, and KindLegalBlock land when Phase 5 wires HTTP status code
// inspection. KindBodyMatch lands in Phase 8 with body-regex detection. The
// enum lists every kind the scoreboard needs so the per-kind policy table
// (penalty + cooldown) is a complete map at construction time, even if some
// kinds are not yet emitted.
type Kind string

const (
	// KindRefused covers TCP connection refusals, resets, and any dial error
	// that did not classify into a more specific kind. The dial path treats
	// it as the canonical "exit cannot reach host" signal.
	KindRefused Kind = "refused"

	// KindTimeout covers context-deadline exceeded, OS deadline exceeded, and
	// any net.Error reporting Timeout(). Softer evidence than refused because
	// it can be transient network weirdness rather than the exit being unable
	// to reach the host.
	KindTimeout Kind = "timeout"

	// KindRateLimit covers HTTP 429 responses. The exit is fine; the
	// destination is rate-limiting that exit's IP. Phase 5 fires it.
	KindRateLimit Kind = "rate_limit"

	// KindForbidden covers HTTP 403 responses. Usually a durable IP or region
	// block at the destination. Phase 5 fires it.
	KindForbidden Kind = "forbidden"

	// KindLegalBlock covers HTTP 451 responses. Geo-block at the legal layer,
	// near-certain evidence the exit's country is wrong for that destination.
	// Phase 5 fires it.
	KindLegalBlock Kind = "legal_block"

	// KindBodyMatch covers a body-regex match flagged by Phase 8. The
	// destination returned a 2xx page that the configured regex identified
	// as a soft block (typical "not available in your region" page).
	KindBodyMatch Kind = "body_match"

	// KindProxyAuth covers an upstream HTTP proxy answering the CONNECT
	// handshake with 407 Proxy Authentication Required. Differs from
	// KindRefused (TCP-level rejection) and KindForbidden (destination
	// 403): the proxy itself rejected our credentials before the request
	// reached the destination. Driven by ClassifyDialError matching the
	// ErrProxyAuth sentinel; the auto-heal driver in cmd/tunnelsmith
	// subscribes to these events and triggers a credential refresh on
	// the configured provider when a burst suggests a rotated password.
	KindProxyAuth Kind = "proxy_auth"
)

// AllKinds lists every Kind in declaration order. Useful for building per-kind
// lookup tables and for tests that want to enumerate the enum without
// hard-coding the list at the call site.
var AllKinds = []Kind{
	KindRefused,
	KindTimeout,
	KindRateLimit,
	KindForbidden,
	KindLegalBlock,
	KindBodyMatch,
	KindProxyAuth,
}

// String returns the kind's stable wire form. Used in log lines and metrics.
func (k Kind) String() string { return string(k) }

// Valid reports whether k is one of the declared Kind values. Returns false
// for the zero value, so callers that build a Kind from untrusted input (for
// example, a status-rule mapping) can guard their lookups.
func (k Kind) Valid() bool {
	switch k {
	case KindRefused, KindTimeout, KindRateLimit, KindForbidden, KindLegalBlock, KindBodyMatch, KindProxyAuth:
		return true
	}
	return false
}

// ClassifyDialError maps a dial error to a Kind. Order matters:
//
//   - ErrProxyAuth wins first. A CONNECT 407 is a credential signal we
//     want the auto-heal driver to react to; misclassifying it as
//     KindRefused (the catch-all branch) would silently bury the
//     event so the operator only sees a generic dial failure.
//   - Timeout wins over refused because a timeout-shaped error can
//     also satisfy IsConnectionRefused on some platforms when the
//     deadline fires mid-handshake; Phase 4 wants the softer kind in
//     that case so a slow exit does not get punished as if it had
//     actively refused.
//   - Anything else falls into KindRefused, matching the proposal's
//     "treat unclassified dial failures as the canonical hard signal"
//     stance.
func ClassifyDialError(err error) Kind {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrProxyAuth):
		return KindProxyAuth
	case IsTimeout(err):
		return KindTimeout
	case IsConnectionRefused(err):
		return KindRefused
	default:
		return KindRefused
	}
}

// MustParseKind returns the Kind whose String() equals s, or panics if no
// such Kind exists. Intended for tests and config-time wiring where an
// invalid value is a programming bug.
func MustParseKind(s string) Kind {
	k := Kind(s)
	if !k.Valid() {
		panic(fmt.Sprintf("failure.MustParseKind: %q is not a known Kind", s))
	}
	return k
}
