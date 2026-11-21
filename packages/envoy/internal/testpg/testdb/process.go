package testdb

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
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
func Run(run func() int) int {
	processDatabases.mu.Lock()
	processDatabases.main = true
	processDatabases.mu.Unlock()
	code := run()
	processDatabases.mu.Lock()
	defer processDatabases.mu.Unlock()
	for _, database := range processDatabases.by {
		if database.url == "" {
			continue
		}
		if err := execOn(database.adminURL, database.host, "drop database if exists "+database.name+" with (force)"); err != nil {
			fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}
