package failure_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/pacnpal/tunnelsmith/internal/failure"
)

func TestIsConnectionRefused(t *testing.T) {
	t.Parallel()

	t.Run("real refused dial", func(t *testing.T) {
		t.Parallel()
		// Bind a listener and immediately close it so the port is in
		// TIME_WAIT-free "nothing listening" state, then dial it.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := l.Addr().String()
		_ = l.Close()

		// Bound the dial in case the OS does something odd; refused
		// returns near-instantly on Linux and macOS.
		d := &net.Dialer{Timeout: 2 * time.Second}
		c, err := d.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
			t.Fatalf("expected refused dial, got connection")
		}
		if !failure.IsConnectionRefused(err) {
			t.Fatalf("IsConnectionRefused(%v) = false, want true", err)
		}
	})

	t.Run("wrapped errno", func(t *testing.T) {
		t.Parallel()
		err := fmt.Errorf("dial: %w", &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED})
		if !failure.IsConnectionRefused(err) {
			t.Fatalf("IsConnectionRefused on wrapped errno = false, want true")
		}
	})

	t.Run("nil and unrelated", func(t *testing.T) {
		t.Parallel()
		if failure.IsConnectionRefused(nil) {
			t.Fatal("IsConnectionRefused(nil) = true, want false")
		}
		if failure.IsConnectionRefused(io.EOF) {
			t.Fatal("IsConnectionRefused(io.EOF) = true, want false")
		}
		if failure.IsConnectionRefused(context.Canceled) {
			t.Fatal("IsConnectionRefused(context.Canceled) = true, want false")
		}
	})
}

func TestIsTimeout(t *testing.T) {
	t.Parallel()

	t.Run("context deadline exceeded", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		if !failure.IsTimeout(ctx.Err()) {
			t.Fatalf("IsTimeout(ctx.Err()=%v) = false, want true", ctx.Err())
		}
	})

	t.Run("net.Error with Timeout()=true", func(t *testing.T) {
		t.Parallel()
		// Earlier versions of this test dialed 192.0.2.1 (TEST-NET-1)
		// expecting a hard timeout, but some networks reply with
		// "network unreachable" before the dialer's deadline fires,
		// making the test flaky. A synthetic net.Error exercises the
		// errors.As branch deterministically.
		err := &syntheticTimeoutError{}
		if !failure.IsTimeout(err) {
			t.Fatalf("IsTimeout(synthetic timeout) = false, want true")
		}
		// Wrapped via fmt.Errorf %w should still classify.
		wrapped := fmt.Errorf("dial: %w", err)
		if !failure.IsTimeout(wrapped) {
			t.Fatalf("IsTimeout(wrapped synthetic timeout) = false, want true")
		}
	})

	t.Run("os deadline exceeded", func(t *testing.T) {
		t.Parallel()
		err := fmt.Errorf("read: %w", os.ErrDeadlineExceeded)
		if !failure.IsTimeout(err) {
			t.Fatalf("IsTimeout(wrapped os.ErrDeadlineExceeded) = false, want true")
		}
	})

	t.Run("nil and unrelated", func(t *testing.T) {
		t.Parallel()
		if failure.IsTimeout(nil) {
			t.Fatal("IsTimeout(nil) = true, want false")
		}
		if failure.IsTimeout(io.EOF) {
			t.Fatal("IsTimeout(io.EOF) = true, want false")
		}
		if failure.IsTimeout(errors.New("plain error")) {
			t.Fatal("IsTimeout(plain error) = true, want false")
		}
	})
}

// syntheticTimeoutError implements net.Error with Timeout() == true so
// IsTimeout's errors.As branch can be exercised without a network call.
type syntheticTimeoutError struct{}

func (syntheticTimeoutError) Error() string   { return "synthetic i/o timeout" }
func (syntheticTimeoutError) Timeout() bool   { return true }
func (syntheticTimeoutError) Temporary() bool { return true }
