// body.go owns the response-body inspector Phase 8 introduced for
// soft geo-block detection. Many destinations return HTTP 200 with a
// "this content is not available in your region" page; status-code
// detection cannot see those, but a regex over the first few kilobytes
// of the response body can. The inspector buffers a bounded prefix,
// runs caller-supplied patterns against the buffered bytes, and
// returns either a match decision plus a replay reader (so a benign
// body still streams to the client uncorrupted) or a discard signal.
//
// The inspector is plain-HTTP only. CONNECT and SOCKS5 traffic carries
// TLS payloads the proxy cannot decrypt; the listener guards body
// inspection at its call site so this package never has to think about
// transport. Likewise, content encodings (gzip, br, ...) are out of
// scope for v1: BufferedReader honors Content-Encoding by skipping
// inspection entirely when the body is encoded, so a regex is never
// run against bytes it cannot interpret.

package failure

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// BodyInspectionDecision is the outcome of one BufferAndDecide call.
// Matched=true means at least one pattern fired and the request should
// be retried through the next-best upstream; Pattern is the first
// matching regex's source string, suitable for a log line. Matched=false
// means the buffered prefix was clean and the response should stream
// through to the client.
//
// Replay carries the bytes the inspector already consumed plus the
// rest of the upstream body, in order. The listener writes Replay to
// the client when Matched=false. When Matched=true the listener does
// not need Replay at all (it discards and retries) but Replay still
// closes resp.Body, so the caller should drain or close it to avoid
// leaking the upstream conn. Skipped=true means inspection was
// declined (no patterns, encoded body, ...); the listener treats it as
// "stream through" and writes Replay to the client, same as a clean
// match.
type BodyInspectionDecision struct {
	Matched bool
	Pattern string
	Skipped bool
	Replay  io.ReadCloser
}

// BufferAndDecide reads up to limit bytes from body, runs each
// non-nil pattern against the buffered prefix, and returns a decision
// with a Replay reader the caller can use to forward the body to the
// client when nothing matched.
//
// Behavior:
//   - body == nil or limit <= 0: no inspection, Replay is http.NoBody,
//     Skipped=true.
//   - encoding != "" and encoding != "identity": no inspection,
//     Replay reconstructs the original body, Skipped=true. The proxy
//     does not decompress before regex matching in v1.
//   - len(patterns) == 0: same as above, Skipped=true.
//   - otherwise: the first len(buffered) <= limit bytes are matched
//     against each pattern in order. On hit, Matched=true; on miss,
//     Matched=false. Replay is always set.
//
// Replay is built so a caller that copies it to the client sees the
// exact bytes the upstream wrote, in order, including any prefix the
// inspector consumed. Closing Replay also closes body.
func BufferAndDecide(body io.ReadCloser, encoding string, limit int, patterns []Pattern) (BodyInspectionDecision, error) {
	if body == nil || limit <= 0 || len(patterns) == 0 {
		// Nothing to inspect. Hand back a Replay that mirrors body
		// so the caller's downstream copy still works.
		return BodyInspectionDecision{
			Skipped: true,
			Replay:  passthrough(body),
		}, nil
	}
	if !isPlainEncoding(encoding) {
		// Body is gzip / br / deflate / etc. Skip inspection so a
		// regex never runs against bytes that mean nothing in their
		// raw form. Document the caveat in docs/configuration.md.
		return BodyInspectionDecision{
			Skipped: true,
			Replay:  passthrough(body),
		}, nil
	}

	// Read up to limit bytes. We deliberately use limit (not limit+1)
	// because the regex engine sees the same window the destination
	// would have served to the client; the goal is to detect on the
	// prefix the user can configure, not to peek beyond it.
	buf := make([]byte, 0, limit)
	prefix, err := io.ReadAll(io.LimitReader(body, int64(limit)))
	if err != nil {
		_ = body.Close()
		return BodyInspectionDecision{}, err
	}
	buf = append(buf, prefix...)

	for _, p := range patterns {
		if p == nil {
			continue
		}
		if p.Match(buf) {
			return BodyInspectionDecision{
				Matched: true,
				Pattern: p.String(),
				Replay:  replay(buf, body),
			}, nil
		}
	}
	return BodyInspectionDecision{
		Replay: replay(buf, body),
	}, nil
}

// Pattern abstracts the regex.Regexp surface BufferAndDecide needs.
// Tests can substitute a fake without depending on the regex package.
type Pattern interface {
	Match(b []byte) bool
	String() string
}

// passthrough wraps body so a nil input becomes http.NoBody. Avoids
// scattering nil checks through the listener path.
func passthrough(body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return http.NoBody
	}
	return body
}

// replay returns an io.ReadCloser that yields the buffered prefix
// followed by the rest of body. Closing the result closes body. The
// upstream body is owned by the returned reader after this call;
// callers must not read body directly afterwards.
func replay(prefix []byte, body io.ReadCloser) io.ReadCloser {
	return &replayReader{
		prefix: bytes.NewReader(prefix),
		rest:   body,
	}
}

type replayReader struct {
	prefix *bytes.Reader
	rest   io.ReadCloser
	closed bool
}

func (r *replayReader) Read(p []byte) (int, error) {
	if r.prefix != nil && r.prefix.Len() > 0 {
		n, err := r.prefix.Read(p)
		if err == io.EOF {
			err = nil
		}
		return n, err
	}
	if r.rest == nil {
		return 0, io.EOF
	}
	return r.rest.Read(p)
}

func (r *replayReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.rest == nil {
		return nil
	}
	return r.rest.Close()
}

// isPlainEncoding reports whether the Content-Encoding header indicates
// a body the inspector can read raw. Empty and "identity" qualify; any
// other value (gzip, br, deflate, compress, ...) defers to the client
// to decode.
func isPlainEncoding(enc string) bool {
	enc = strings.TrimSpace(strings.ToLower(enc))
	return enc == "" || enc == "identity"
}
