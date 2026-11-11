// The leak check: the fixture reads the shared server's client connections when a test takes the
// server and again when that test ends, so a connection that was not there at the start and is
// still there at the end fails the test that left it. Before this check the drain found such a
// connection at the start of the next test to take the server, failed that test after a
// thirty-second wait, and named nobody (dispatch://INFRA-1964).
//
// It is a file of its own because dispatch://INFRA-1524's rewrite of nats.go replaces the shared
// server with one server per test: the arming, the settle and the report here hold whatever
// answers which clients a server holds, and only clientConnections has to be re-pointed.
//
// The check holds for the shared server alone. A server a test starts for itself
// (StartNkeyAuthorized, StartNkeyAuthorizedUsers) is removed when that test ends, so a client
// left on it reaches no other test and there is no successor to blame. It depends as well on
// this fixture's rule of one test binary at a time, which the package comment and
// skills/legion-worker/references/tools.md both state: every connection the server holds is this
// binary's, so one that appeared while a test held the server is that test's own.

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
// test that left nothing reads once and returns.
const (
	leakSettle = 2 * time.Second
	leakPoll   = 50 * time.Millisecond
)

var (
	// leakMu guards both of the records below, which the test holding the server writes and the
	// next test to take it reads.
	leakMu sync.Mutex
	// fixtureConns are the connections the fixture opened for the test holding the server: its
	// reset client, and every client that test took from Connect. The leak check closes them, so
	// whatever is open after that is a connection the fixture does not hold.
	fixtureConns []*nats.Conn
	// leftOpen is what the last test's leak check found still open once it had closed the
	// fixture's own clients, and leftOpenBy names that test, for the next test's drain.
	leftOpen   []Connection
	leftOpenBy string
)

// fixtureOpenedNone forgets the connections the fixture opened for the test that has just
// released the server, so the test taking it now starts owning none. URL calls it before it
// opens the reset's own client.
func fixtureOpenedNone() {
	leakMu.Lock()
	defer leakMu.Unlock()
	fixtureConns = nil
}

// fixtureOpened records conn as the fixture's own, so the leak check closes it at the end of the
// test it was opened for. Connect and connect call it with every connection they hand out.
func fixtureOpened(conn *nats.Conn) {
	leakMu.Lock()
	defer leakMu.Unlock()
	fixtureConns = append(fixtureConns, conn)
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

// armLeakCheck records which connections the server holds now and registers the check that reads
// them again when t ends. URL calls it while the reset's own client is still open, and that
// client is the check's positive control: a reader that answered nothing whatever the server
// held would let every test pass, so the check refuses to arm unless it can see its own client.
func armLeakCheck(t testing.TB) {
	t.Helper()
	open, err := readConnections()
	if err != nil {
		t.Fatalf("read the NATS server's connections to arm the leak check: %v", err)
	}
	if !slices.ContainsFunc(open, func(c Connection) bool { return c.Name == t.Name() }) {
		t.Fatalf("the leak check cannot see its own client %q among the %d connections %v the NATS server reports: an empty answer at the end of %s would prove nothing about what it left open",
			t.Name(), len(open), open, t.Name())
	}
	armed := cids(open)
	t.Cleanup(func() { leakCheck(t, armed) })
}

// leakCheck is what the end of a test that took the server runs. It is registered by URL, so
// every cleanup the test registered afterwards — the closes of its own clients among them — has
// run by the time it is called, cleanups running in reverse order of their registration.
func leakCheck(t testing.TB, armed map[uint64]bool) {
	left, err := settled(armed)
	if err != nil {
		t.Errorf("read the NATS server's connections at the end of %s: %v", t.Name(), err)
	}
	if len(left) > 0 {
		t.Errorf("%s left %d client connection(s) %v open on the shared NATS server. One this fixture opened it closes now, and no test after this one is failed for it; one the fixture did not open nothing here can close, and the next test to take the server is failed in this test's name. A test closes every client it opens, and testnats.Connect closes the caller's for it",
			t.Name(), len(left), left)
		// Recorded here, before the close below narrows it, because the read after that close
		// can fail: a record cleared by such a failure costs the next test the drain's whole
		// thirty-second bound and a failure that names nobody, which is the fault this check
		// exists to end (dispatch://INFRA-1964). The record holds the fixture's own clients as
		// well as the others until the read below narrows it, and that is what leftByTheLastTest
		// is written for: it asks only that every connection the next test finds is one of the
		// recorded ids, and the fixture's own are closed by then, so the next test does not find
		// them.
		recordLeft(t.Name(), left)
	}
	closeFixtureConnections()
	if err == nil && len(left) == 0 {
		recordLeft(t.Name(), nil)
		return
	}
	// What the fixture opened is closed now, so what is still open is a connection it never
	// held: the daemon's own, a child process's, or a raw nats.Connect of the test's. NATS has
	// no call that closes another client's connection without a system account, which this
	// fixture's server does not configure, so the record is all the next test's drain can have.
	left, err = settled(armed)
	if err != nil {
		// The record made above stands. This read failing is exactly when the next test's drain
		// needs it, and narrowing it to nil here would leave that drain with nothing to name.
		t.Errorf("read the NATS server's connections after closing %s's fixture clients: %v", t.Name(), err)
		return
	}
	recordLeft(t.Name(), left)
}

// closeFixtureConnections closes every client the fixture opened for the test that is ending,
// and forgets them. A client the test already closed is closed again, which nats.go ignores.
func closeFixtureConnections() {
	leakMu.Lock()
	ours := fixtureConns
	fixtureConns = nil
	leakMu.Unlock()
	for _, conn := range ours {
		conn.Close()
	}
}

// recordLeft keeps what the ending test left open for drained to read at the start of the next
// test, and forgets the last test's record when this test left nothing.
func recordLeft(test string, left []Connection) {
	leakMu.Lock()
	defer leakMu.Unlock()
	if len(left) == 0 {
		leftOpen, leftOpenBy = nil, ""
		return
	}
	leftOpen, leftOpenBy = left, test
}

// leftByTheLastTest names the last test that left connections its leak check could not close,
// "" when the last test left none, and reports whether every connection in found is one of
// them. The drain asks before it waits: nothing will close those connections, so waiting its
// bound out costs the next test thirty seconds and says no more than the leaker's own failure.
// It asks that of the connections found, not of the record, so a record that still holds the
// fixture's own clients — the leak check writes one before it closes them, and a failed read
// after the close leaves it standing — answers true all the same for what the next test finds:
// those clients are closed and the next test finds none of them.
func leftByTheLastTest(found []Connection) (string, bool) {
	leakMu.Lock()
	defer leakMu.Unlock()
	if leftOpenBy == "" || len(found) == 0 {
		return leftOpenBy, false
	}
	recorded := cids(leftOpen)
	for _, c := range found {
		if !recorded[c.CID] {
			return leftOpenBy, false
		}
	}
	return leftOpenBy, true
}

// settled waits up to leakSettle for the server to hold no connection that was absent when the
// check was armed, and answers what is left with the last read's error.
func settled(armed map[uint64]bool) ([]Connection, error) {
	var left []Connection
	var err error
	testwait.Holds(leakSettle, leakPoll, func() bool {
		left, err = opened(armed)
		return err == nil && len(left) == 0
	})
	return left, err
}

// opened is what the server holds that it did not hold when the check was armed.
func opened(armed map[uint64]bool) ([]Connection, error) {
	open, err := readConnections()
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
