package scoreboard

import (
	"context"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Persistence layer added in Phase 7. The scoreboard state survives a
// restart by writing a gob snapshot to a configured path on a tick and
// at shutdown. The format is intentionally boring: a small fixed header
// (magic + uint32 version) followed by a gob-encoded snapshot struct.
// Atomic write goes through a temp file plus os.Rename so a crash
// mid-write cannot leave the path holding a half-written file.
//
// Pruning piggybacks on the same tick: zero-score entries older than
// PruneAfter are dropped, expired cascade entries are removed, and
// debounce keys older than 10 * DebounceWindow are evicted. The pass
// runs under the regular mu.Lock; with the entry counts the v1 scoping
// implies (homelab-scale, low thousands of entries) the pause is
// imperceptible. If profiling shows otherwise (#12), the prune pass can
// switch to copy-then-iterate at that point.

// persistMagic is the 4-byte file marker we write at the start of every
// snapshot. Distinct from any well-known format so a misconfigured path
// pointing at the wrong file fails fast at load time.
var persistMagic = [4]byte{'T', 'S', 'B', '1'}

// persistVersion bumps any time the snapshot wire format changes in a
// non-backward-compatible way. Load rejects mismatched versions without
// trying to upgrade in place; users can safely delete the file and the
// scoreboard rebuilds state from live traffic.
const persistVersion uint32 = 1

// PersistenceSink is the metrics surface the persistence loop calls when
// it commits a snapshot. metrics.Registry implements it; pass nil to
// disable emission.
type PersistenceSink interface {
	ObservePersistenceWrite(result string)
}

// snapshotFile is the exact wire shape gob encodes. New optional fields
// can be added at the end without bumping persistVersion as long as
// readers tolerate zero values for omitted fields.
type snapshotFile struct {
	WrittenAt time.Time
	Hosts     []hostSnapshot
	Cascade   []cascadeSnapshot
}

type hostSnapshot struct {
	Host    string
	Entries []entrySnapshot
}

type entrySnapshot struct {
	UpstreamID         string
	Score              float64
	CooldownUntil      time.Time
	LastSeen           time.Time
	GlobalSuccessCount uint64
	GlobalFailureCount uint64
}

type cascadeSnapshot struct {
	Host  string
	Until time.Time
}

// SaveSnapshot writes the scoreboard state to path atomically. The
// directory must exist; the file itself is created on first write. Errors
// from the encode or rename surface to the caller so the persistence loop
// can log them and report through the metrics sink.
func (s *Scoreboard) SaveSnapshot(path string) error {
	if path == "" {
		return errors.New("scoreboard: SaveSnapshot called with empty path")
	}
	snap := s.buildSnapshot()
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tunnelsmith-scoreboard-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp snapshot: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	// Header: 4-byte magic + 4-byte big-endian version.
	if _, err := tmp.Write(persistMagic[:]); err != nil {
		cleanup()
		return fmt.Errorf("write magic: %w", err)
	}
	var versionBuf [4]byte
	binary.BigEndian.PutUint32(versionBuf[:], persistVersion)
	if _, err := tmp.Write(versionBuf[:]); err != nil {
		cleanup()
		return fmt.Errorf("write version: %w", err)
	}
	enc := gob.NewEncoder(tmp)
	if err := enc.Encode(snap); err != nil {
		cleanup()
		return fmt.Errorf("encode snapshot: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close snapshot tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename snapshot %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// buildSnapshot copies the scoreboard's entry and cascade state into a
// stable, sortable shape suitable for gob encoding. Holds the read lock
// for the duration; entries are small structs so the copy is cheap.
func (s *Scoreboard) buildSnapshot() snapshotFile {
	now := s.clock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	hosts := make([]hostSnapshot, 0, len(s.entries))
	for host, perUp := range s.entries {
		entries := make([]entrySnapshot, 0, len(perUp))
		for id, e := range perUp {
			entries = append(entries, entrySnapshot{
				UpstreamID:         id,
				Score:              e.score,
				CooldownUntil:      e.cooldownUntil,
				LastSeen:           e.lastSeen,
				GlobalSuccessCount: e.globalSuccessCount,
				GlobalFailureCount: e.globalFailureCount,
			})
		}
		hosts = append(hosts, hostSnapshot{Host: host, Entries: entries})
	}
	cascade := make([]cascadeSnapshot, 0, len(s.cascade))
	for host, until := range s.cascade {
		if !until.After(now) {
			continue
		}
		cascade = append(cascade, cascadeSnapshot{Host: host, Until: until})
	}
	return snapshotFile{
		WrittenAt: now,
		Hosts:     hosts,
		Cascade:   cascade,
	}
}

// LoadSnapshot reads a snapshot from path and overlays it onto the
// scoreboard's in-memory state. Missing files are not an error: a fresh
// install legitimately has nothing to load. Format mismatches log via the
// scoreboard's logger and return a non-nil error so the caller can decide
// whether to bail.
func (s *Scoreboard) LoadSnapshot(path string) error {
	if path == "" {
		return errors.New("scoreboard: LoadSnapshot called with empty path")
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open snapshot %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var header [8]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return fmt.Errorf("read snapshot header: %w", err)
	}
	for i, b := range persistMagic {
		if header[i] != b {
			return fmt.Errorf("snapshot %s magic mismatch (file may be from a different tool)", path)
		}
	}
	gotVersion := binary.BigEndian.Uint32(header[4:])
	if gotVersion != persistVersion {
		return fmt.Errorf("snapshot %s version %d unsupported (this binary speaks %d)", path, gotVersion, persistVersion)
	}

	dec := gob.NewDecoder(f)
	var snap snapshotFile
	if err := dec.Decode(&snap); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	s.applySnapshot(snap)
	return nil
}

// applySnapshot replaces the scoreboard's entries and cascade map with the
// shapes encoded in snap. Debounce state is intentionally not persisted:
// it is short-lived (~100ms) and rebuilds itself from live traffic.
func (s *Scoreboard) applySnapshot(snap snapshotFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]map[string]*entry, len(snap.Hosts))
	for _, h := range snap.Hosts {
		perUp := make(map[string]*entry, len(h.Entries))
		for _, e := range h.Entries {
			perUp[e.UpstreamID] = &entry{
				score:              e.Score,
				cooldownUntil:      e.CooldownUntil,
				lastSeen:           e.LastSeen,
				globalSuccessCount: e.GlobalSuccessCount,
				globalFailureCount: e.GlobalFailureCount,
			}
		}
		if len(perUp) > 0 {
			s.entries[h.Host] = perUp
		}
	}
	s.cascade = make(map[string]time.Time, len(snap.Cascade))
	for _, c := range snap.Cascade {
		s.cascade[c.Host] = c.Until
	}
}

// PruneStats counts what the prune pass dropped on its most recent run.
// Useful for tests and logs; the metrics sink does not currently track it.
type PruneStats struct {
	EntriesDropped  int
	HostsDropped    int
	CascadeDropped  int
	DebounceDropped int
}

// Prune drops scoreboard state that is no longer load-bearing:
//
//   - entries with score == 0 and lastSeen older than PruneAfter
//   - per-host maps that became empty after entry pruning
//   - cascade entries whose expiry is in the past
//   - debounce keys older than 10 * DebounceWindow
//
// PruneAfter <= 0 disables entry pruning (cascade and debounce still run,
// since they are bounded short-lived state). DebounceWindow <= 0 leaves
// debounce keys untouched.
//
// Holds Scoreboard.mu for write while iterating entries and cascade.
// Debounce uses its own mutex.
func (s *Scoreboard) Prune() PruneStats {
	now := s.clock()
	pruneAfter := s.cfg.PruneAfter
	stats := PruneStats{}

	s.mu.Lock()
	for host, perUp := range s.entries {
		if pruneAfter > 0 {
			for id, e := range perUp {
				if e.score == 0 && !e.lastSeen.IsZero() && now.Sub(e.lastSeen) > pruneAfter {
					delete(perUp, id)
					stats.EntriesDropped++
				}
			}
		}
		if len(perUp) == 0 {
			delete(s.entries, host)
			stats.HostsDropped++
		}
	}
	for host, until := range s.cascade {
		if !until.After(now) {
			delete(s.cascade, host)
			stats.CascadeDropped++
		}
	}
	s.mu.Unlock()

	if s.cfg.DebounceWindow > 0 {
		stale := 10 * s.cfg.DebounceWindow
		s.debounceMu.Lock()
		for k, last := range s.debounce {
			if now.Sub(last) > stale {
				delete(s.debounce, k)
				stats.DebounceDropped++
			}
		}
		s.debounceMu.Unlock()
	}

	return stats
}

// PersistenceConfig carries the runtime knobs the persistence loop reads.
// Path is required; Interval may be <= 0 to disable periodic writes (the
// loop still flushes once at shutdown when Path is set).
type PersistenceConfig struct {
	Path     string
	Interval time.Duration
}

// PersistenceLoop ticks every cfg.Interval, calling Prune followed by
// SaveSnapshot. On ctx cancellation the loop runs one final flush so a
// graceful shutdown does not lose the last few seconds of scoreboard
// activity. sink may be nil; it receives a "success" / "error" outcome
// per write.
//
// PersistenceLoop is blocking; callers run it in a goroutine (typically
// under the binary's errgroup). It returns nil on a clean shutdown.
type PersistenceLoop struct {
	sb     *Scoreboard
	cfg    PersistenceConfig
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Debug(msg string, args ...any)
	}
	sink PersistenceSink

	// guard ensures Run is only entered once per loop instance.
	guard sync.Mutex
	used  bool
}

// NewPersistenceLoop builds a loop that operates on sb. Logger is required
// (nil panics); sink may be nil.
func NewPersistenceLoop(sb *Scoreboard, cfg PersistenceConfig, logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
}, sink PersistenceSink) *PersistenceLoop {
	if logger == nil {
		panic("scoreboard: NewPersistenceLoop logger is nil")
	}
	return &PersistenceLoop{sb: sb, cfg: cfg, logger: logger, sink: sink}
}

// Run blocks until ctx is cancelled, ticking on cfg.Interval. On every
// tick the prune pass runs first, then the snapshot. ctx cancellation
// triggers one final flush before return.
func (l *PersistenceLoop) Run(ctx context.Context) error {
	l.guard.Lock()
	if l.used {
		l.guard.Unlock()
		return errors.New("scoreboard: persistence loop already used")
	}
	l.used = true
	l.guard.Unlock()

	if l.cfg.Path == "" {
		return errors.New("scoreboard: persistence loop requires non-empty path")
	}

	if l.cfg.Interval <= 0 {
		// Periodic writes disabled; still flush once at shutdown.
		<-ctx.Done()
		l.flushOnce("shutdown")
		return nil
	}

	ticker := time.NewTicker(l.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.flushOnce("shutdown")
			return nil
		case <-ticker.C:
			l.flushOnce("tick")
		}
	}
}

func (l *PersistenceLoop) flushOnce(reason string) {
	stats := l.sb.Prune()
	if stats.EntriesDropped+stats.HostsDropped+stats.CascadeDropped+stats.DebounceDropped > 0 {
		l.logger.Debug("scoreboard prune",
			"reason", reason,
			"entries_dropped", stats.EntriesDropped,
			"hosts_dropped", stats.HostsDropped,
			"cascade_dropped", stats.CascadeDropped,
			"debounce_dropped", stats.DebounceDropped,
		)
	}
	if err := l.sb.SaveSnapshot(l.cfg.Path); err != nil {
		l.logger.Warn("scoreboard snapshot failed",
			"reason", reason,
			"path", l.cfg.Path,
			"err", err,
		)
		if l.sink != nil {
			l.sink.ObservePersistenceWrite("error")
		}
		return
	}
	l.logger.Debug("scoreboard snapshot written",
		"reason", reason,
		"path", l.cfg.Path,
	)
	if l.sink != nil {
		l.sink.ObservePersistenceWrite("success")
	}
}

// EntriesCount returns the number of (host, upstream) entries currently
// tracked. Mostly useful for tests; production code goes through Snapshot.
func (s *Scoreboard) EntriesCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, perUp := range s.entries {
		total += len(perUp)
	}
	return total
}

// CooledHostsByUpstream returns a per-upstream count of hosts currently in
// cooldown. The metrics package consumes the same map shape on every
// scrape so /metrics stays consistent with what Prune saw.
func (s *Scoreboard) CooledHostsByUpstream() map[string]int {
	now := s.clock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]int)
	for _, perUp := range s.entries {
		for id, e := range perUp {
			if !e.cooldownUntil.IsZero() && e.cooldownUntil.After(now) {
				out[id]++
			}
		}
	}
	return out
}

// CascadeActiveCount returns the number of hosts currently in cascade.
func (s *Scoreboard) CascadeActiveCount() int {
	now := s.clock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, until := range s.cascade {
		if until.After(now) {
			count++
		}
	}
	return count
}
