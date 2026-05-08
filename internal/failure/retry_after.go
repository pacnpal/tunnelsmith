package failure

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter parses an RFC 7231 §7.1.3 Retry-After header value relative
// to the supplied clock reading. The grammar admits two shapes: a
// non-negative integer count of seconds, or an HTTP-date in any of the three
// forms RFC 7231 §7.1.1.1 lists (IMF-fixdate, the obsolete RFC 850 form, and
// ANSI C asctime). net/http.ParseTime handles all three date shapes.
//
// Returns the duration the destination is asking the caller to wait. ok is
// false when the value is empty, parses as neither shape, names a negative
// integer of seconds, or names a date in the past relative to now (which
// would otherwise produce a negative duration). The returned duration is
// non-negative when ok is true, including the legitimate "Retry-After: 0"
// case which means "retry immediately".
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	d := t.Sub(now)
	if d < 0 {
		return 0, false
	}
	return d, true
}
