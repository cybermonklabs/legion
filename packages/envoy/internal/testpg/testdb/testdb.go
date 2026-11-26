// Package testdb is the half of internal/testpg that needs no container runtime: DatabaseOn makes
// an empty Postgres database of a test's own on a server the test already holds, and
// ProcessDatabaseOn one of the test binary's own, which Main drops after the binary's tests. It
// imports no testcontainers package, so a test helper every Postgres-backed test links (the
// broker's storetest among them) does not pay testcontainers' package init, which walks every
// process on the machine before any test runs. internal/testpg keeps the container path and calls
// this. Every database made here runs with JIT off, as production's databases do (WithoutJIT).
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

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
	// `with (force)` ends every backend attached to the database, which the drop needs because a
	// database a connection still holds cannot be dropped. That covers a connection the test left
	// open, so one leaked pool cannot keep a database on a shared server for the rest of the run.
	// `if exists`, because a cleanup the test registered later runs first and may have dropped
	// the database already: the cleanup of a role the test created there drops the database before
	// the role, which nothing in the database then holds (internal/dispatch/store's
	// dropRoleAfterTestDatabase).
	t.Cleanup(func() { adminExec(t, adminURL, server.Host, "drop database if exists "+name+" with (force)") })
	adminExec(t, adminURL, server.Host, WithoutJIT(name))
	return databaseURL(*server, name)
}

// WithoutJIT is the statement that turns JIT off for every session that connects to the database
// name from then on. Dispatch runs on a Cloud SQL for PostgreSQL instance (packages/envoy/infra's
// gcp:Dispatch), and Cloud SQL lists LLVM JIT compilation among the features it does not support
// (https://cloud.google.com/sql/docs/postgres/features, "Unsupported features"); CI's Postgres
// image runs with jit = on. Under jit = on a statement whose estimate passes jit_above_cost
// compiles before it runs, which production never pays: main's closure statement, the oracle of
// internal/dispatch/refs's comparisons, estimates past a million on a fixture of 200 issues and
// spent 445-475 ms of each 453-564 ms run compiling 141 functions, 200 times, most of that
// package's time. The setting is the database's, so it reaches every pool and process a test opens
// there, and it goes with the database: no setting of the server changes. A clone does not copy
// its template's settings, so every database takes it when it is made, a template included, so a
// migration runs as production's does too (docs/testing.md, "A database a Go test creates").
func WithoutJIT(name string) string {
	return "alter database " + name + " set jit = off"
}

// newDatabaseName is prefix, an underscore and eight random bytes in hex.
func newDatabaseName(prefix string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(suffix[:]), nil
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
