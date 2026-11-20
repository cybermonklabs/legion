package stalledconnect

import (
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// Listen returns the address of a loopback listener whose accept queue is full, so a connect to
// it stays pending until the connecting side gives up. The listener gets a backlog of none, one
// connection Listen makes and never accepts fills its queue, and the listener's own count of
// connections waiting to be accepted (TCP_INFO's tcpi_unacked, beside its backlog in
// tcpi_sacked) says the queue is full before Listen returns. Nothing accepts, so it stays full
// until t ends, when both are closed.
func Listen(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	raw, err := listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatalf("the listener's socket: %v", err)
	}
	var listenErr error
	if err := raw.Control(func(fd uintptr) { listenErr = syscall.Listen(int(fd), 0) }); err != nil || listenErr != nil {
		t.Fatalf("giving the listener a backlog of none: %v, %v", err, listenErr)
	}
	queue := func() (waiting, backlog uint32) {
		t.Helper()
		var info *unix.TCPInfo
		var infoErr error
		if err := raw.Control(func(fd uintptr) {
			info, infoErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		}); err != nil || infoErr != nil {
			t.Fatalf("reading the listener's accept queue: %v, %v", err, infoErr)
		}
		return info.Unacked, info.Sacked
	}
	filler, err := net.DialTimeout("tcp", listener.Addr().String(), testwait.Bounded(t, time.Minute, time.Second))
	if err != nil {
		t.Fatalf("the connection that fills the listener's accept queue: %v", err)
	}
	t.Cleanup(func() { _ = filler.Close() })
	testwait.Eventually(t, "the listener's accept queue to be full", func() bool {
		waiting, backlog := queue()
		return waiting > backlog
	})
	return listener.Addr().String()
}
