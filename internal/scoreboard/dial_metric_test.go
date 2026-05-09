package scoreboard

import (
	"sync"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
)

// TestObserveDialFailureNeverReportsSuccess locks in the structural fix
// for the Gemini-flagged bug: observeDialFailure must never produce
// outcome="success", even when ClassifyDialError returned an empty Kind
// (the API contract allows it). Before the split into separate
// success / failure helpers, an empty Kind in the failure path was
// silently mapped to "success".
func TestObserveDialFailureNeverReportsSuccess(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind failure.Kind
		want string
	}{
		{name: "empty kind", kind: "", want: "other"},
		{name: "refused", kind: failure.KindRefused, want: "refused"},
		{name: "timeout", kind: failure.KindTimeout, want: "timeout"},
		{name: "unknown kind", kind: failure.Kind("nonsense"), want: "other"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink := &recordingSink{}
			sb := &Scoreboard{metrics: sink}
			sb.observeDialFailure("upstream-x", tc.kind, 5*time.Millisecond)
			got := sink.lastOutcome()
			if got == "success" {
				t.Fatalf("observeDialFailure(%q) reported outcome=success; failure path must never claim success", tc.kind)
			}
			if got != tc.want {
				t.Errorf("observeDialFailure(%q) outcome = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

// TestObserveDialSuccessAlwaysReportsSuccess is the matching positive
// assertion: the success helper always tags outcome="success".
func TestObserveDialSuccessAlwaysReportsSuccess(t *testing.T) {
	t.Parallel()
	sink := &recordingSink{}
	sb := &Scoreboard{metrics: sink}
	sb.observeDialSuccess("upstream-y", time.Millisecond)
	if got := sink.lastOutcome(); got != "success" {
		t.Errorf("observeDialSuccess outcome = %q, want success", got)
	}
}

type recordingSink struct {
	mu     sync.Mutex
	last   string
	calls  int
	upID   string
	period time.Duration
}

func (r *recordingSink) ObserveDial(upstreamID, outcome string, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = outcome
	r.upID = upstreamID
	r.period = latency
	r.calls++
}

func (r *recordingSink) ObserveCascadeTrip() {}
func (r *recordingSink) ObserveProbePick()   {}

func (r *recordingSink) lastOutcome() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}
