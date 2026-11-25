// Package testdb is the half of internal/testpg that needs no container runtime: DatabaseOn makes
// an empty Postgres database of a test's own on a server the test already holds, and
// ProcessDatabaseOn one of the test binary's own, which Main drops after the binary's tests. It
// imports no testcontainers package, so a test helper every Postgres-backed test links (the
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
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testwait"
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
// Every connect to adminURL here, and to the URL it returns, waits until the test binary's deadline
// less testwait.Margin (WithConnectDeadline), whatever connect_timeout adminURL names.
func DatabaseOn(t testing.TB, adminURL, prefix string) string {
	t.Helper()
	adminURL = WithConnectDeadline(t, adminURL)
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
	return databaseURL(*server, name)
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
	server.RawQuery = strings.Join(otherPairs(server.RawQuery, "dbname", "database"), "&")
	return server.String()
}

// otherPairs is the pairs of rawQuery but those whose key, unescaped, is one of names: every other
// pair kept byte for byte and in its order, an empty pair dropped (databaseURL says why).
func otherPairs(rawQuery string, names ...string) []string {
	var kept []string
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		key, _, _ := strings.Cut(pair, "=")
		if name, err := url.PathUnescape(key); err == nil && slices.Contains(names, name) {
			continue
		}
		kept = append(kept, pair)
	}
	return kept
}

// WithConnectDeadline returns databaseURL with connect_timeout set to the whole seconds left until
// t's deadline, the test binary's own (go test's -timeout), less testwait.Margin, rounded down and
// at least 1, in place of any connect_timeout the URL names. pgx bounds the whole of a new
// connection (the dial, the authentication and the server's startup answer) by connect_timeout,
// through pgx.Connect and every pool. A URL that names none has no bound through pgx.Connect,
// which bounds a connect only by the connect_timeout it is given (pgconn's connectPreferred), and
// two minutes in a pgxpool pool, which sets them where its config has none (pgxpool's
// constructor; both at pgx v5.13.0). On a loaded machine Postgres can take longer than two
// minutes to answer a new connection, and a test then failed on its connect, whatever it checked
// (rows-md:101: TestAMachineExchangeOfAnEndedCredentialIsRefused failed after 182.44 s). With the
// binary's deadline less the margin as the bound, a slow server only slows a test, and one that
// never answers fails it on pgx's own error with the margin left for the failure and the test's
// cleanups, as a testwait wait gives up, before go test's alarm would end the binary with neither.
// A shorter connect_timeout in the URL would make the test depend on load again, so it is
// replaced. Where t names no deadline (ConnectEnd says which callers) it is
// NoDeadlineConnectTimeout, two minutes. Every other pair of the query is kept byte for byte
// (otherPairs), as databaseURL keeps them, so an options pair still connects. A failure prints
// only what url.Parse wraps, since the URL carries a credential.
func WithConnectDeadline(t testing.TB, databaseURL string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse the URL of the test database: %v", errors.Unwrap(err))
	}
	parsed.RawQuery = strings.Join(append(otherPairs(parsed.RawQuery, "connect_timeout"), "connect_timeout="+connectTimeout(t)), "&")
	return parsed.String()
}

// serverKey is adminURL without its connect_timeout pairs, the server and query a test names apart
// from the bound WithConnectDeadline gives that test's connects, which differs from test to test
// of one binary. A URL url.Parse refuses is its own key.
func serverKey(adminURL string) string {
	parsed, err := url.Parse(adminURL)
	if err != nil {
		return adminURL
	}
	parsed.RawQuery = strings.Join(otherPairs(parsed.RawQuery, "connect_timeout"), "&")
	return parsed.String()
}

// connectTimeout is the connect_timeout WithConnectDeadline sets for t, in whole seconds
// (TimeoutUntil of ConnectEnd), and NoDeadlineConnectTimeout where t names no deadline.
func connectTimeout(t testing.TB) string {
	end, bounded := ConnectEnd(t)
	if !bounded {
		return strconv.FormatInt(int64(NoDeadlineConnectTimeout/time.Second), 10)
	}
	return strconv.FormatInt(int64(TimeoutUntil(end)/time.Second), 10)
}

// NoDeadlineConnectTimeout is the connect bound of a connect that no test deadline bounds: two
// minutes, pgxpool's own default (pgxpool/pool.go:291-292 at pgx v5.13.0), which a plain
// pgx.Connect would otherwise not have. A caller with no deadline (ConnectEnd) gets it; the
// longest connect_timeout pgx holds, 9,223,372,036 s, would leave it no bound at all.
const NoDeadlineConnectTimeout = 2 * time.Minute

// ConnectEnd is the moment every connect a test of t's binary makes must end by: the binary's
// deadline (go test's -timeout, the one t.Deadline names for every test of the binary) less
// testwait.Margin. It is false where t names no deadline: a *testing.T under -timeout 0, and a
// testing.TB with no Deadline method, as a benchmark's *testing.B and a fuzz target's *testing.F
// are at Go 1.27.3. A TB that embeds a *testing.T has its Deadline, as storetest's stopsOnFatal
// (pool_hold_test.go) does, and is read as that test is. Nothing inside go test ends a benchmark
// (testing's alarm stops before benchmarks run, and cmd/go sets no kill for -bench), so
// NoDeadlineConnectTimeout is the only bound a benchmark's connect has. Inside a
// synctest bubble it panics, as T.Deadline does there (testing.go:2323-2327 at Go 1.27.3); no
// test that opens a test store runs in one.
func ConnectEnd(t testing.TB) (time.Time, bool) {
	named, ok := t.(interface{ Deadline() (time.Time, bool) })
	if !ok {
		return time.Time{}, false
	}
	deadline, bounded := named.Deadline()
	if !bounded {
		return time.Time{}, false
	}
	return deadline.Add(-testwait.Margin), true
}

// TimeoutUntil is the connect timeout of a connect that begins now and must end by end: the whole
// seconds left, rounded down, and a second where less than one is left (pgx reads 0 as no bound
// at all). A pool that outlives the test that opened it sets it on each new connection as it
// makes it (pgxpool.Config.BeforeConnect), so a connection made late gets what is left then, not
// what was left when the pool was opened.
func TimeoutUntil(end time.Time) time.Duration {
	return time.Duration(max(1, int64(time.Until(end)/time.Second))) * time.Second
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
