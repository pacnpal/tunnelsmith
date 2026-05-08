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

	t.Run("net dial timeout", func(t *testing.T) {
		t.Parallel()
		// 192.0.2.1 is TEST-NET-1; nothing routes there, so the dial
		// hits the dialer's hard timeout instead of refusing.
		d := &net.Dialer{Timeout: 50 * time.Millisecond}
		c, err := d.Dial("tcp", "192.0.2.1:65000")
		if err == nil {
			_ = c.Close()
			t.Skip("dial unexpectedly succeeded; environment is too clever for this test")
		}
		if !failure.IsTimeout(err) {
			t.Fatalf("IsTimeout(net dial timeout %v) = false, want true", err)
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
