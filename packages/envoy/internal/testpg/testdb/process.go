package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// processDatabases are the databases ProcessDatabaseOn makes, one per server and prefix in a test
// binary, which Run drops once the binary's tests have ended.
var processDatabases struct {
	mu   sync.Mutex
	main bool
	keys []string
	by   map[string]*processDatabase
}

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
// ended: adminURL with its path replaced by that database and its query kept. The name is prefix,
// an underscore and eight random bytes in hex, so no other binary, nor another run of this one,
// takes it.
//
// It is for a harness whose tests each make a schema of their own in one database, which costs a
// test far less than a database of its own (DatabaseOn), and whose tests take a lock Postgres holds
// database-wide, as a migration's advisory lock is. On one database every package of a run, and
// every run on a shared server, waits on that lock for the others; on a database of the binary's
// own, a test waits only for the tests of its own binary, which run one after another unless they
// ask to run in parallel.
//
// The package's TestMain must run its tests through Run (or Main), which drops the database; it
// fails the test otherwise, as a database no step drops would be left on the server.
func ProcessDatabaseOn(t testing.TB, adminURL, prefix string) string {
	t.Helper()
	processDatabases.mu.Lock()
	if !processDatabases.main {
		processDatabases.mu.Unlock()
		t.Fatal("testdb: the package's TestMain must return testdb.Main(m) or testdb.Run, which drops the database ProcessDatabaseOn makes")
	}
	if processDatabases.by == nil {
		processDatabases.by = map[string]*processDatabase{}
	}
	key := adminURL + "\x00" + prefix
	database, ok := processDatabases.by[key]
	if !ok {
		database = &processDatabase{adminURL: adminURL}
		processDatabases.by[key] = database
		processDatabases.keys = append(processDatabases.keys, key)
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
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("random database name: %w", err)
	}
	d.host, d.name = server.Host, prefix+"_"+hex.EncodeToString(suffix[:])
	if err := d.exec("create database " + d.name + " strategy file_copy"); err != nil {
		return err
	}
	d.url = databaseURL(*server, d.name)
	return nil
}

// exec runs statement over a connection of its own to the admin URL, naming the server's host and
// never the URL, which carries a credential.
func (d *processDatabase) exec(statement string) error {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, d.adminURL)
	if err != nil {
		return fmt.Errorf("connect to the Postgres server at %s: %w", d.host, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		return fmt.Errorf("%s on %s: %w", statement, d.host, err)
	}
	return nil
}

// Main runs the package's tests and then drops the database ProcessDatabaseOn made for them. A
// package whose tests ask for one uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }.
func Main(m *testing.M) int { return Run(m.Run) }

// Run is Main for a package whose TestMain also runs another harness's Main, or runs its tests
// more than once: run runs them, and Run then drops each database ProcessDatabaseOn made, with
// force, since a pool a test left open would otherwise hold it. A drop that fails is printed and
// fails the run, so a database left on a shared server is reported where it was left.
func Run(run func() int) int {
	processDatabases.mu.Lock()
	processDatabases.main = true
	processDatabases.mu.Unlock()
	code := run()
	processDatabases.mu.Lock()
	defer processDatabases.mu.Unlock()
	for _, key := range processDatabases.keys {
		database := processDatabases.by[key]
		if database.name == "" || database.url == "" {
			continue
		}
		if err := database.exec("drop database if exists " + database.name + " with (force)"); err != nil {
			fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}
