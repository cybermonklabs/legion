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
// such a client opens without the launcher's hello, often with nothing at all. Here six such
// connections stand ahead of the first role's redial, the last of them opened just after the
// close, a second before the launcher redials (reconnectDelay): one that opens and sends nothing,
// one that opens and closes at once, an HTTP request, the hello another rig's launcher of this
// package sent for that role while every rig's tokens were the package's constants, a hello under
// another launcher's token, and one more that sends nothing. The daemon end takes the role's own
// redial, the launcher it was before, and both hellos that are not its launcher's are closed with
// no acknowledgement.
//
// This file holds only what the rig had before its daemon end read each connection's opening
// (newFleetDialed, acceptRole, the fleet's listener), so the same case runs against that rig.
func TestARolesRedialIsTakenPastTheConnectionsAheadOfIt(t *testing.T) {
	// Not parallel: it writes the package variable roleHome (newFleetDialed), which parallel tests
	// read.
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
	otherRig := stray(func(conn net.Conn) error {
		return shimwire.NewWriter(conn).WriteFrame(shimwire.LauncherHello{Token: "launcher-token-" + podRoles[0], Sandbox: first.Sandbox,
			Role: podRoles[0], PodUID: first.PodUID, LauncherID: "another-rigs-launcher"})
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

	// Each read runs on a goroutine of its own, and the stray's cleanup closes the connection, so a
	// read that never ends is blocked only until then. The wait is testwait.Eventually, which the
	// rig before the daemon end had too, so this file still builds against that rig.
	for name, conn := range map[string]net.Conn{"another rig's launcher": otherRig, "another launcher's token": another} {
		type read struct {
			line []byte
			err  error
		}
		reads := make(chan read, 1)
		go func() {
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			reads <- read{line, err}
		}()
		var got read
		testwait.Eventually(t, "the close of the connection that opened with the hello of "+name, func() bool {
			select {
			case got = <-reads:
				return true
			default:
				return false
			}
		})
		if got.err != io.EOF || len(got.line) != 0 {
			t.Fatalf("the hello of %s was answered %q (%v), want its connection closed with no acknowledgement", name, got.line, got.err)
		}
	}
}
