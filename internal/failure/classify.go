// Package failure classifies dial and request errors into the kinds the
// scoreboard cares about. Phase 3 ships the two simplest classifiers,
// IsConnectionRefused and IsTimeout, that the priority pool's retry
// behavior is built on. Later phases add status-code, body-regex, and
// kind-aware penalty mapping.
package failure

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
)

// IsConnectionRefused reports whether err carries a TCP connection-refused
// signal (POSIX ECONNREFUSED). Walks wrapped errors so it works against
// the typical *net.OpError -> *os.SyscallError -> syscall.Errno chain
// returned by net.Dial and friends.
func IsConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

// IsTimeout reports whether err represents a timeout. Recognizes context
// deadlines, os.ErrDeadlineExceeded (raised by Conn read/write deadlines),
// and any error implementing the net.Error interface with Timeout() == true.
// The three sources cover every timeout shape Go's standard library can
// surface from a dial or read.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}
