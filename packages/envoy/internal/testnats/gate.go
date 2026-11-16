package testnats

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
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

	mu sync.Mutex
	// relay is the gate's listener and its copies while the gate is open, and nil while it is
	// closed.
	relay *relay
}

// A gate's port is one no :0 bind can be handed. While the gate is closed nothing listens at its
// address, and it cannot hold the port instead: nats.go runs ReconnectErrCB only for a reconnect
// whose dial failed, so a held or accepting port would let the test the gate serves pass while
// testing nothing. Its port is therefore free whenever it is closed, and every :0 bind on the
// machine - each relay of this rig, each server's client and monitoring listener at every start,
// another test binary's - takes its port from the kernel's ephemeral range, so a gate port inside
// that range can be taken while the gate is closed, and Open then fails or the client's dials
// reach someone else's listener.
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

// StartGate opens a gate on a free port of the band gatePorts chooses from this machine's
// ephemeral range, relaying to srv, and closes it when the test ends.
func StartGate(t testing.TB, srv *Server) *Gate {
	t.Helper()
	// An unreadable file is empty text, which gatePorts reads as the fallback range.
	text, _ := os.ReadFile(ephemeralRangePath)
	first, count, err := gatePorts(string(text))
	if err != nil {
		t.Fatalf("choose a gate port: %v", err)
		return nil
	}
	g := &Gate{t: t, server: strings.TrimPrefix(srv.URL(), "nats://")}
	var last error
	for range 100 {
		address := fmt.Sprintf("127.0.0.1:%d", first+rand.IntN(count))
		listener, err := net.Listen("tcp", address)
		if err != nil {
			last = err
			continue
		}
		g.address = address
		g.relay = newRelay(listener, g.target, false)
		t.Cleanup(g.Close)
		return g
	}
	t.Fatalf("hold a gate port: %v", last)
	return nil
}

// target is the address the gate's relay dials: the server's relay, whose port the server's URL
// names for the test's life.
func (g *Gate) target() (string, error) { return g.server, nil }

// URL is the client URL of the gate's address, which is the same for the test's whole life.
func (g *Gate) URL() string { return "nats://" + g.address }

// Close stops listening, so every dial of the gate's address is refused. It does nothing when the
// gate is already closed. It does not break a connection already joined through the gate, which
// keeps copying until one side ends, so a test that needs a connected client to lose its
// connection stops the server too.
func (g *Gate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.relay == nil {
		return
	}
	g.relay.Close()
	g.relay = nil
}

// Open listens on the gate's address again, so dials reach the server once more. It does nothing
// when the gate is already open, and fails the test when the address cannot be taken.
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
