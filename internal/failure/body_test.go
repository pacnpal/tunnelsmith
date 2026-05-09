package failure

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

// regexpPattern adapts *regexp.Regexp to the Pattern interface.
type regexpPattern struct{ *regexp.Regexp }

func (p regexpPattern) Match(b []byte) bool { return p.Regexp.Match(b) }
func (p regexpPattern) String() string      { return p.Regexp.String() }

func mustPatterns(t *testing.T, srcs ...string) []Pattern {
	t.Helper()
	out := make([]Pattern, 0, len(srcs))
	for _, s := range srcs {
		re, err := regexp.Compile(s)
		if err != nil {
			t.Fatalf("compile %q: %v", s, err)
		}
		out = append(out, regexpPattern{re})
	}
	return out
}

func TestBufferAndDecideMatchSetsMatched(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader("Sorry, this content is not available in your region. (long body...)"))
	pats := mustPatterns(t, "(?i)content is not available")
	dec, err := BufferAndDecide(body, "", 256, pats)
	if err != nil {
		t.Fatalf("BufferAndDecide: %v", err)
	}
	if !dec.Matched {
		t.Fatal("Matched = false, want true")
	}
	if dec.Skipped {
		t.Error("Skipped = true on a real match")
	}
	if !strings.Contains(dec.Pattern, "content is not available") {
		t.Errorf("Pattern = %q, want one with 'content is not available'", dec.Pattern)
	}
	// Replay still readable and includes the matched bytes; verify
	// the prefix is intact so the caller can drain or forward it.
	got, err := io.ReadAll(dec.Replay)
	if err != nil {
		t.Fatalf("Replay read: %v", err)
	}
	if !strings.Contains(string(got), "content is not available") {
		t.Errorf("Replay missing matched substring: %q", string(got))
	}
	if err := dec.Replay.Close(); err != nil {
		t.Errorf("Replay close: %v", err)
	}
}

func TestBufferAndDecideNoMatchKeepsBody(t *testing.T) {
	t.Parallel()
	const payload = "ordinary HTML content with no flagged patterns at all"
	body := io.NopCloser(strings.NewReader(payload))
	pats := mustPatterns(t, "geo.?block", "region.?lock")
	dec, err := BufferAndDecide(body, "identity", 256, pats)
	if err != nil {
		t.Fatalf("BufferAndDecide: %v", err)
	}
	if dec.Matched {
		t.Fatal("Matched = true on benign body")
	}
	if dec.Skipped {
		t.Error("Skipped = true; want false (we did inspect)")
	}
	got, err := io.ReadAll(dec.Replay)
	if err != nil {
		t.Fatalf("Replay read: %v", err)
	}
	if string(got) != payload {
		t.Errorf("Replay = %q, want %q (byte-for-byte)", string(got), payload)
	}
}

func TestBufferAndDecideHonorsLimit(t *testing.T) {
	t.Parallel()
	// Pattern matches only past the limit; the inspector must NOT
	// detect because it never read those bytes.
	prefix := strings.Repeat("a", 64)
	tail := "MARKER"
	body := io.NopCloser(strings.NewReader(prefix + tail))
	pats := mustPatterns(t, "MARKER")
	dec, err := BufferAndDecide(body, "", 64, pats)
	if err != nil {
		t.Fatalf("BufferAndDecide: %v", err)
	}
	if dec.Matched {
		t.Error("Matched = true; pattern lay beyond the buffered limit")
	}
	got, err := io.ReadAll(dec.Replay)
	if err != nil {
		t.Fatalf("Replay read: %v", err)
	}
	if string(got) != prefix+tail {
		t.Errorf("Replay lost bytes past limit: got %q", string(got))
	}
}

func TestBufferAndDecideSkipsEncodedBody(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader("would-have-matched"))
	pats := mustPatterns(t, "would-have-matched")
	dec, err := BufferAndDecide(body, "gzip", 256, pats)
	if err != nil {
		t.Fatalf("BufferAndDecide: %v", err)
	}
	if dec.Matched {
		t.Error("Matched = true on gzip-encoded body; inspector must skip")
	}
	if !dec.Skipped {
		t.Error("Skipped = false; gzip body should not be inspected")
	}
	got, err := io.ReadAll(dec.Replay)
	if err != nil {
		t.Fatalf("Replay read: %v", err)
	}
	if string(got) != "would-have-matched" {
		t.Errorf("Replay = %q; encoded body must reach client byte-for-byte", string(got))
	}
}

func TestBufferAndDecideNoPatternsSkips(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader("hello"))
	dec, err := BufferAndDecide(body, "", 64, nil)
	if err != nil {
		t.Fatalf("BufferAndDecide: %v", err)
	}
	if !dec.Skipped {
		t.Error("Skipped = false on nil patterns")
	}
	if dec.Matched {
		t.Error("Matched = true on nil patterns")
	}
}

func TestBufferAndDecideNilBodySkips(t *testing.T) {
	t.Parallel()
	pats := mustPatterns(t, "anything")
	dec, err := BufferAndDecide(nil, "", 64, pats)
	if err != nil {
		t.Fatalf("BufferAndDecide: %v", err)
	}
	if !dec.Skipped {
		t.Error("Skipped = false; nil body should skip cleanly")
	}
	if dec.Replay == nil {
		t.Error("Replay = nil; want a non-nil reader")
	}
}

// errReader stalls inspection by returning an error before any byte
// reaches the buffer. The decision must surface the error and close
// the underlying body.
type errReader struct {
	err    error
	closed bool
}

func (e *errReader) Read(p []byte) (int, error) { return 0, e.err }
func (e *errReader) Close() error               { e.closed = true; return nil }

func TestBufferAndDecideReadErrorClosesBody(t *testing.T) {
	t.Parallel()
	want := errors.New("upstream read failed")
	er := &errReader{err: want}
	pats := mustPatterns(t, "x")
	_, err := BufferAndDecide(er, "", 64, pats)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if !er.closed {
		t.Error("body was not closed after read error")
	}
}

// chunkedReader hands out one byte at a time so io.LimitReader cannot
// short-circuit on the first Read; ensures the inspector still works
// when the upstream emits bytes in tiny chunks.
type chunkedReader struct{ data []byte }

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	p[0] = c.data[0]
	c.data = c.data[1:]
	return 1, nil
}
func (c *chunkedReader) Close() error { return nil }

func TestReplayReadAfterCloseReturnsErrClosedPipe(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader("ordinary content with no patterns to match"))
	pats := mustPatterns(t, "no.match.here")
	dec, err := BufferAndDecide(body, "", 256, pats)
	if err != nil {
		t.Fatalf("BufferAndDecide: %v", err)
	}
	if dec.Matched {
		t.Fatal("Matched = true on benign body")
	}
	if err := dec.Replay.Close(); err != nil {
		t.Fatalf("Replay close: %v", err)
	}
	buf := make([]byte, 8)
	n, err := dec.Replay.Read(buf)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Read after Close err = %v, want io.ErrClosedPipe", err)
	}
	if n != 0 {
		t.Errorf("Read after Close returned %d bytes, want 0", n)
	}
}

func TestBufferAndDecideHandlesChunkedReads(t *testing.T) {
	t.Parallel()
	body := &chunkedReader{data: []byte("region locked content here")}
	pats := mustPatterns(t, "region.?locked")
	dec, err := BufferAndDecide(body, "", 256, pats)
	if err != nil {
		t.Fatalf("BufferAndDecide: %v", err)
	}
	if !dec.Matched {
		t.Error("Matched = false; pattern was present in the chunked stream")
	}
}
