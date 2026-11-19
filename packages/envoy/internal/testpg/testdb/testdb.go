// Package testdb is the half of internal/testpg that needs no container runtime, and is, with
// internal/testpg and the two storetest packages it serves, the only code of Envoy's tests that
// reads a variable naming a Postgres server (Server). A test never gets the database that
// variable's URL names, so two test binaries sharing one server see none of each other's rows.
//
// Server.Database is an empty database on the server a variable names, which the caller migrates
// itself, skipping the test when the variable is unset. DatabaseOn is the helper for a test that
// already holds the URL of the server it runs on: it reads no variable and starts nothing.
// WithParameter sets a query parameter on any of these URLs. Template is a migrated database a
// binary makes once and copies for each test; Dispatch's and the broker's storetest packages give
// out its clones as migrated stores, and a test of either store that wants one calls its storetest
// instead. Every URL a Server hands out carries the test binary's deadline as its connect bound
// (WithConnectDeadline), and every drop of a database here waits out the backends still attached
// to it until that same deadline (dropDatabase).
//
// It imports no testcontainers package, so a test helper every Postgres-backed test links (the
// broker's storetest among them) does not pay testcontainers' package init, which walks every
// process on the machine before any test runs. internal/testpg keeps the container path and calls
// this.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testwait"
)

// DatabaseOn creates a database of this test's own on the server adminURL names, drops it when the
// test ends, and returns adminURL with its path replaced by that database and its query kept. The
// name is prefix, an underscore and eight random bytes in hex: it is random because every test of
// every package sharing one server takes a name there.
//
// Both statements run on adminURL itself, which is exact for every caller because a caller passes
// the URL its own statements run on, and that URL and the result differ only in the path.
//
// A failure names the server's host and the database, never adminURL: a DSN carries a credential,
// and a test here prints none (docs/solutions/testing/a-proof-script-never-prints-a-dsn-a-userinfo-scrubber-leaks-the-query-and-keyword-password-forms-INFRA-229.md).
// This function reads no environment variable and starts no container, so its failures can name
// neither.
func DatabaseOn(t testing.TB, adminURL, prefix string) string {
	t.Helper()
	server, err := url.Parse(adminURL)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is printed.
		t.Fatalf("parse the admin URL of the Postgres server: %v", errors.Unwrap(err))
	}
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("%v", err)
	}
	name := prefix + "_" + suffix
	// A copy of template1's files: created the default way, through the write-ahead log, an empty
	// database costs 4.4 MB of log (docs/testing.md, "A database a Go test creates").
	adminExec(t, adminURL, server.Host, "create database "+name+" strategy file_copy")
	// dropDatabase ends every backend attached to the database, which the drop needs because a
	// database a connection still holds cannot be dropped. That covers a connection the test left
	// open, so one leaked pool cannot keep a database on a shared server for the rest of the run.
	// The drop runs in t's cleanup, so t's deadline, the binary's, is its one bound. It drops the
	// database only if it still exists (dropDatabase): a cleanup the test registered later may
	// have dropped it already.
	t.Cleanup(func() {
		conn := adminConnect(t, adminURL, server.Host)
		defer conn.Close(context.Background())
		wait := func(what string, holds func() bool) error {
			testwait.Eventually(t, what, holds)
			return nil
		}
		if err := dropDatabase(conn, name, wait); err != nil {
			t.Fatalf("drop database %s: %v", name, err)
		}
	})
	return databaseURL(*server, name)
}

// databaseURL is server's URL with its path replaced by database, its query kept.
func databaseURL(server url.URL, database string) string {
	server.Path = "/" + database
	return server.String()
}

// adminExec runs one statement over a connection of its own to adminURL (adminConnect).
func adminExec(t testing.TB, adminURL, host, statement string) {
	t.Helper()
	conn := adminConnect(t, adminURL, host)
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// adminConnect opens a connection of its own to adminURL: neither create database nor drop
// database may run inside a transaction or against the database it names. host is that server's
// host, printed in place of adminURL, which carries a credential.
func adminConnect(t testing.TB, adminURL, host string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), adminURL)
	if err != nil {
		t.Fatalf("connect to the Postgres server at %s: %v", host, err)
	}
	return conn
}

// WithParameter returns databaseURL with its query parameter key set to value. A test that has a
// connection ask for a setting adds it here rather than appending "&key=value", which names a
// parameter only when the URL already carries a query: DatabaseOn keeps the server URL's query, so
// on a URL that has none the suffix runs on into the database's name. A failure prints only what
// url.Parse wraps, since the URL carries a credential (DatabaseOn).
func WithParameter(t testing.TB, databaseURL, key, value string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse the URL of the test database: %v", errors.Unwrap(err))
	}
	setParameter(parsed, key, value)
	return parsed.String()
}

// setParameter sets u's query parameter key to value.
func setParameter(u *url.URL, key, value string) {
	query := u.Query()
	query.Set(key, value)
	u.RawQuery = query.Encode()
}

// WithConnectDeadline returns databaseURL with connect_timeout set to the whole seconds left until
// t's deadline, the test binary's own (go test's -timeout), rounded up and at least 1, in place of
// any connect_timeout the URL names. Server sets it on every URL it reads, so every database,
// clone and pool a fixture here hands out carries it; a URL that comes from somewhere else, such
// as a container testpg starts, takes it here.
//
// pgx applies connect_timeout to the whole of a new connection (the dial, the authentication and
// the server's startup answer), through pgx.Connect and through every pool, and a pool keeps it in
// place of the 2 minutes pgxpool gives a connection whose configuration names none. On a loaded
// machine Postgres can take longer than 2 minutes to answer a new connection, and a test then
// failed on its connect, whatever it checked (dispatch://INFRA-4308). With the binary's deadline
// as the bound, a slow server only slows a test, and one that never answers ends the test at that
// deadline. A shorter connect_timeout in the URL would make the test depend on load again, which
// is why it is replaced. Under -timeout 0, and for a t that names no deadline (testing.TB has no
// Deadline method, so t is asked for one by interface), it is the longest connect_timeout pgx
// holds, so the connect has no bound of the test's own there either. A failure prints only what
// url.Parse wraps, since the URL carries a credential (DatabaseOn).
func WithConnectDeadline(t testing.TB, databaseURL string) string {
	t.Helper()
	return WithParameter(t, databaseURL, "connect_timeout", connectTimeout(t))
}

// connectTimeout is the connect_timeout WithConnectDeadline sets for t, in whole seconds.
func connectTimeout(t testing.TB) string {
	unbounded := strconv.FormatInt(math.MaxInt64/int64(time.Second), 10)
	named, ok := t.(interface{ Deadline() (time.Time, bool) })
	if !ok {
		return unbounded
	}
	deadline, bounded := named.Deadline()
	if !bounded {
		return unbounded
	}
	left := time.Until(deadline)
	return strconv.FormatInt(max(1, int64((left+time.Second-1)/time.Second)), 10)
}

// randomSuffix is eight random bytes in hex, the part of a database's name that keeps it apart
// from every other test's on a shared server.
func randomSuffix() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return hex.EncodeToString(suffix[:]), nil
}
