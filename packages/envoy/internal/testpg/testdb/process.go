package testdb

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// processDatabases are the databases ProcessDatabaseOn makes, one per server and prefix in a test
// binary, which Run drops once the binary's tests have ended.
var processDatabases = struct {
	mu   sync.Mutex
	main bool
	by   map[string]*processDatabase
}{by: map[string]*processDatabase{}}

// processDatabase is one database ProcessDatabaseOn made, or the error that stopped it.
type processDatabase struct {
	once     sync.Once
	err      error
	adminURL string
	host     string
	name     string
	url      string
}

// ProcessDatabaseOn answers a database of this test binary's own on the server adminURL names,
// made by the first test that asks for it and dropped by Run once every test of the binary has
// ended, as a URL of that database (databaseURL: adminURL with its path replaced by it, and its
// query's pairs kept byte for byte but for any dbname or database). The name is prefix, an
// underscore and eight random bytes in hex, so no other binary, nor another run of this one,
// takes it.
//
// It is for a harness whose tests each make a schema of their own in one database, which costs a
// test far less than a database of its own (DatabaseOn), and whose tests take a lock Postgres holds
// database-wide, as a migration's advisory lock is. On one database every package of a run, and
// every run on a shared server, waits on that lock for the others; on a database of the binary's
// own, a test waits only for the tests of its own binary, which run one after another unless they
// ask to run in parallel. The server's login needs CREATEDB.
//
// The package's TestMain must run its tests through Run (or Main), which drops the database; it
// fails the test otherwise, as a database no step drops would be left on the server.
//
// The database is the one for adminURL less its connect_timeout (serverKey): WithConnectDeadline
// sets that pair to the seconds the asking test has left, which differ from test to test of the
// binary, while the server does not. The URL answered is the asking test's adminURL naming that
// database, so each test's connects keep the bound it asked with.
func ProcessDatabaseOn(t testing.TB, adminURL, prefix string) string {
	t.Helper()
	processDatabases.mu.Lock()
	if !processDatabases.main {
		processDatabases.mu.Unlock()
		t.Fatal("testdb: the package's TestMain must run its tests through testdb.Main or testdb.Run " +
			"(storetest.Main or storetest.Run in the envoy module, ciphertest.Main outside it), " +
			"which drop the database ProcessDatabaseOn makes")
	}
	key := serverKey(adminURL) + "\x00" + prefix
	database, ok := processDatabases.by[key]
	if !ok {
		database = &processDatabase{adminURL: adminURL}
		processDatabases.by[key] = database
	}
	processDatabases.mu.Unlock()
	database.once.Do(func() { database.err = database.create(prefix) })
	if database.err != nil {
		t.Fatalf("testdb: make the test binary's database: %v", database.err)
	}
	server, err := url.Parse(adminURL)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is printed.
		t.Fatalf("testdb: parse the admin URL of the Postgres server: %v", errors.Unwrap(err))
	}
	return databaseURL(*server, database.name)
}

// create makes the database, as DatabaseOn makes one: a copy of template1's files.
func (d *processDatabase) create(prefix string) error {
	server, err := url.Parse(d.adminURL)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is kept.
		return fmt.Errorf("parse the admin URL of the Postgres server: %w", errors.Unwrap(err))
	}
	name, err := newDatabaseName(prefix)
	if err != nil {
		return err
	}
	d.host = server.Host
	if err := execOn(d.adminURL, d.host, "create database "+name+" strategy file_copy"); err != nil {
		return err
	}
	// Named once it exists, so Run drops it even when the setting below fails.
	d.name, d.url = name, databaseURL(*server, name)
	return execOn(d.adminURL, d.host, WithoutJIT(name))
}

// Main runs the package's tests and then drops the database ProcessDatabaseOn made for them. A
// package whose tests ask for one uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }.
func Main(m *testing.M) int { return Run(m.Run) }

// Run is Main for a package whose TestMain also runs another harness's Main, or runs its tests
// more than once: run runs them, and Run then drops each database ProcessDatabaseOn made, with
// force, since a pool a test left open would otherwise hold it. A drop that fails is printed and
// fails the run, so a database left on a shared server is reported where it was left. A binary
// that ends before run returns, on a test's panic or the -timeout's, drops nothing, and leaves
// its database on the server under its prefix, as it left its tests' schemas before.
//
// The wait for the drops is bounded (TeardownDeadline), as internal/dispatch/store/storetest's
// teardown is, because a drop can wait for a reason outside this process: DROP DATABASE waits until
// every backend of the server, in any database, has accepted the barrier it raises (wait event
// IPC/ProcSignalBarrier), so a backend that cannot run holds every drop on the server for as long
// as it cannot. The binary's own -timeout alarm stops when run returns, and cmd/go kills the
// binary only well past it (TeardownGrace says when). No drop has a deadline of its own: a cancel
// request does end the barrier wait, and ends the drop with it, leaving the database on the server
// (measured on PostgreSQL 18.6 and 16). So each drop runs on context.Background(), and at the bound
// Run reads what each drop still in flight waits on while it waits (WaitingOn), names the database
// and that read, and returns the tests' exit code, since every test has reported by then and the
// wait is another backend's. The process's exit only closes the drops' sockets, which the server
// does not read while a drop waits, so the server finishes each drop once the backend it waits on
// accepts the barrier (measured on both).
func Run(run func() int) int {
	processDatabases.mu.Lock()
	processDatabases.main = true
	processDatabases.mu.Unlock()
	started := time.Now()
	code := run()
	processDatabases.mu.Lock()
	var made []*processDatabase
	for _, database := range processDatabases.by {
		if database.url != "" {
			made = append(made, database)
		}
	}
	processDatabases.mu.Unlock()
	if len(made) == 0 {
		return code
	}
	type drop struct {
		database *processDatabase
		err      error
	}
	done := make(chan drop, len(made))
	pending := make(map[*processDatabase]bool, len(made))
	for _, database := range made {
		pending[database] = true
		go func() {
			done <- drop{database, execOn(database.adminURL, database.host, database.dropStatement())}
		}()
	}
	bound := time.NewTimer(time.Until(TeardownDeadline(started)))
	defer bound.Stop()
	for len(pending) > 0 {
		select {
		case result := <-done:
			delete(pending, result.database)
			if result.err != nil {
				fmt.Fprintf(os.Stderr, "testdb: %v\n", result.err)
				if code == 0 {
					code = 1
				}
			}
		case <-bound.C:
			// The drops still in flight keep their connections; the process ends with them, and
			// the server goes on with each drop.
			for database := range pending {
				fmt.Fprintf(os.Stderr, "testdb: drop database %s on %s: still waiting at the teardown's bound, %s after the tests began, so the drop is left to the server: %s\n",
					database.name, database.host, time.Since(started).Round(time.Second), WaitingOn(database.adminURL, database.dropStatement()))
			}
			return code
		}
	}
	return code
}

// dropStatement is the statement Run drops the database with.
func (d *processDatabase) dropStatement() string {
	return "drop database if exists " + d.name + " with (force)"
}

// TeardownGrace and teardownUnbounded set Run's bound (TeardownDeadline), and
// internal/dispatch/store/storetest's Run's. Nothing else ends a teardown that waits: the
// binary's own alarm stops when its tests return, and cmd/go kills the binary ("ran too long")
// only a minute or a tenth of its -timeout past that -timeout, whichever is longer
// (cmd/go/internal/test/test.go:836-854 at Go 1.27.3): three minutes under CI's -timeout 30m. So
// the teardown ends TeardownGrace past the -timeout, counted from the start of the tests, inside
// that kill, with the time left to print what it waited on. cmd/go counts its kill from just
// before it starts the binary, so the margin under the kill is less by the time the binary takes
// to reach its tests (its package init, and its TestMain before Run). A binary run with -timeout 0
// has no kill to stay inside, and gets teardownUnbounded from the end of its tests.
const (
	TeardownGrace     = 30 * time.Second
	teardownUnbounded = 5 * time.Minute
)

// teardownFloor is the least a teardown waits from its own start, for a harness's Run nested in
// another's: internal/dispatch/api runs the Dispatch storetest's Run inside the broker
// storetest's (this package's Run), and both take their bound from TeardownDeadline with nearly
// the same start. Where the inner Run waited to that bound, the outer one's had passed by the time
// its drop began, and it named its database at once, before that drop's statement could reach the
// server, which then never dropped it: a statement the server has read it finishes after the
// process exits (Run), and one it never read it never runs. With the floor the outer drop has
// teardownFloor to reach the server, and both waits stay inside cmd/go's kill. Counted from the
// start of the tests, the inner bound is TeardownGrace past the -timeout; its pg_stat_activity
// read takes up to waitingOnBound; then the floor; and the outer read for its one database, up to
// waitingOnBound: 55 s past the -timeout. cmd/go counts its kill from just before it starts the
// binary, so under the kill's least, a minute past the -timeout, the margin is 5 s less the time
// the binary takes to reach its tests (its package init, and its TestMain before Run); under the
// -timeouts CI gives these packages, 20 minutes and more, the kill is two minutes or more past the
// -timeout, and the margin more than a minute. A Run that is no other's inner or outer starts its
// teardown before its -timeout, so the floor never moves its bound.
const teardownFloor = 5 * time.Second

// TeardownDeadline is when a test binary's teardown gives up, for a binary whose tests began at
// started: Run's, and internal/dispatch/store/storetest's Run's. It is TeardownGrace past the
// -timeout, counted from started, and at least teardownFloor from now; teardownUnbounded from now
// under -timeout 0.
func TeardownDeadline(started time.Time) time.Time {
	floor := time.Now().Add(teardownFloor)
	if timeoutFlag := flag.Lookup("test.timeout"); timeoutFlag != nil {
		if timeout, ok := timeoutFlag.Value.(flag.Getter).Get().(time.Duration); ok && timeout > 0 {
			if bound := started.Add(timeout + TeardownGrace); bound.After(floor) {
				return bound
			}
			return floor
		}
	}
	return time.Now().Add(teardownUnbounded)
}

// WaitingOn says what the server's backends running any of statements wait on, and lists the
// server's other client backends, by pid, database, state and wait, from a pg_stat_activity read on
// a connection of its own to adminURL, bounded by waitingOnBound: the read Run makes at its bound,
// and internal/dispatch/store/storetest's Run at its own. A backend that holds a drop is among
// them, though nothing the server shows says which. Where the read fails it says at which step, so
// an empty list says why: a login the server never answered, or a query it took the login for and
// never answered. The read takes a new connection rather than one held from before the drops: a
// connection held from before them would be held from the first test on, beside the admin pool,
// on a server whose connections the tests are sized to.
func WaitingOn(adminURL string, statements ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), waitingOnBound)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		if pgconn.Timeout(err) {
			return fmt.Sprintf("pg_stat_activity could not be read: a new connection to the server got no answer to its login within %s, so the server answers no new login and no backend can be listed (%v)", waitingOnBound, err)
		}
		return fmt.Sprintf("pg_stat_activity could not be read: a new connection to the server failed (%v)", err)
	}
	defer conn.Close(context.Background())
	unread := func(err error) string {
		if pgconn.Timeout(err) {
			return fmt.Sprintf("pg_stat_activity could not be read: the server answered this read's login but not its query within %s, so no backend can be listed (%v)", waitingOnBound, err)
		}
		return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
	}
	rows, err := conn.Query(ctx, `select pid, coalesce(datname, ''), coalesce(state, ''), coalesce(wait_event_type, ''),
		coalesce(wait_event, ''), left(coalesce(query, ''), 60), coalesce(query, '') = any($1)
		from pg_stat_activity where backend_type = 'client backend' and pid <> pg_backend_pid() order by pid`, statements)
	if err != nil {
		return unread(err)
	}
	defer rows.Close()
	var drops, others []string
	for rows.Next() {
		var pid int
		var database, state, waitType, wait, query string
		var isDrop bool
		if err := rows.Scan(&pid, &database, &state, &waitType, &wait, &query, &isDrop); err != nil {
			return unread(err)
		}
		line := fmt.Sprintf("pid %d in %q, %s, waiting on %s/%s, %q", pid, database, state, waitType, wait, query)
		if isDrop {
			drops = append(drops, line)
		} else {
			others = append(others, line)
		}
	}
	if err := rows.Err(); err != nil {
		return unread(err)
	}
	return fmt.Sprintf("the drops: %s; the server's other client backends: %s", listOrNone(drops), listOrNone(others))
}

// waitingOnBound bounds WaitingOn's read of pg_stat_activity.
const waitingOnBound = 10 * time.Second

func listOrNone(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "; ")
}
