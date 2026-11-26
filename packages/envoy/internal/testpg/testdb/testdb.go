// Package testdb is the half of internal/testpg that needs no container runtime: DatabaseOn makes
// a Postgres database of a test's own on a server the test already holds, empty or a copy of a
// template, and DatabaseURL names a database on such a server. It imports no testcontainers
// package, so a test helper every Postgres-backed test links (the broker's storetest among them)
// does not pay testcontainers' package init, which walks every process on the machine before any
// test runs. internal/testpg keeps the container path and calls this. Every database made here
// runs with JIT off, as production's databases do (WithoutJIT).
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
// Every connect to adminURL here, and to the URL it returns, waits until the test binary's deadline
// less testwait.Margin (WithConnectDeadline), whatever connect_timeout adminURL names.
func DatabaseOn(t testing.TB, adminURL, template, prefix string, set ...string) string {
	t.Helper()
	adminURL = WithConnectDeadline(t, adminURL)
	server, err := url.Parse(adminURL)
	if err != nil {
		// url.Parse's error quotes the URL, and even the error it wraps can: an invalid port's quotes
		// what follows the authority's last colon, which for a password holding an unescaped '#',
		// '/' or '?' is the password's first part. So no part of it is printed.
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
	adminExec(t, adminURL, server.Host, WithoutJIT(name))
	return DatabaseURL(*server, name, set...)
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
	replaced := []string{"dbname", "database"}
	for _, pair := range set {
		key, _, _ := strings.Cut(pair, "=")
		if name, err := url.PathUnescape(key); err == nil {
			replaced = append(replaced, name)
		}
	}
	server.Path, server.RawPath = "/"+database, ""
	server.RawQuery = strings.Join(append(otherPairs(server.RawQuery, replaced...), set...), "&")
	return server.String()
}

// otherPairs is the pairs of rawQuery but those whose key, unescaped, is one of names: every other
// pair kept byte for byte and in its order, an empty pair dropped (DatabaseURL says why).
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
// (otherPairs), as DatabaseURL keeps them, so an options pair still connects. A URL that does
// not parse fails t with no part of it printed, since the URL carries a credential.
func WithConnectDeadline(t testing.TB, databaseURL string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		// url.Parse's error, and the error it wraps, can quote the URL's password, so none of it is printed.
		t.Fatal("the URL of the test database does not parse")
	}
	parsed.RawQuery = strings.Join(append(otherPairs(parsed.RawQuery, "connect_timeout"), "connect_timeout="+connectTimeout(t)), "&")
	return parsed.String()
}

// ServerKey is adminURL without its connect_timeout pairs, the server and query a test names apart
// from the bound WithConnectDeadline gives that test's connects, which differs from test to test
// of one binary: the key of a cache a harness keeps per server, such as the broker storetest's
// templates. A URL url.Parse refuses is its own key.
func ServerKey(adminURL string) string {
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
