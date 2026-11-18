// The leak check: when a test that took a server from URL or JetStream ends, that server must hold
// no client connection. The server is the test's alone and held none when URL started it, so every
// connection it holds at the end is one the test, or the code under it, opened and left open. The
// check runs in the cleanup URL registers first, so the test's own cleanups have run by then: the
// closes Connect registered, the stop of a daemon, the kill of a child process.
//
// It tells two kinds of client apart. A client the fixture opened (Connect, JetStream) closes when
// the test it was given ends, so one still connected here was given another test than the one whose
// server it reached, or a stand-in for one whose cleanups do not run: the check fails the test at
// once, naming it, and closes it. Any other client (the daemon's own, a child process's, a raw
// nats.Connect of the test's) is waited for through the shared wait (testwait.Until) until the
// server holds none, since a server drops a socket on a clock of its own rather than when the
// client's Close returns, and a child process killed in a cleanup closes its socket after that
// cleanup has returned. One still open when the wait gives up fails the test with its CID and name.
//
// A server a test starts with StartNkeyAuthorized or StartNkeyAuthorizedUsers has no check: its
// clients are the test's own nkey connections, which it closes as it ends, and the server goes
// with the test whatever it still holds.

package testnats

import (
	"fmt"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

var (
	// fixtureMu guards fixtureConns, which parallel tests share.
	fixtureMu sync.Mutex
	// fixtureConns are the clients Connect handed out that no check has yet found closed.
	fixtureConns []*nats.Conn
)

// fixtureOpened records conn as the fixture's own, so the leak check of the server it reaches knows
// it from a client the fixture did not open.
func fixtureOpened(conn *nats.Conn) {
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	fixtureConns = append(fixtureConns, conn)
}

// leakCheck is what the end of a test that took a server from URL runs, before the server shuts
// down: it fails t for each client of the fixture's still connected to the server, at once, and
// for each other client the server still holds when the shared wait gives up.
func (s *server) leakCheck(t testing.TB) {
	t.Helper()
	if left := s.closeFixtureClients(); len(left) > 0 {
		t.Errorf("testnats: %s ended with %d client(s) %v that testnats.Connect opened still connected to its NATS server. Connect closes a client when the test it was given ends, so each of these was given another test, or a stand-in for one whose cleanups do not run; the fixture has closed them now",
			t.Name(), len(left), left)
	}
	testwait.Until(t, fmt.Sprintf("the NATS server of %s to hold no client connection at the test's end (each one the last read names, by CID and name, is a client %s or the code under it left open)", t.Name(), t.Name()),
		func() ([]Connection, bool) {
			open := s.connections(t)
			return open, len(open) == 0
		})
}

// closeFixtureClients closes and returns, by CID and name, every client the fixture opened that is
// still connected to s, and forgets every fixture client found closed, so the record holds only
// clients still open.
func (s *server) closeFixtureClients() []Connection {
	id := s.srv.ID()
	fixtureMu.Lock()
	var connected []*nats.Conn
	kept := fixtureConns[:0]
	for _, conn := range fixtureConns {
		switch {
		case conn.IsClosed():
		case conn.ConnectedServerId() == id:
			connected = append(connected, conn)
		default:
			kept = append(kept, conn)
		}
	}
	clear(fixtureConns[len(kept):])
	fixtureConns = kept
	fixtureMu.Unlock()
	left := make([]Connection, 0, len(connected))
	for _, conn := range connected {
		cid, _ := conn.GetClientID()
		left = append(left, Connection{CID: cid, Name: conn.Opts.Name})
		conn.Close()
	}
	return left
}
