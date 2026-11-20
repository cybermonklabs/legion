//go:build linux

package launcher

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A role's redial is the connection the daemon end takes after the role's close, past every
// connection another process opened ahead of it. Another process can reach the listener: the
// kernel hands it a loopback port that a client of another test binary may still redial, and
// such a client opens without the launcher's hello, often with nothing at all. Here five such
// connections stand ahead of the first role's redial, the last of them opened just after the
// close, a second before the launcher redials (reconnectDelay): one that opens and sends nothing,
// one that opens and closes at once, an HTTP request, a hello under another launcher's token and
// one more that sends nothing. The daemon end takes the role's own redial, the launcher it was
// before, and the hello under another token is closed with no acknowledgement.
//
// This file holds only what the rig had before its daemon end read each connection's opening
// (newFleetDialed, acceptRole, the fleet's listener), so the same case runs against that rig.
func TestARolesRedialIsTakenPastTheConnectionsAheadOfIt(t *testing.T) {
	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	for _, role := range podRoles {
		pod.role(role).acknowledge()
	}
	first := pod.role(podRoles[0]).hello
	address := pod.listener.Addr().String()
	stray := func(open func(net.Conn) error) net.Conn {
		t.Helper()
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := open(conn); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	silent := func(net.Conn) error { return nil }
	stray(silent)
	stray(func(conn net.Conn) error { return conn.Close() })
	stray(func(conn net.Conn) error {
		_, err := io.WriteString(conn, "GET /computeMetadata/v1/instance/service-accounts/default/identity HTTP/1.1\r\nHost: metadata\r\n\r\n")
		return err
	})
	another := stray(func(conn net.Conn) error {
		return shimwire.NewWriter(conn).WriteFrame(shimwire.LauncherHello{Token: "another-launchers-token", Sandbox: first.Sandbox,
			Role: podRoles[0], PodUID: first.PodUID, LauncherID: "another-launcher"})
	})
	_ = pod.role(podRoles[0]).conn.Close()
	stray(silent)

	again := acceptRole(t, pod.listener)
	if again.hello.Role != podRoles[0] || again.hello.Token != first.Token || again.hello.LauncherID != first.LauncherID {
		t.Fatalf("the daemon end took %#v, want the redial of %s as launcher %s", again.hello, podRoles[0], first.LauncherID)
	}
	again.acknowledge()
	pod.conns[podRoles[0]] = again

	if err := another.SetReadDeadline(time.Now().Add(testwait.Bounded(t, time.Minute, time.Second))); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(another).ReadBytes('\n'); err != io.EOF || len(line) != 0 {
		t.Fatalf("the hello under another launcher's token was answered %q (%v), want its connection closed with no acknowledgement", line, err)
	}
}
