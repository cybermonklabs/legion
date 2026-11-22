package testport

import (
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
// process other than this one to bind, such as the host side of a port Docker publishes. Docker,
// asked for port 0 on an address, picks the port from its own record of what it has published
// there, and in envoy-infra-go it picked one already held, refusing the start with "failed to
// listen on TCP socket: address already in use". The kernel, asked for port 0, never hands out a
// port that is held.
// The probes are closed before LoopbackPorts returns, so another program can take one of the
// ports before the caller's process binds it: a caller retries a bind refused for that
// (AddressInUse) on a fresh address and fresh ports. That freed-port race is the class Refused
// closes for a peer that is not there; a port another process must bind cannot be held here.
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

// AddressInUse reports whether err is a bind refused because something already listens on the
// address, as Docker words it ("failed to listen on TCP socket: address already in use") and as
// the kernel does (EADDRINUSE).
func AddressInUse(err error) bool {
	return err != nil && strings.Contains(err.Error(), "address already in use")
}
