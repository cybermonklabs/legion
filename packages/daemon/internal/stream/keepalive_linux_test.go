//go:build linux

package stream

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// Every connection the worker stream's TCP listener accepts carries TCP keepalive and a
// TCP_USER_TIMEOUT, so a connection whose other end vanished — a node that went away, a network
// that stopped carrying its packets — ends within 3 minutes rather than holding the claim's
// stream for the retransmission timeout, about 15 minutes (R3). Go's own keepalive does not fire
// while a sent byte is unacknowledged, and the stop frame is such a byte, which is why the user
// timeout is set beside it as grpc-go does. A far end whose kernel still acknowledges, as a pod's
// sandbox that outlived its force delete does, is ended by the lock-out's silence instead
// (lockoutsilence_test.go).
//
// The numbers are read back off the accepted socket with getsockopt rather than from this
// package's constants: a test that read the constants would pass against a listener that sets
// nothing at all.
func TestAnAcceptedConnectionHasKeepaliveAndUserTimeout(t *testing.T) {
	h := startListener(t, harnessOptions{})
	h.connect(testToken)

	accepted, ok := h.conn().nc.(*net.TCPConn)
	if !ok {
		t.Fatalf("the accepted connection is a %T, want a TCP connection", h.conn().nc)
	}
	raw, err := accepted.SyscallConn()
	if err != nil {
		t.Fatalf("the accepted connection's descriptor: %v", err)
	}
	for _, option := range []struct {
		name  string
		level int
		opt   int
		want  int
	}{
		{"SO_KEEPALIVE", unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1},
		{"TCP_KEEPIDLE", unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, 15},
		{"TCP_KEEPINTVL", unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, 15},
		{"TCP_KEEPCNT", unix.IPPROTO_TCP, unix.TCP_KEEPCNT, 9},
		{"TCP_USER_TIMEOUT", unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, 120_000},
	} {
		var got int
		var readErr error
		if err := raw.Control(func(fd uintptr) {
			got, readErr = unix.GetsockoptInt(int(fd), option.level, option.opt)
		}); err != nil {
			t.Fatalf("read %s: %v", option.name, err)
		}
		if readErr != nil {
			t.Fatalf("read %s: %v", option.name, readErr)
		}
		if got != option.want {
			t.Errorf("%s = %d, want %d", option.name, got, option.want)
		}
	}
}
