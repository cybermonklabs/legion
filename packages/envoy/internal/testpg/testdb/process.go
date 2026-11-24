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
func ProcessDatabaseOn(t testing.TB, adminURL, prefix string) string {
	t.Helper()
	processDatabases.mu.Lock()
	if !processDatabases.main {
		processDatabases.mu.Unlock()
		t.Fatal("testdb: the package's TestMain must run its tests through testdb.Main or testdb.Run " +
			"(storetest.Main or storetest.Run in the envoy module, ciphertest.Main outside it), " +
			"which drop the database ProcessDatabaseOn makes")
	}
	key := adminURL + "\x00" + prefix
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
	return database.url
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
	d.name, d.url = name, databaseURL(*server, name)
	return nil
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
// The wait for the drops is bounded (teardownDeadline), as internal/dispatch/store/storetest's
// teardown is, because a drop can wait for a reason outside this process: DROP DATABASE waits until
// every backend of the server, in any database, has accepted the barrier it raises (wait event
// IPC/ProcSignalBarrier), so a backend that cannot run holds every drop on the server for as long
// as it cannot. The binary's own -timeout alarm stops when run returns, and cmd/go kills the
// binary a minute past it. No drop has a deadline of its own: a cancel request does end the
// barrier wait, and ends the drop with it, leaving the database on the server (measured on
// PostgreSQL 18.6 and 16). So each drop runs on context.Background(), and at the bound Run reads
// what each drop still in flight waits on while it waits (waitingOn), names the database and that
// read, and returns the tests' exit code, since every test has reported by then and the wait is
// another backend's. The process's exit only closes the drops' sockets, which the server does not
// read while a drop waits, so the server finishes each drop once the backend it waits on accepts
// the barrier (measured on both).
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
	bound := time.NewTimer(time.Until(teardownDeadline(started)))
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
					database.name, database.host, time.Since(started).Round(time.Second), waitingOn(database.adminURL, database.dropStatement()))
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

// teardownGrace and teardownUnbounded set Run's bound, as they set
// internal/dispatch/store/storetest's. cmd/go kills a test binary one minute past its -timeout
// ("ran too long"), and the binary's own alarm stops when its tests return, so nothing else ends a
// teardown that waits: the teardown ends teardownGrace past the -timeout, inside that minute. A
// binary run with -timeout 0 has no kill to stay inside, and gets teardownUnbounded from the end
// of its tests.
const (
	teardownGrace     = 30 * time.Second
	teardownUnbounded = 5 * time.Minute
)

// teardownDeadline is when Run's teardown gives up, for a test binary whose tests began at
// started.
func teardownDeadline(started time.Time) time.Time {
	if timeoutFlag := flag.Lookup("test.timeout"); timeoutFlag != nil {
		if timeout, ok := timeoutFlag.Value.(flag.Getter).Get().(time.Duration); ok && timeout > 0 {
			return started.Add(timeout + teardownGrace)
		}
	}
	return time.Now().Add(teardownUnbounded)
}

// waitingOn says what the server's backend running statement waits on, and lists the server's
// other client backends, by pid, database, state and wait, from a pg_stat_activity read on a
// connection of its own to adminURL, bounded by waitingOnBound. A backend that holds a drop is
// among them, though nothing the server shows says which.
func waitingOn(adminURL, statement string) string {
	ctx, cancel := context.WithTimeout(context.Background(), waitingOnBound)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `select pid, coalesce(datname, ''), coalesce(state, ''), coalesce(wait_event_type, ''),
		coalesce(wait_event, ''), left(coalesce(query, ''), 60), coalesce(query, '') = $1
		from pg_stat_activity where backend_type = 'client backend' and pid <> pg_backend_pid() order by pid`, statement)
	if err != nil {
		return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
	}
	defer rows.Close()
	var drops, others []string
	for rows.Next() {
		var pid int
		var database, state, waitType, wait, query string
		var isDrop bool
		if err := rows.Scan(&pid, &database, &state, &waitType, &wait, &query, &isDrop); err != nil {
			return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
		}
		line := fmt.Sprintf("pid %d in %q, %s, waiting on %s/%s, %q", pid, database, state, waitType, wait, query)
		if isDrop {
			drops = append(drops, line)
		} else {
			others = append(others, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
	}
	return fmt.Sprintf("the drop: %s; the server's other client backends: %s", listOrNone(drops), listOrNone(others))
}

// waitingOnBound bounds waitingOn's read of pg_stat_activity.
const waitingOnBound = 10 * time.Second

func listOrNone(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "; ")
}
