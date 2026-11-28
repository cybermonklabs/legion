// The leak check: the fixture reads a pooled server's client connections when a test takes the
// server and again when that test ends, so a connection that was not there at the start and is
// still there at the end fails the test that left it. Before this check the drain found such a
// connection at the start of the next test to take the server, failed that test after a
// thirty-second wait, and named nobody (dispatch://INFRA-1964).
//
// It is a file of its own because what it holds — the arming, the settle and the report — holds
// whatever answers which clients a server holds, and only clientConnections has to be re-pointed
// for another kind of server.
//
// The check holds each pooled server, and each server a test starts for itself
// (StartNkeyAuthorized, StartNkeyAuthorizedUsers) as well (armServerLeakCheck): such a server is
// removed when that test ends, but a client left on it outlives the test all the same. One that
// connects with ReconnectForever, as the daemon's workflow does, keeps dialing the server's address
// once it is gone, and a later server given its port would answer it. Where the Docker daemon is
// this machine, every server here listens on an address of its own (LoopbackIP), so that stray
// reaches none of them; where it is another machine (localDaemon), the servers share its default
// binding, and the check is what makes the client's own test fail for leaving it, either way. A
// pooled server's check depends as well on the pool's rule of one test on a server at a time and
// on this fixture's rule of one test binary at a time on an external server, which the package
// comment and skills/legion-worker/references/tools.md both state: every connection the server
// holds is the holding test's binary's, so one that appeared while a test held the server is that
// test's own.

package testnats

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// leakSettle bounds the wait for the ending test's connections to be gone, and leakPoll is how
// often the server is asked for them. A server drops a client's socket on a clock of its own
// rather than when that client's Close returns, and two of this fixture's callers kill a child
// process in a cleanup — the scratch Dispatch binary
// (internal/daemon/outbox_scratch_dispatch_test.go) and the Envoy listener
// (internal/daemon/idle_rig_listener_test.go) — so the server sees those sockets close after the
// cleanup that killed the process has run. The settle lengthens no bound of any test under it: a
// test that left nothing reads once and returns. A test's own server (armServerLeakCheck) shares
// this bound: on the dev box at a load near 520, its /connz stopped listing a client within 69 ms
// of the client's Close in each of 120 closes (median under 1 ms), well inside the 2 s.
const (
	leakSettle = 2 * time.Second
	leakPoll   = 50 * time.Millisecond
)

// leakMu guards each server's record (server.fixtureConns, leftOpen and leftOpenBy), which the test
// holding the server writes and the next test to take it reads.
var leakMu sync.Mutex

// fixtureOpenedNone forgets the connections the fixture opened on s for the test that has just
// released it, so the test taking it now starts owning none. URL calls it before it opens the
// reset's own client.
func fixtureOpenedNone(s *server) {
	leakMu.Lock()
	defer leakMu.Unlock()
	s.fixtureConns = nil
}

// fixtureOpened records conn as the fixture's own on s, so the leak check closes it at the end of
// the test it was opened for. Connect and connect call it with every connection they hand out on a
// pooled server.
func fixtureOpened(s *server, conn *nats.Conn) {
	leakMu.Lock()
	defer leakMu.Unlock()
	s.fixtureConns = append(s.fixtureConns, conn)
}

// cids is the set of connections open, by the id the server gave them, which is what the check
// compares: the server never reuses an id within its run, while a name is the client's own and
// two clients can carry one.
func cids(open []Connection) map[uint64]bool {
	ids := make(map[uint64]bool, len(open))
	for _, c := range open {
		ids[c.CID] = true
	}
	return ids
}

// armLeakCheck records which connections s holds now and registers the check that reads them
// again when t ends. URL calls it while the reset's own client is still open, and that client is
// the check's positive control: a reader that answered nothing whatever the server held would let
// every test pass, so the check refuses to arm unless it can see its own client.
func armLeakCheck(t testing.TB, s *server) {
	t.Helper()
	open, err := readConnections(s)
	if err != nil {
		t.Fatalf("read the NATS server's connections to arm the leak check: %v", err)
	}
	if !slices.ContainsFunc(open, func(c Connection) bool { return c.Name == t.Name() }) {
		t.Fatalf("the leak check cannot see its own client %q among the %d connections %v the NATS server reports: an empty answer at the end of %s would prove nothing about what it left open",
			t.Name(), len(open), open, t.Name())
	}
	armed := cids(open)
	t.Cleanup(func() { leakCheck(t, s, armed) })
}

// leakCheck is what the end of a test that took s runs. It is registered by URL, so every cleanup
// the test registered afterwards — the closes of its own clients among them — has run by the time
// it is called, cleanups running in reverse order of their registration.
func leakCheck(t testing.TB, s *server, armed map[uint64]bool) {
	read := func() ([]Connection, error) { return readConnections(s) }
	left, err := settled(read, armed)
	if err != nil {
		t.Errorf("read the NATS server's connections at the end of %s: %v", t.Name(), err)
	}
	if len(left) > 0 {
		t.Errorf("%s left %d client connection(s) %v open on its NATS server. One this fixture opened it closes now, and no test after this one is failed for it; one the fixture did not open nothing here can close, and the next test to take the server is failed in this test's name. A test closes every client it opens, and testnats.Connect closes the caller's for it",
			t.Name(), len(left), left)
	}
	// Recorded here, before the close below narrows it, because the read after that close can
	// fail: a record cleared by such a failure costs the next test the drain's whole
	// thirty-second bound and a failure that names nobody, which is the fault this check exists
	// to end (dispatch://INFRA-1964). The record holds the fixture's own clients as well as the
	// others until the read below narrows it, and that is what leftByTheLastTest is written
	// for: it asks only that every connection the next test finds is one of the recorded ids,
	// and the fixture's own are closed by then, so the next test does not find them.
	//
	// It is written whatever was found, so that the record always belongs to the check that
	// last ran: no path out of this function — the one where both reads fail among them, which
	// writes left as nil here and nothing after — can leave an earlier test's name standing for
	// the next test's failure to blame, and a wrong name is exactly what the message this check
	// exists to make right must not carry.
	recordLeft(s, t.Name(), left)
	closeFixtureConnections(s)
	if err == nil && len(left) == 0 {
		recordLeft(s, t.Name(), nil)
		return
	}
	// What the fixture opened is closed now, so what is still open is a connection it never
	// held: the daemon's own, a child process's, or a raw nats.Connect of the test's. NATS has
	// no call that closes another client's connection without a system account, which this
	// fixture's server does not configure, so the record is all the next test's drain can have.
	left, err = settled(read, armed)
	if err != nil {
		// The record made above stands. This read failing is exactly when the next test's drain
		// needs it, and narrowing it to nil here would leave that drain with nothing to name.
		t.Errorf("read the NATS server's connections after closing %s's fixture clients: %v", t.Name(), err)
		return
	}
	recordLeft(s, t.Name(), left)
}

// closeFixtureConnections closes every client the fixture opened on s for the test that is ending,
// and forgets them. A client the test already closed is closed again, which nats.go ignores.
func closeFixtureConnections(s *server) {
	leakMu.Lock()
	ours := s.fixtureConns
	s.fixtureConns = nil
	leakMu.Unlock()
	for _, conn := range ours {
		conn.Close()
	}
}

// recordLeft keeps what the ending test left open on s for drained to read at the start of the
// next test to take s, and forgets the last test's record when this test left nothing.
func recordLeft(s *server, test string, left []Connection) {
	leakMu.Lock()
	defer leakMu.Unlock()
	if len(left) == 0 {
		s.leftOpen, s.leftOpenBy = nil, ""
		return
	}
	s.leftOpen, s.leftOpenBy = left, test
}

// leftByTheLastTest names the last test that left connections on s its leak check could not close,
// "" when the last test left none, and reports whether every connection in found is one of
// them. The drain asks before it waits: nothing will close those connections, so waiting its
// bound out costs the next test thirty seconds and says no more than the leaker's own failure.
// It asks that of the connections found, not of the record, so a record that still holds the
// fixture's own clients — the leak check writes one before it closes them, and a failed read
// after the close leaves it standing — answers true all the same for what the next test finds:
// those clients are closed and the next test finds none of them.
func leftByTheLastTest(s *server, found []Connection) (string, bool) {
	leakMu.Lock()
	defer leakMu.Unlock()
	if s.leftOpenBy == "" || len(found) == 0 {
		return s.leftOpenBy, false
	}
	recorded := cids(s.leftOpen)
	for _, c := range found {
		if !recorded[c.CID] {
			return s.leftOpenBy, false
		}
	}
	return s.leftOpenBy, true
}

// settled waits up to leakSettle for the server read reads to hold no connection that was absent
// when the check was armed, and answers what is left with the last read's error.
func settled(read func() ([]Connection, error), armed map[uint64]bool) ([]Connection, error) {
	var left []Connection
	var err error
	testwait.Holds(leakSettle, leakPoll, func() bool {
		left, err = opened(read, armed)
		return err == nil && len(left) == 0
	})
	return left, err
}

// opened is what the server read reads holds that it did not hold when the check was armed.
func opened(read func() ([]Connection, error), armed map[uint64]bool) ([]Connection, error) {
	open, err := read()
	if err != nil {
		return nil, err
	}
	var left []Connection
	for _, c := range open {
		if !armed[c.CID] {
			left = append(left, c)
		}
	}
	return left, nil
}

// armServerLeakCheck records which connections a server t started for itself holds now, the
// server whose monitoring endpoint's base URL is monitor, and registers the check that reads them
// again when t ends: a connection absent now and still open leakSettle after the test's own
// cleanups have run fails t, naming each by id and name. StartNkeyAuthorizedUsers calls it once the
// server answers and after registering the container's removal, so the check runs before the
// container is removed and after every cleanup t registered later, its clients' closes among them.
// The server admits only the test's own nkey users, so the fixture holds no client of it to see,
// as the shared check sees its reset's; TestAClientLeftOnATestsOwnServerFailsTheCheck is the
// check's positive control.
func armServerLeakCheck(t testing.TB, monitor string) {
	t.Helper()
	read := func() ([]Connection, error) { return connectionsAt(monitor) }
	open, err := read()
	if err != nil {
		t.Fatalf("read the connections of %s's own NATS server to arm the leak check: %v", t.Name(), err)
	}
	armed := cids(open)
	t.Cleanup(func() {
		left, err := settled(read, armed)
		if err != nil {
			t.Errorf("read the connections of %s's own NATS server at its end: %v", t.Name(), err)
		}
		if len(left) > 0 {
			t.Errorf("%s left %d client connection(s) %v open on its own NATS server. A test closes every client it opens: one left open reconnects to the server's address after the server is gone",
				t.Name(), len(left), left)
		}
	})
}
