package failure_test

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/pacnpal/tunnelsmith/internal/failure"
)

func TestKindStringMatchesValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		k    failure.Kind
		want string
	}{
		{failure.KindRefused, "refused"},
		{failure.KindTimeout, "timeout"},
		{failure.KindRateLimit, "rate_limit"},
		{failure.KindForbidden, "forbidden"},
		{failure.KindLegalBlock, "legal_block"},
		{failure.KindBodyMatch, "body_match"},
		{failure.KindProxyAuth, "proxy_auth"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("Kind(%q).String() = %q, want %q", c.k, got, c.want)
		}
	}
}

func TestKindValid(t *testing.T) {
	t.Parallel()
	for _, k := range failure.AllKinds {
		if !k.Valid() {
			t.Errorf("Kind %q is in AllKinds but Valid() returned false", k)
		}
	}
	for _, bad := range []failure.Kind{"", "wat", "Refused", "REFUSED"} {
		if bad.Valid() {
			t.Errorf("Kind %q is not declared but Valid() returned true", bad)
		}
	}
}

func TestAllKindsCoversEveryDeclaredValue(t *testing.T) {
	t.Parallel()
	// Mechanical: AllKinds must include every Kind constant. If a future
	// phase adds a new Kind without appending it here, the per-kind policy
	// table built off AllKinds will silently miss the new kind.
	want := map[failure.Kind]bool{
		failure.KindRefused:    true,
		failure.KindTimeout:    true,
		failure.KindRateLimit:  true,
		failure.KindForbidden:  true,
		failure.KindLegalBlock: true,
		failure.KindBodyMatch:  true,
		failure.KindProxyAuth:  true,
	}
	if len(failure.AllKinds) != len(want) {
		t.Fatalf("len(AllKinds) = %d, want %d", len(failure.AllKinds), len(want))
	}
	for _, k := range failure.AllKinds {
		if !want[k] {
			t.Errorf("AllKinds contains unknown Kind %q", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("AllKinds is missing declared Kind %q", k)
	}
}

func TestClassifyDialError(t *testing.T) {
	t.Parallel()

	t.Run("nil returns empty kind", func(t *testing.T) {
		t.Parallel()
		if got := failure.ClassifyDialError(nil); got != "" {
			t.Errorf("ClassifyDialError(nil) = %q, want empty", got)
		}
	})

	t.Run("context deadline is timeout", func(t *testing.T) {
		t.Parallel()
		if got := failure.ClassifyDialError(context.DeadlineExceeded); got != failure.KindTimeout {
			t.Errorf("ClassifyDialError(DeadlineExceeded) = %q, want timeout", got)
		}
	})

	t.Run("net timeout is timeout", func(t *testing.T) {
		t.Parallel()
		if got := failure.ClassifyDialError(fakeNetErr{timeout: true}); got != failure.KindTimeout {
			t.Errorf("ClassifyDialError(net.Error timeout) = %q, want timeout", got)
		}
	})

	t.Run("ECONNREFUSED is refused", func(t *testing.T) {
		t.Parallel()
		if got := failure.ClassifyDialError(syscall.ECONNREFUSED); got != failure.KindRefused {
			t.Errorf("ClassifyDialError(ECONNREFUSED) = %q, want refused", got)
		}
	})

	t.Run("unknown error falls back to refused", func(t *testing.T) {
		t.Parallel()
		if got := failure.ClassifyDialError(errors.New("kaboom")); got != failure.KindRefused {
			t.Errorf("ClassifyDialError(kaboom) = %q, want refused", got)
		}
	})

	t.Run("ErrProxyAuth maps to KindProxyAuth", func(t *testing.T) {
		t.Parallel()
		// The sentinel wrapped via %w is exactly how upstream.go marks
		// a CONNECT-407 dial error. ClassifyDialError must recognise it
		// via errors.Is across the chain so the auto-heal driver
		// observes the event under the right kind.
		wrapped := errors.New("http upstream \"ws-d-1\": CONNECT got status 407: " + failure.ErrProxyAuth.Error())
		// errors.New doesn't actually wrap; build a real wrap.
		_ = wrapped
		dialErr := fakeWrappedErr{inner: failure.ErrProxyAuth}
		if got := failure.ClassifyDialError(dialErr); got != failure.KindProxyAuth {
			t.Errorf("ClassifyDialError(ErrProxyAuth) = %q, want proxy_auth", got)
		}
	})

	t.Run("ErrProxyAuth wins over a wrapped timeout", func(t *testing.T) {
		t.Parallel()
		// Defensive case: if a future code path produces an error that
		// satisfies both branches (e.g. CONNECT response read returned
		// 407 after a deadline-extended retry), the proxy-auth kind
		// must still win because the operator-actionable signal is
		// the 407, not the timeout.
		dialErr := wrappedProxyAuthAndTimeout{}
		if got := failure.ClassifyDialError(dialErr); got != failure.KindProxyAuth {
			t.Errorf("ClassifyDialError(proxyAuth+timeout) = %q, want proxy_auth", got)
		}
	})

	t.Run("timeout wins over a wrapped refused", func(t *testing.T) {
		t.Parallel()
		// Construct an error that satisfies both Timeout() and ECONNREFUSED.
		// The classifier must pick KindTimeout because the softer kind is
		// the right call when a deadline fires mid-handshake.
		if got := failure.ClassifyDialError(wrappedTimeoutAndRefused{}); got != failure.KindTimeout {
			t.Errorf("ClassifyDialError(timeout+refused) = %q, want timeout", got)
		}
	})
}

func TestMustParseKind(t *testing.T) {
	t.Parallel()
	if got := failure.MustParseKind("refused"); got != failure.KindRefused {
		t.Errorf("MustParseKind(refused) = %q, want refused", got)
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustParseKind on unknown kind did not panic")
		}
	}()
	_ = failure.MustParseKind("not-a-kind")
}

// fakeNetErr is a minimal net.Error that lets tests exercise the timeout
// branch of ClassifyDialError without leaning on platform-specific dialing.
type fakeNetErr struct {
	timeout bool
}

func (f fakeNetErr) Error() string   { return "fake net error" }
func (f fakeNetErr) Timeout() bool   { return f.timeout }
func (f fakeNetErr) Temporary() bool { return false }

// wrappedTimeoutAndRefused satisfies both errors.Is(syscall.ECONNREFUSED) and
// the net.Error Timeout() check. ClassifyDialError must prefer the timeout
// classification because it is the softer (and more accurate) kind when a
// deadline fires mid-handshake.
type wrappedTimeoutAndRefused struct{}

func (wrappedTimeoutAndRefused) Error() string        { return "timeout-after-refused" }
func (wrappedTimeoutAndRefused) Timeout() bool        { return true }
func (wrappedTimeoutAndRefused) Temporary() bool      { return false }
func (wrappedTimeoutAndRefused) Is(target error) bool { return target == syscall.ECONNREFUSED }

// fakeWrappedErr is a minimal error whose Unwrap exposes the inner
// error so errors.Is walks to it. Lets the test demonstrate the
// "wrapped via %w" production shape without depending on fmt.Errorf
// behavior changes across Go versions.
type fakeWrappedErr struct {
	inner error
}

func (f fakeWrappedErr) Error() string { return "wrap: " + f.inner.Error() }
func (f fakeWrappedErr) Unwrap() error { return f.inner }

// wrappedProxyAuthAndTimeout satisfies both errors.Is(failure.ErrProxyAuth)
// and the net.Error Timeout() check. ClassifyDialError must prefer the
// proxy-auth classification because that is the operator-actionable
// kind that the auto-heal driver subscribes to.
type wrappedProxyAuthAndTimeout struct{}

func (wrappedProxyAuthAndTimeout) Error() string   { return "proxy-auth-and-timeout" }
func (wrappedProxyAuthAndTimeout) Timeout() bool   { return true }
func (wrappedProxyAuthAndTimeout) Temporary() bool { return false }
func (wrappedProxyAuthAndTimeout) Is(target error) bool {
	return target == failure.ErrProxyAuth
}
