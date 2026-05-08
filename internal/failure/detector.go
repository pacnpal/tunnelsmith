package failure

import (
	"net/http"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/config"
)

// StatusDetector inspects an HTTP response status code and headers and
// reports whether the response should be treated as an upstream-side
// failure. The detector is configured from the parsed [[failure.status]]
// rules; codes without a configured rule are reported as success.
//
// The detector deliberately maps a small fixed set of status codes to
// failure kinds: 429 -> rate limit, 403 -> forbidden, 451 -> legal block.
// Other codes that appear in [[failure.status]] (for example, a user opting
// into 503 detection per the proposal) are not yet wired through to a
// failure.Kind, so they are skipped silently. Phase 8's body-regex work and
// any future Kind additions will broaden the supported set.
type StatusDetector struct {
	rules map[int]statusRule
	now   func() time.Time
}

// statusRule is the per-code detection policy the detector consults at
// request time. The kind is the failure.Kind to record; honorRetryAfter
// turns on Retry-After header parsing for this code only.
type statusRule struct {
	kind            Kind
	honorRetryAfter bool
}

// Detection is the return shape for a positive Detect call. Kind names the
// failure shape so the scoreboard can apply the right penalty and cooldown.
//
// CooldownOverride is non-nil only when the matching rule honors
// Retry-After AND the response carried a parsable Retry-After value. When
// non-nil, it is the parsed duration the destination asked Tunnelsmith to
// wait, including the legitimate "Retry-After: 0" case which is honored
// verbatim ("retry immediately"). nil means the scoreboard should fall
// back to the kind's configured cooldown.
type Detection struct {
	Kind             Kind
	CooldownOverride *time.Duration
}

// statusKinds is the fixed map from HTTP status code to failure.Kind. The
// proposal calls these out as the three response shapes Tunnelsmith treats
// as upstream-affecting failures: 429 cycles, 403 hardens, 451 quarantines.
// Adding a kind here without also updating scoreboard.FromConfig produces a
// Kind without a penalty, so the failure is recorded but has no effect.
var statusKinds = map[int]Kind{
	429: KindRateLimit,
	403: KindForbidden,
	451: KindLegalBlock,
}

// NewStatusDetector builds a detector from the parsed [[failure.status]]
// rules. Rules whose code is not in statusKinds are skipped: they do not
// have a corresponding failure.Kind yet and the listener cannot do anything
// useful with them.
//
// A nil or empty rules slice produces a detector that reports success for
// every status code, which is the right behavior when the user disables
// status-code detection by emptying out [[failure.status]].
func NewStatusDetector(rules []config.StatusRule) *StatusDetector {
	out := &StatusDetector{
		rules: make(map[int]statusRule, len(rules)),
		now:   time.Now,
	}
	for _, r := range rules {
		kind, ok := statusKinds[r.Code]
		if !ok {
			continue
		}
		out.rules[r.Code] = statusRule{
			kind:            kind,
			honorRetryAfter: r.HonorRetryAfter,
		}
	}
	return out
}

// WithClock injects a custom time source for HTTP-date Retry-After parsing.
// Tests use this so the parser's "is this date in the future" check is
// deterministic. Returns the detector so calls can chain.
func (d *StatusDetector) WithClock(now func() time.Time) *StatusDetector {
	if d != nil && now != nil {
		d.now = now
	}
	return d
}

// Detect reports whether a response with the given status code and headers
// should be treated as an upstream failure. ok is false for any code without
// a configured rule (including all 2xx responses, all 5xx responses, and
// 4xx codes outside the statusKinds set). When ok is true, the returned
// Detection.Kind names the failure shape; RetryAfter is non-zero only when
// the rule allows honoring it and the Retry-After header parsed cleanly.
func (d *StatusDetector) Detect(status int, header http.Header) (Detection, bool) {
	if d == nil {
		return Detection{}, false
	}
	rule, ok := d.rules[status]
	if !ok {
		return Detection{}, false
	}
	out := Detection{Kind: rule.kind}
	if rule.honorRetryAfter && header != nil {
		if v := header.Get("Retry-After"); v != "" {
			if dur, parsed := ParseRetryAfter(v, d.now()); parsed {
				out.CooldownOverride = &dur
			}
		}
	}
	return out, true
}
