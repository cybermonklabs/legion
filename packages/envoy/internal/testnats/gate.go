package testnats

import (
	"strings"
	"sync"
	"testing"
)

// Gate is one TCP address a test holds for its whole life, which relays to srv while it is open
// and has nothing listening there while it is closed.
//
// nats.go runs ReconnectErrCB only for a reconnect attempt whose dial failed: an attempt that
// connects and is then dropped is silent. The server's own relay accepts every connection while
// the server is stopped and closes it, so a test that needs a connected client's reconnect
// attempts to fail needs an address nothing listens on, and a test that then needs a successful
// reconnect needs that same address to come back once the server is up. The gate's open relay
// dials the server's URL, which names the server's relay and so survives Server.Stop and
// Server.Start.
//
// The gate holds its port from StartGate to the test's end with a socket bound there that never
// listens (holdPort). While the gate is open, a second socket listens beside it at the same
// address (listenBesideHold), and Close closes only that listener. Both set SO_REUSEPORT, with
// which Linux and Darwin let a socket bind an address another socket of the same user holds when
// both set it. The hold sets no SO_REUSEADDR: on Linux, two sockets that both set it may bind one
// address while neither listens, and every Go listener sets it, as Docker's bind of a container's
// published host port does. So while the gate is closed, no bind without SO_REUSEPORT takes its
// port, another gate's hold included, and no bind of port 0 is handed it, which is why the hold
// can take a port the kernel chooses. The limit: a program of the same user that sets SO_REUSEPORT
// too can still bind the port, and while the gate is open, share its connections.
//
// What a dial of a closed gate meets is the kernel's. Linux answers it with a reset, since no
// socket listens, so the dial is refused at once (ECONNREFUSED). Darwin's kernel looks the dial's
// SYN up, finds the hold, a socket that does not listen, and drops the SYN unanswered (xnu's
// in_pcblookup_hash_locked and tcp_input), so there the dial fails at its own timeout. Either way
// the dial fails, which is what the gate's callers need.
type Gate struct {
	t       testing.TB
	address string
	server  string

	mu sync.Mutex
	// relay is the gate's listener and its copies while the gate is open, and nil while it is
	// closed.
	relay *relay
}

// StartGate holds a port the kernel chooses on 127.0.0.1 for the test's life and opens a gate
// there that relays to srv, which closes when the test ends.
func StartGate(t testing.TB, srv *Server) *Gate {
	t.Helper()
	address, release, err := holdPort()
	if err != nil {
		t.Fatalf("hold a gate port: %v", err)
		return nil
	}
	// Cleanups run last first, so the gate closes before its port is given up.
	t.Cleanup(release)
	g := &Gate{t: t, address: address, server: strings.TrimPrefix(srv.URL(), "nats://")}
	g.Open()
	t.Cleanup(g.Close)
	return g
}

// target is the address the gate's relay dials: the server's relay, whose port the server's URL
// names for the test's life.
func (g *Gate) target() (string, error) { return g.server, nil }

// URL is the client URL of the gate's address, which is the same for the test's whole life.
func (g *Gate) URL() string { return "nats://" + g.address }

// Close stops listening, so every dial of the gate's address fails, while the gate still holds its
// port (Gate). It does nothing when the gate is already closed. It does not break a connection
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

// Open listens on the gate's address again, beside the socket that holds it, so dials reach the
// server once more. It does nothing when the gate is already open, and fails the test when the
// listener cannot bind the address.
func (g *Gate) Open() {
	g.t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.relay != nil {
		return
	}
	listener, err := listenBesideHold(g.address)
	if err != nil {
		g.t.Fatalf("open the gate at %s: %v", g.address, err)
		return
	}
	// A gate's relay is no server's own client port, so a hold (Server.Reload) never stops it.
	g.relay = newRelay(listener, g.target, false)
}
