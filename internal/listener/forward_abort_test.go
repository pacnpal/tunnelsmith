package listener

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleForwardContextCanceledAbortsHandler(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/path", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h := &HTTPServer{
		retryCap: 1,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	defer func() {
		r := recover()
		if r != http.ErrAbortHandler {
			t.Fatalf("panic = %v, want %v", r, http.ErrAbortHandler)
		}
	}()

	h.handleForward(rec, req)
	t.Fatal("handleForward returned; want panic(http.ErrAbortHandler)")
}
