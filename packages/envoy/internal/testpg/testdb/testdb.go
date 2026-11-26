// Package testdb is the half of internal/testpg that needs no container runtime: DatabaseOn makes
// a Postgres database of a test's own on a server the test already holds, empty or a copy of a
// template, and DatabaseURL names a database on such a server. It imports no testcontainers
// package, so a test helper every Postgres-backed test links (the broker's storetest among them)
// does not pay testcontainers' package init, which walks every process on the machine before any
// test runs. internal/testpg keeps the container path and calls this.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// DatabaseOn creates a database of this test's own on the server adminURL names, drops it when the
// test ends, and returns a URL of that database with each of set added (DatabaseURL: adminURL with
// its path replaced by it, and its query's pairs kept byte for byte but for any dbname or database
// and those set replaces). The database is a copy of template's files, or of template1's where
// template is "", Postgres's own default. The name is prefix, an underscore and eight random bytes
// in hex (NewDatabaseName): it is random because every test of every package sharing one server
// takes a name there.
//
// Both statements run on adminURL itself, which is exact for every caller because a caller passes
// the URL its own statements run on, and that URL and the result differ only in the database they
// select and the pairs set adds. Each opens a connection of its own, so no pool outlives the test.
//
// A failure names the server's host and the database, never adminURL: a DSN carries a credential,
// and a test here prints none (docs/solutions/testing/a-proof-script-never-prints-a-dsn-a-userinfo-scrubber-leaks-the-query-and-keyword-password-forms-INFRA-229.md).
// This function reads no environment variable and starts no container, so its failures can name
// neither.
func DatabaseOn(t testing.TB, adminURL, template, prefix string, set ...string) string {
	t.Helper()
	server, err := url.Parse(adminURL)
	if err != nil {
		// url.Parse's error quotes the URL, and even the error it wraps can: an invalid port's quotes
		// what follows the authority's last colon, which for a password holding an unescaped '#',
		// '/' or '?' is the user and the password's first part. So no part of it is printed.
		t.Fatal("the admin URL of the Postgres server does not parse")
	}
	name, err := NewDatabaseName(prefix)
	if err != nil {
		t.Fatal(err)
	}
	from := ""
	if template != "" {
		from = " template " + pgx.Identifier{template}.Sanitize()
	}
	// A copy of the source's files: created the default way, through the write-ahead log, an empty
	// database costs 4.4 MB of log (docs/testing.md, "A database a Go test creates").
	adminExec(t, adminURL, server.Host, "create database "+name+from+" strategy file_copy")
	// `with (force)` ends every backend attached to the database, which the drop needs because a
	// database a connection still holds cannot be dropped. That covers a connection the test left
	// open, so one leaked pool cannot keep a database on a shared server for the rest of the run.
	// `if exists`, because a cleanup the test registered later runs first and may have dropped
	// the database already: the cleanup of a role the test created there drops the database before
	// the role, which nothing in the database then holds (internal/dispatch/store's
	// dropRoleAfterTestDatabase).
	t.Cleanup(func() { adminExec(t, adminURL, server.Host, "drop database if exists "+name+" with (force)") })
	return DatabaseURL(*server, name, set...)
}

// NewDatabaseName is prefix, an underscore and eight random bytes in hex: the name of a database a
// test, or a harness, makes of its own on a server others share.
func NewDatabaseName(prefix string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(suffix[:]), nil
}

// DatabaseURL is server's URL naming database in every field pgx selects a database by, with each
// of set, a raw key=value query pair, added: its path is replaced by database, and its query loses
// each dbname or database pair, which pgx reads over the path, so a server URL that carried one
// would put every caller in the one database it names, and each pair of a key set names, which
// set's pair replaces (a test's search_path or pool size). Every other pair is kept byte for byte,
// as are the pairs' order and escapes: re-encoding the query (url.Values.Encode) would write a
// space as a plus, which pgx reads as a plus, so an options pair such as
// -c%20statement_timeout%3D5000 would stop connecting. An empty pair is dropped, and set's pairs
// follow the kept ones, in set's order.
func DatabaseURL(server url.URL, database string, set ...string) string {
	replaced := map[string]bool{"dbname": true, "database": true}
	for _, pair := range set {
		key, _, _ := strings.Cut(pair, "=")
		if name, err := url.PathUnescape(key); err == nil {
			replaced[name] = true
		}
	}
	server.Path, server.RawPath = "/"+database, ""
	var kept []string
	for _, pair := range strings.Split(server.RawQuery, "&") {
		if pair == "" {
			continue
		}
		key, _, _ := strings.Cut(pair, "=")
		if name, err := url.PathUnescape(key); err == nil && replaced[name] {
			continue
		}
		kept = append(kept, pair)
	}
	server.RawQuery = strings.Join(append(kept, set...), "&")
	return server.String()
}

// adminExec runs one statement over a connection of its own to adminURL, failing t: neither
// create database nor drop database may run inside a transaction or against the database it
// names. host is that server's host, printed in place of adminURL, which carries a credential.
func adminExec(t testing.TB, adminURL, host, statement string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to the Postgres server at %s: %v", host, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("%s on %s: %v", statement, host, err)
	}
}
