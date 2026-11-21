package testnats

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/testport"
)

// Gate is one TCP address a test holds for its whole life, which relays to srv while it is open
// and has nothing listening at all while it is closed.
//
// nats.go runs ReconnectErrCB only for a reconnect attempt whose dial failed: an attempt that
// connects and is then dropped is silent. The server's own relay accepts every connection while
// the server is stopped and closes it, so a test that needs a connected client's reconnect
// attempts to fail needs an address nothing listens on, and a test that then needs a successful
// reconnect needs that same address to come back once the server is up. The gate's open relay
// dials the server's URL, which names the server's relay and so survives Server.Stop and
// Server.Start.
type Gate struct {
	t       testing.TB
	address string
	server  string
	// claim and reservation hold the gate's port from StartGate to the test's end, open or closed
	// (holdGatePort).
	claim       net.Listener
	reservation int

	mu sync.Mutex
	// relay is the gate's listener and its copies while the gate is open, and nil while it is
	// closed.
	relay *relay
}

// While a gate is closed nothing listens at its address, since a listening or accepting port would
// let the test the gate serves pass while testing nothing. The port is never free all the same:
// from StartGate to the test's end the gate holds it twice over (holdGatePort).
//
//   - A reservation: a socket bound to the gate's address with SO_REUSEADDR that never listens
//     (testport.Reserve). A dial of it is refused, and the gate's listener binds beside it at each
//     Open, since Go's listeners set SO_REUSEADDR too. A program that binds the port without
//     SO_REUSEADDR is refused, closed gate or open.
//   - A claim: the abstract unix socket gateClaim(port), which one socket of the machine's network
//     namespace can hold at a time. Every gate's own sockets set SO_REUSEADDR, so the reservation
//     alone would let a second gate bind beside a closed one; the claim keeps every other gate off
//     the port.
//
// Both hold only on Linux. The reservation rests on Linux's bind rules (testport.Reserve), and a
// unix socket name that starts with @ is abstract only on Linux: elsewhere it is a file in the
// current directory that nothing unlinks. So StartGate refuses to run on any other system. Every
// place a Gate runs today is Linux: CI's ubuntu-24.04 runners, the development boxes, and Legion's
// worker pods, whose gVisor sandbox presents the Linux interface.
//
// What neither stops is another program that picks the number itself and binds it with
// SO_REUSEADDR. Docker does that for a container's published random host port inside the
// kernel's ephemeral range: it picks the number from its own allocator over that range and binds
// it with SO_REUSEADDR before it listens (moby daemon/libnetwork/portallocator,
// OSAllocator.RequestPortsInRange and bindTCPOrUDP). So a gate's port lies below that range, where
// no :0 bind, no connect and no Docker host port is given.
//
// That range is the machine's, not a constant: Linux's default is 32768-60999, and in a Legion
// worker pod (gVisor) it is 16000-65535. So the gate reads it from ephemeralRangePath and picks
// from the gatePortCount ports just below its first port (gatePorts).
const (
	ephemeralRangePath = "/proc/sys/net/ipv4/ip_local_port_range"
	// fallbackEphemeralLow is the first ephemeral port assumed when ephemeralRangePath cannot be
	// read or parsed: the lowest any machine this rig runs on has shown, so an unknown machine
	// gets the band that the most ranges leave alone rather than Linux's default.
	fallbackEphemeralLow = 16000
	gatePortCount        = 1000
	// gatePortFloor is the lowest port a gate's band may start at: below it lie the ports that
	// system services and fixed listeners take.
	gatePortFloor = 2048
)

// gatePorts returns the band a gate picks its port from, first through first+count-1, given text,
// the content of ephemeralRangePath: two decimal fields, the range's first port and its last. Text
// that is not such a range is read as a range starting at fallbackEphemeralLow. It returns an error
// naming the range when the band below its first port would start under gatePortFloor.
func gatePorts(text string) (first, count int, err error) {
	low := fallbackEphemeralLow
	if fields := strings.Fields(text); len(fields) == 2 {
		parsedLow, lowErr := strconv.Atoi(fields[0])
		parsedHigh, highErr := strconv.Atoi(fields[1])
		if lowErr == nil && highErr == nil && 0 < parsedLow && parsedLow <= parsedHigh && parsedHigh <= 65535 {
			low = parsedLow
		}
	}
	first = low - gatePortCount
	if first < gatePortFloor {
		return 0, 0, fmt.Errorf("the ephemeral range %q starts at port %d, so the %d ports below it would start at %d, under %d: no band there is free of both the kernel's own ports and the ones fixed services listen on", strings.TrimSpace(text), low, gatePortCount, first, gatePortFloor)
	}
	return first, gatePortCount, nil
}

// StartGate opens a gate on a port of the band gatePorts chooses from this machine's ephemeral
// range that no other gate and no listener holds, relaying to srv, and closes it and gives up the
// port when the test ends. It fails the test on a system other than Linux, before it claims
// anything (the comment above ephemeralRangePath).
func StartGate(t testing.TB, srv *Server) *Gate {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Fatalf("testnats: a gate holds its port with Linux's bind rules and an abstract unix socket, and this system is %s", runtime.GOOS)
		return nil
	}
	// An unreadable file is empty text, which gatePorts reads as the fallback range.
	text, _ := os.ReadFile(ephemeralRangePath)
	first, count, err := gatePorts(string(text))
	if err != nil {
		t.Fatalf("choose a gate port: %v", err)
		return nil
	}
	var last error
	for range 100 {
		port := first + rand.IntN(count)
		ip := testport.LoopbackIP()
		claim, reservation, err := holdGatePort(ip, port)
		if err != nil {
			last = err
			continue
		}
		g := &Gate{t: t, address: net.JoinHostPort(ip.String(), strconv.Itoa(port)), server: strings.TrimPrefix(srv.URL(), "nats://"), claim: claim, reservation: reservation}
		listener, err := net.Listen("tcp", g.address)
		if err != nil {
			g.release()
			last = err
			continue
		}
		g.relay = newRelay(listener, g.target, false)
		// Cleanups run last first: the gate closes, then gives up its port.
		t.Cleanup(g.release)
		t.Cleanup(g.Close)
		return g
	}
	t.Fatalf("hold a gate port: %v", last)
	return nil
}

// gateClaim is the abstract unix socket a gate on port holds for its whole life, so no other gate
// of the machine's network namespace takes the port.
func gateClaim(port int) string { return "@legion-testnats-gate-" + strconv.Itoa(port) }

// holdGatePort takes port for a gate at ip, a loopback address of its own (testport.LoopbackIP):
// its claim, then its reservation at ip, as the comment above ephemeralRangePath describes. It
// fails when another gate holds the claim, or when a socket already on the address refuses the
// reservation: one that listens, or one without SO_REUSEADDR.
func holdGatePort(ip net.IP, port int) (net.Listener, int, error) {
	claim, err := net.Listen("unix", gateClaim(port))
	if err != nil {
		return nil, -1, fmt.Errorf("claim port %d: %w", port, err)
	}
	reservation, _, err := testport.Reserve(&syscall.SockaddrInet4{Port: port, Addr: [4]byte(ip.To4())})
	if err != nil {
		_ = claim.Close()
		return nil, -1, fmt.Errorf("reserve %s:%d: %w", ip, port, err)
	}
	return claim, reservation, nil
}

// release gives up the gate's port: its reservation and its claim.
func (g *Gate) release() {
	_ = syscall.Close(g.reservation)
	_ = g.claim.Close()
}

// target is the address the gate's relay dials: the server's relay, whose port the server's URL
// names for the test's life.
func (g *Gate) target() (string, error) { return g.server, nil }

// URL is the client URL of the gate's address, which is the same for the test's whole life.
func (g *Gate) URL() string { return "nats://" + g.address }

// Close stops listening, so every dial of the gate's address is refused; the gate keeps its port
// (holdGatePort). It does nothing when the gate is already closed. It does not break a connection
// already joined through the gate, which keeps copying until one side ends, so a test that needs a
// connected client to lose its connection stops the server too.
func (g *Gate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.relay == nil {
		return
	}
	g.relay.Close()
	g.relay = nil
}

// Open listens on the gate's address again, beside its reservation, so dials reach the server once
// more. It does nothing when the gate is already open, and fails the test when the address cannot
// be taken.
func (g *Gate) Open() {
	g.t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.relay != nil {
		return
	}
	listener, err := net.Listen("tcp", g.address)
	if err != nil {
		g.t.Fatalf("open the gate at %s: %v", g.address, err)
		return
	}
	g.relay = newRelay(listener, g.target, false)
}
