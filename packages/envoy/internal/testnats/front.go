package testnats

import (
	"net"
	"testing"
)

// Front is the held front of a server this package does not run, such as a nats-server binary a
// test starts as a process: a loopback port the test holds from StartFront until it ends, which
// relays each connection it accepts to wherever the server listens at that moment, as each
// Server's own URL does (relay). Such a server takes no socket from the test, so it binds a port
// the kernel chooses at each start (nats-server -p -1) and cannot keep one across a restart; the
// test gives every client the front's address instead, which no other program can take while the
// test runs. A connection made while the server listens nowhere is closed at once, as a refused
// dial is, and one the server drops is dropped on the client's side too.
type Front struct {
	addr  string
	relay *relay
}

// StartFront holds a loopback port the kernel chooses and relays each connection it accepts to the
// address target returns as the connection arrives, until the test ends. target returns an error
// while the server is not listening, and the front then closes the connection. A caller stops the
// target naming a server before it stops that server, so no connection is relayed to a port the
// server has given up, which another program can bind once it has.
func StartFront(t testing.TB, target func() (string, error)) *Front {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a front's port: %v", err)
	}
	// A front is no server's own client port, so a hold (Server.Reload) never stops it.
	f := &Front{addr: listener.Addr().String(), relay: newRelay(listener, target, false)}
	t.Cleanup(f.relay.Close)
	return f
}

// Addr is the address the front holds, 127.0.0.1 and its port.
func (f *Front) Addr() string { return f.addr }
