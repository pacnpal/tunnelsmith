package listener

import (
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// stalledBody is a ReadCloser whose Read blocks until Close is called.
// Mirrors a misbehaving upstream that ships status line + headers and
// then stalls on body bytes. Used to confirm drainAndClose returns even
// when the underlying read never completes on its own.
type stalledBody struct {
	closed atomic.Bool
	gate   chan struct{}
}

func newStalledBody() *stalledBody {
	return &stalledBody{gate: make(chan struct{})}
}

func (s *stalledBody) Read(p []byte) (int, error) {
	<-s.gate
	return 0, io.EOF
}

func (s *stalledBody) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		close(s.gate)
	}
	return nil
}

// TestDrainAndCloseIsTimeBounded covers Copilot's review on PR #13:
// drainAndClose's byte cap (LimitReader 64 KiB) bounds bytes but not
// wall time. An upstream that stalls mid-body would otherwise block
// the listener's retry loop indefinitely. The function must time out,
// close the body, and return inside the configured drain timeout.
func TestDrainAndCloseIsTimeBounded(t *testing.T) {
	t.Parallel()
	body := newStalledBody()

	start := time.Now()
	drainAndClose(body)
	elapsed := time.Since(start)

	// drainAndClose's internal timeout is 250ms; allow a little slack
	// for scheduler jitter on a busy CI runner without making green
	// runs slower in any meaningful way.
	if elapsed > time.Second {
		t.Fatalf("drainAndClose took %v on a stalled body, expected to return well under 1s", elapsed)
	}
	if !body.closed.Load() {
		t.Error("drainAndClose returned without closing the body")
	}
}

// TestDrainAndCloseFastPath confirms a body that finishes draining
// before the timeout returns immediately rather than waiting out the
// full timer. The fast path is the common case (bodies that fit under
// the 64 KiB cap), so it must not introduce a 250 ms tax per discarded
// response.
func TestDrainAndCloseFastPath(t *testing.T) {
	t.Parallel()
	// io.NopCloser around a strings.Reader-equivalent finishes the
	// Copy effectively instantly; using io.NopCloser of an empty
	// reader is enough to exercise the done-arrives-first branch.
	body := io.NopCloser(emptyReader{})

	start := time.Now()
	drainAndClose(body)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("drainAndClose took %v on an empty body; should be near-instant", elapsed)
	}
}

// emptyReader returns EOF on the first Read so drainAndClose's Copy
// completes immediately.
type emptyReader struct{}

func (emptyReader) Read(p []byte) (int, error) { return 0, io.EOF }
