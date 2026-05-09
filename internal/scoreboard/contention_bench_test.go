package scoreboard_test

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
	"github.com/pacnpal/tunnelsmith/internal/scoreboard"
	"github.com/pacnpal/tunnelsmith/internal/upstream"
)

// BenchmarkScoreboardWriterContention is the profile run for issue #12.
// It seeds a scoreboard with 1k hosts spread across 20 upstreams, then
// has every available CPU writer hammer Pick + RecordSuccess in a tight
// loop while a background goroutine calls Snapshot, CooledHostsByUpstream,
// and CascadeActiveCount every 5ms and a second goroutine runs
// SaveSnapshot every 50ms. The benchmark reports ns/op alongside the
// total snapshot reads and writes so reviewers can see whether the
// prune / decay write-lock holds back the hot path.
//
// The benchmark uses public APIs only; no internal hooks. Snapshot and
// CooledHostsByUpstream both take the read lock, so they exercise the
// reader-vs-writer contention path. SaveSnapshot also takes the read
// lock and dominates real-world snapshot cost.
func BenchmarkScoreboardWriterContention(b *testing.B) {
	const (
		hostCount     = 1_000
		upstreamCount = 20
	)

	pool := buildPersistTestPoolN(b, upstreamCount)
	cfg := scoreboard.Config{
		KindPolicy: map[failure.Kind]scoreboard.Policy{
			failure.KindRefused: {Penalty: 1, Cooldown: 30 * time.Second},
		},
		SuccessWeight:  1,
		ScoreCap:       100,
		ProbeChance:    0,
		DecayInterval:  100 * time.Millisecond,
		DecayStep:      0.5,
		CascadeTTL:     30 * time.Second,
		DebounceWindow: 0,
		PruneAfter:     time.Hour,
	}
	sb, err := scoreboard.New(pool, cfg, scoreboard.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		b.Fatalf("New: %v", err)
	}

	// Seed the scoreboard so the writers hit the existing-entry path.
	for h := 0; h < hostCount; h++ {
		host := fmt.Sprintf("host-%05d.example.com", h)
		for u := 0; u < upstreamCount; u++ {
			id := fmt.Sprintf("up-%02d", u)
			sb.RecordSuccess(host, id, time.Millisecond)
		}
	}

	stop := make(chan struct{})
	var snapshotsTaken atomic.Int64
	var snapshotsWritten atomic.Int64

	go func() {
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = sb.Snapshot()
				_ = sb.CooledHostsByUpstream()
				_ = sb.CascadeActiveCount()
				snapshotsTaken.Add(1)
			}
		}
	}()

	dir := b.TempDir()
	snapshotPath := filepath.Join(dir, "snapshot.gob")
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = sb.SaveSnapshot(snapshotPath)
				snapshotsWritten.Add(1)
			}
		}
	}()

	b.ResetTimer()
	b.SetParallelism(runtime.NumCPU())
	b.RunParallel(func(pb *testing.PB) {
		var counter uint64
		for pb.Next() {
			counter++
			host := fmt.Sprintf("host-%05d.example.com", counter%hostCount)
			id := fmt.Sprintf("up-%02d", counter%upstreamCount)
			tried := map[string]bool{}
			if up, err := sb.Pick(host, tried); err == nil {
				sb.RecordSuccess(host, up.ID(), time.Millisecond)
			} else {
				sb.RecordSuccess(host, id, time.Millisecond)
			}
		}
	})
	b.StopTimer()
	close(stop)

	// ReportMetric values are absolute counts over the benchmark's run,
	// not rates. Earlier labels said "/sec" which made identical numbers
	// from a 1s and a 10s run look the same.
	b.ReportMetric(float64(snapshotsTaken.Load()), "snapshots")
	b.ReportMetric(float64(snapshotsWritten.Load()), "writes")
}

// buildPersistTestPoolN is a sibling of buildPersistTestPool that takes a
// numeric upstream count so the benchmark can scale the pool size up.
func buildPersistTestPoolN(b *testing.B, n int) *upstream.Pool {
	b.Helper()
	entries := make([]upstream.PoolEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, upstream.PoolEntry{
			Up:       &stubUpstream{id: fmt.Sprintf("up-%02d", i)},
			Priority: 100 + i,
		})
	}
	pool, err := upstream.NewPool(entries, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatalf("upstream.NewPool: %v", err)
	}
	return pool
}

// _ keeps sync referenced (used by other tests in the package) without
// breaking goimports if the benchmark gets temporarily commented out.
var _ sync.Mutex
