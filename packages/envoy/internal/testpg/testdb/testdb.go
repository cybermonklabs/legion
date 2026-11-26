// Package testdb is the half of internal/testpg that needs no container runtime, and is, with
// internal/testpg and the two storetest packages it serves, the only code of Envoy's tests that
// reads a variable naming a Postgres server (Server). A test never gets the database that
// variable's URL names, so two test binaries sharing one server see none of each other's rows.
//
// Server.Database is an empty database on the server a variable names, which the caller migrates
// itself, skipping the test when the variable is unset. DatabaseOn is the helper for a test that
// already holds the URL of the server it runs on: it reads no variable and starts nothing.
// ProcessDatabaseOn is one database of the test binary's own on such a server, which Main (or
// Run) drops after the binary's tests. WithParameter sets a query parameter on any of these URLs,
// and Now and ClockTimestamp read the clock of a database a test holds. Template is a migrated
// database a binary makes once and copies for each test; Dispatch's and the broker's storetest
// packages give out its clones as migrated stores, and a test of either store that wants one calls
// its storetest instead. Every URL a Server hands out carries the test binary's deadline as its
// connect bound (WithConnectDeadline), and every drop of a database here but ProcessDatabaseOn's
// waits out the backends still attached to it until that same deadline (dropDatabase).
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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// DatabaseOn creates a database of this test's own on the server adminURL names, drops it when the
// test ends, and returns a URL of that database (databaseURL: adminURL with its path replaced by
// it, and its query's pairs kept byte for byte but for any dbname or database). The name is
// prefix, an underscore and eight random bytes in hex: it is random because every test of every
// package sharing one server takes a name there.
//
// Both statements run on adminURL itself, which is exact for every caller because a caller passes
// the URL its own statements run on, and that URL and the result differ only in the database they
// select.
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
	name, err := newDatabaseName(prefix)
	if err != nil {
		t.Fatal(err)
	}
	// A copy of template1's files: created the default way, through the write-ahead log, an empty
	// database costs 4.4 MB of log (docs/testing.md, "A database a Go test creates").
	adminExec(t, adminURL, server.Host, "create database "+name+" strategy file_copy")
	// DropDatabaseOn ends every backend attached to the database (dropDatabase), which the drop
	// needs because a database a connection still holds cannot be dropped. That covers a connection
	// the test left open, so one leaked pool cannot keep a database on a shared server for the rest
	// of the run. The drop runs in t's cleanup through t's shared wait (WaitIn), so t's deadline,
	// the binary's, is its one bound. It drops the database only if it still exists (dropDatabase):
	// a cleanup the test registered later may have dropped it already.
	t.Cleanup(func() {
		if err := DropDatabaseOn(adminURL, name, WaitIn(t)); err != nil {
			t.Fatalf("drop database %s: %v", name, err)
		}
	})
	return databaseURL(*server, name)
}

// newDatabaseName is prefix, an underscore and eight random bytes in hex (randomSuffix).
func newDatabaseName(prefix string) (string, error) {
	suffix, err := randomSuffix()
	if err != nil {
		return "", err
	}
	return prefix + "_" + suffix, nil
}

// databaseURL is server's URL naming database in every field pgx selects a database by: its path
// is replaced by database, and its query loses each dbname or database pair, which pgx reads over
// the path, so a server URL that carried one would put every caller in the one database it names.
// Every other pair is kept byte for byte, as are the pairs' order and escapes: re-encoding the
// query (url.Values.Encode) would write a space as a plus, which pgx reads as a plus, so an
// options pair such as -c%20statement_timeout%3D5000 would stop connecting. An empty pair is
// dropped.
func databaseURL(server url.URL, database string) string {
	server.Path, server.RawPath = "/"+database, ""
	var kept []string
	for _, pair := range strings.Split(server.RawQuery, "&") {
		if pair == "" {
			continue
		}
		key, _, _ := strings.Cut(pair, "=")
		if name, err := url.PathUnescape(key); err == nil && (name == "dbname" || name == "database") {
			continue
		}
		kept = append(kept, pair)
	}
	server.RawQuery = strings.Join(kept, "&")
	return server.String()
}

// adminExec runs one statement over a connection of its own to adminURL, failing t: neither
// create database nor drop database may run inside a transaction or against the database it
// names.
func adminExec(t testing.TB, adminURL, host, statement string) {
	t.Helper()
	if err := execOn(adminURL, host, statement); err != nil {
		t.Fatal(err)
	}
}

// execOn runs statement over a connection of its own to adminURL. host is that server's host,
// printed in place of adminURL, which carries a credential.
func execOn(adminURL, host, statement string) error {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect to the Postgres server at %s: %w", host, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		return fmt.Errorf("%s on %s: %w", statement, host, err)
	}
	return nil
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
	return withConnectDeadlineAt(t, databaseURL, time.Now())
}

// withConnectDeadlineAt is WithConnectDeadline with the time left counted from now, so a test of it
// names the moment rather than reading the clock.
func withConnectDeadlineAt(t testing.TB, databaseURL string, now time.Time) string {
	t.Helper()
	return WithParameter(t, databaseURL, "connect_timeout", connectTimeout(t, now))
}

// connectTimeout is the connect_timeout WithConnectDeadline sets for t at now, in whole seconds.
func connectTimeout(t testing.TB, now time.Time) string {
	unbounded := strconv.FormatInt(math.MaxInt64/int64(time.Second), 10)
	named, ok := t.(interface{ Deadline() (time.Time, bool) })
	if !ok {
		return unbounded
	}
	deadline, bounded := named.Deadline()
	if !bounded {
		return unbounded
	}
	left := deadline.Sub(now)
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

// Querier is what Now and ClockTimestamp need of a pool, a connection or a transaction: one row.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Now is the database's clock now as now() reads it: the moment the transaction db runs the
// statement in began, the clock that stamps what a statement, a default or a trigger writes with
// now(). On a pool that is the moment of the one statement it runs. A moment a test judges such a
// stamp against is read here, not off the test's own clock.
func Now(t testing.TB, db Querier) time.Time {
	t.Helper()
	return readClock(t, db, `select now()`)
}

// ClockTimestamp is the database's clock at the moment the statement reads it (clock_timestamp()),
// not the start of a transaction: two readings of it around a pass bracket the moment the pass
// stamped with its own now(), at the precision the database writes stamps with, which a reading
// of the test process's own clock does not.
func ClockTimestamp(t testing.TB, db Querier) time.Time {
	t.Helper()
	return readClock(t, db, `select clock_timestamp()`)
}

func readClock(t testing.TB, db Querier, query string) time.Time {
	t.Helper()
	var now time.Time
	if err := db.QueryRow(context.Background(), query).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return now
}
