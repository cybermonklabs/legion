// Package testdb is the half of internal/testpg that needs no container runtime: DatabaseOn makes
// an empty Postgres database of a test's own on a server the test already holds. It imports no
// testcontainers package, so a test helper every Postgres-backed test links (the broker's
// storetest among them) does not pay testcontainers' package init, which walks every process on
// the machine before any test runs. internal/testpg keeps the container path and calls this.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
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
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	name := prefix + "_" + hex.EncodeToString(suffix[:])
	// A copy of template1's files: created the default way, through the write-ahead log, an empty
	// database costs 4.4 MB of log (docs/testing.md, "A database a Go test creates").
	adminExec(t, adminURL, server.Host, "create database "+name+" strategy file_copy")
	// `with (force)` ends every backend attached to the database, which the drop needs because a
	// database a connection still holds cannot be dropped. That covers a connection the test left
	// open, so one leaked pool cannot keep a database on a shared server for the rest of the run.
	// `if exists`, because a cleanup the test registered later runs first and may have dropped
	// the database already: the cleanup of a role the test created there drops the database before
	// the role, which nothing in the database then holds (internal/dispatch/store's
	// dropRoleAfterTestDatabase).
	t.Cleanup(func() { adminExec(t, adminURL, server.Host, "drop database if exists "+name+" with (force)") })
	return databaseURL(*server, name)
}

// databaseURL is server's URL with its path replaced by database, its query kept.
func databaseURL(server url.URL, database string) string {
	server.Path = "/" + database
	return server.String()
}

// adminExec runs one statement over a connection of its own to adminURL: neither create database
// nor drop database may run inside a transaction or against the database it names. host is that
// server's host, printed in place of adminURL, which carries a credential.
func adminExec(t testing.TB, adminURL, host, statement string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to the Postgres server at %s: %v", host, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}
