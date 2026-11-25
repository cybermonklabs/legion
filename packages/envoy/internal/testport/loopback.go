package testport

import (
	"fmt"
	"math/rand/v2"
	"net"
	"runtime"
	"strings"
)

// LoopbackIP is an address for one test listener a client dials: on Linux, where every address of
// 127.0.0.0/8 is the loopback, one of them chosen at random and never 127.0.0.1, and elsewhere
// 127.0.0.1.
//
// A listener holds its port only while its test runs. A client that test leaves behind keeps
// dialing the address (a bus client reconnects forever), and the kernel or Docker can give the
// same port to a later listener, in the same test binary or in another of the binaries go test
// runs beside it. That listener's test then takes a stranger's connections for its own: bus
// clients that cmd/listener's tests left open went on dialing their servers' old addresses and
// reached the test servers of other packages' binaries minutes after their tests ended. The
// leftover client dials the address its own listener had, so a listener on an address of its own
// is out of its reach, whichever port it gets.
func LoopbackIP() net.IP {
	if runtime.GOOS != "linux" {
		return net.IPv4(127, 0, 0, 1).To4()
	}
	return net.IPv4(127, byte(1+rand.IntN(254)), byte(rand.IntN(256)), byte(1+rand.IntN(254))).To4()
}

// ListenLoopback listens on a port the kernel chooses at an address LoopbackIP picks.
func ListenLoopback() (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort(LoopbackIP().String(), "0"))
}

// LoopbackPorts answers an address LoopbackIP picks and n ports on it the kernel chose, for a
// process other than this one to bind, such as the host side of a port Docker publishes, where
// each is the first port of the range Docker publishes from (PublishedRange). Docker, asked for any
// port on an address, searches from the first port of its range it has not published on that
// address, which on a fresh address is the range's first port, held or not, and in envoy-infra-go
// it took one already held, refusing the start with "failed to listen on TCP socket: address
// already in use". The kernel, asked for port 0, never hands out a port that is held, so Docker's
// search starts at a port that was free a moment before. The probes are closed before
// LoopbackPorts returns, so a port can be taken before the caller's process binds it;
// PublishedRange says why a Docker publish still gets a port. That freed-port race is the class
// Refused closes for a peer that is not there; a port another process must bind cannot be held
// here.
func LoopbackPorts(n int) (string, []int, error) {
	host := LoopbackIP().String()
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	ports := make([]int, 0, n)
	for range n {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return "", nil, err
		}
		listeners = append(listeners, listener)
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
	}
	return host, ports, nil
}

// lastPublishedPort ends every range PublishedRange writes.
const lastPublishedPort = 65535

// PublishedRange is the host ports Docker may publish a container port on, from first, a port
// LoopbackPorts probed, through lastPublishedPort, written as a binding's HostPort. A probed port
// can be taken between the probe and the publish: by another program, or by Docker itself for a
// container another test publishes on every address. Given that one port, Docker refuses the
// start; Docker 29 refuses a port it holds on every address for any address, as "Bind for
// 0.0.0.0:<port> failed: port is already allocated", before it tries to bind (moby v29.9.3
// daemon/libnetwork/portallocator/portallocator.go, RequestPortsInRange). Given a range, Docker
// takes the first port of it that its own record shows free on the address and on every address,
// recording it as taken there, binds it, and while that bind fails, as on a port another program
// holds, goes on to the next, up to ten (osallocator_linux.go, maxAllocateAttempts). Its record
// holds a port from the moment it chooses it, so no other publish of Docker's takes it before the
// bind. A container's port is the one Docker reports, not the probed one.
func PublishedRange(first int) string { return fmt.Sprintf("%d-%d", first, lastPublishedPort) }

// AddressInUse reports whether err is a bind refused because something already listens on the
// address, as Docker 29 words it ("failed to bind host port 127.1.2.3:33000/tcp: address already
// in use"), as Docker 28 did when the listen failed ("failed to listen on TCP socket: address
// already in use"), and as the kernel does (EADDRINUSE).
func AddressInUse(err error) bool {
	return err != nil && strings.Contains(err.Error(), "address already in use")
}
