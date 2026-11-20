package testport

import (
	"net"
	"runtime"
	"testing"
	"time"
)

// strangerReaches dials 127.0.0.1 at listener's port, as a client of another test binary that was
// handed that port once dials it, and reports whether listener accepted the connection. A dial of
// 127.0.0.1 can connect to another process's socket that holds the same port number there, so the
// answer is listener's own accept, never the dial alone.
func strangerReaches(t *testing.T, listener net.Listener) bool {
	t.Helper()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("read the port of %s: %v", listener.Addr(), err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	stranger, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), time.Second)
	if err != nil {
		return false
	}
	defer stranger.Close()
	select {
	case conn := <-accepted:
		_ = conn.Close()
		return true
	case <-time.After(time.Second):
		return false
	}
}

// A listener Listen opens is on this binary's own loopback address and not on 127.0.0.1, and a
// stranger's dial of 127.0.0.1 at its port does not reach it. Every relay, proxy and fake server a
// test starts in front of a NATS server or a database, or in place of one, opens through Listen,
// and the check in internal/textcheck (loopback_listeners_test.go) fails on one that listens on a
// fixed address instead; so this row is the run-time half of that check for every such listener.
// Linux serves all of 127.0.0.0/8; another system may serve 127.0.0.1 alone, where Loopback is
// 127.0.0.1 and no address of this binary's own exists.
func TestAListenerOnThisBinarysLoopbackIsNotReachedByAStrangersDialOf127001(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("only Linux serves every address of 127.0.0.0/8, and this system is %s", runtime.GOOS)
	}
	if Loopback == "127.0.0.1" {
		t.Fatalf("Loopback = %s on Linux, which serves all of 127.0.0.0/8: want an address of this binary's own", Loopback)
	}
	listener, err := Listen()
	if err != nil {
		t.Fatalf("listen on this binary's loopback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if host, _, err := net.SplitHostPort(listener.Addr().String()); err != nil || host != Loopback {
		t.Fatalf("Listen opened %s, want a port at %s", listener.Addr(), Loopback)
	}
	if strangerReaches(t, listener) {
		t.Fatalf("a stranger's dial of 127.0.0.1 at the port of %s reached the listener", listener.Addr())
	}
}

// The control of the row above: the same dial reaches a listener on 127.0.0.1, so a listener the
// row finds unreached was out of the stranger's reach, not out of the row's sight.
func TestAStrangersDialOf127001ReachesAListenerOn127001(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on 127.0.0.1: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if !strangerReaches(t, listener) {
		t.Fatalf("a dial of 127.0.0.1 at the port of %s did not reach the listener there", listener.Addr())
	}
}
