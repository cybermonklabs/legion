// Package testdb gives a test a Postgres database of its own on the devbox and CI Postgres that
// LEGION_TEST_PG_DSN names, dropped when the test ends, so no test sees another's rows, and a way
// to reach it across a network's delay (Delayed). A test is skipped without it: there is nothing
// to run against.
//
// Database is an empty database. A test that needs the daemon's schema takes internal/testpg's
// Clone or CloneStore instead, which copy a template that package migrated once.
//
// A package whose tests migrate into the database their DSN names, rather than making one per
// test, takes one database for the whole run instead: RunDatabaseMain from its TestMain, and
// RunDatabaseFor wherever it reads the DSN. The two mechanisms compose, and a package that needs
// both nests them (internal/daemon's TestMain).
//
// This package imports nothing of this module, and nothing added here may: migrating the template
// needs internal/store, so internal/testpg imports it, and a test binary of internal/store that
// imported internal/testpg back would be a cycle. Keeping the schema-free half apart is what lets
// internal/store's own tests take the same fixture every other package's tests take.
//
// DatabaseURL is the one rewrite of LEGION_TEST_PG_DSN a test binary of this module makes, and
// its own comment says which callers make that rewrite and what it refuses. RandomName is
// exported for internal/testpg alone, which names its template and each clone of it with it.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Image is the Postgres server image a developer starts for the daemon's own tests: PostgreSQL 18
// (tag 18, server 18.6-1.pgdg13+2), pinned by digest through the mirror. 18 is also the major
// version of the Postgres service CI's daemon test job runs (ci.yaml's `test` job), and
// the version envoy-infra gives a new Cloud SQL instance.
const Image = "mirror.gcr.io/library/postgres@sha256:979bccfde35445db618c65107d27d08c6c21339d8dc3ad4c830c6633947be0e4"

// SkipMessage is what a test is skipped with when there is no Postgres to run it against. It is
// exported for the packages whose own skip says the same thing (internal/daemon's testDSN).
const SkipMessage = "LEGION_TEST_PG_DSN is unset, so there is no Postgres to test against. In a Legion pod, read skill://legion-worker/references/tools.md, \"The database your tests use\"; never the sessions database. Elsewhere, start one and set it:\n" +
	"  docker run -d --name legion-pg --cpus 2 --memory 2g -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion -p 127.0.0.1::5432 " + Image + "\n" +
	"  port=$(docker port legion-pg 5432/tcp | head -1 | sed 's/.*://')\n" +
	"  LEGION_TEST_PG_DSN=postgres://legion:legion@127.0.0.1:$port/legion go test ./..."

// Database is the URL of a new, empty database named <prefix>_<random>, dropped at the test's end.
func Database(t testing.TB, prefix string) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(SkipMessage)
	}
	database, drop, err := create(dsn, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := drop(); err != nil {
			t.Error(err)
		}
	})
	return database
}

// create makes an empty database named <prefix>_<random> on the server dsn names, and answers its
// URL and the function that drops it, which also closes the admin connection create opened. Both
// URLs it connects or points a caller at — the admin database's and the new database's — come
// from DatabaseURL, and it makes them before it connects, so a DSN whose rewrite names another
// server ends the call with no connection and no database. Database and RunDatabaseMain are its
// two callers: one database per test, one per run.
func create(dsn, prefix string) (string, func() error, error) {
	admin, err := DatabaseURL(dsn, "postgres")
	if err != nil {
		return "", nil, err
	}
	name, err := RandomName(prefix)
	if err != nil {
		return "", nil, err
	}
	database, err := DatabaseURL(dsn, name)
	if err != nil {
		return "", nil, err
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return "", nil, fmt.Errorf("connect to the admin database: %w", err)
	}
	// A copy of template1's files. Created the default way, from PostgreSQL 15, every page of
	// template1 goes into the write-ahead log: 4.4 MB of log for each empty database.
	if _, err := conn.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		_ = conn.Close(ctx)
		return "", nil, fmt.Errorf("create %s: %w", name, err)
	}
	drop := func() error {
		defer func() { _ = conn.Close(context.Background()) }()
		if err := Drop(context.Background(), conn, name); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
		return nil
	}
	return database, drop, nil
}

// insufficientPrivilege is PostgreSQL's 42501, which a forced drop answers when a session of the
// database belongs to another role.
const insufficientPrivilege = "42501"

// Execer is what Drop needs of a connection or a pool: one statement.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Drop drops name, waiting an autovacuum worker out. `drop database ... with (force)` terminates
// every other session of the database, which PostgreSQL refuses to a login that is neither
// superuser nor a member of pg_signal_backend as soon as one session belongs to another role — an
// autovacuum worker, which attaches to a database a test has just filled. Every Legion pod shares
// one server whose test login holds neither privilege, so the refusal is waited out for 30
// seconds; any other error is the caller's at once (dispatch://INFRA-714 round six).
//
// It lives here rather than in internal/testpg because this package imports nothing of the
// module, so every package's tests can take it — internal/store's among them, whose own tests
// cannot import internal/testpg at all.
func Drop(ctx context.Context, admin Execer, name string) error {
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; ; attempt++ {
		_, err := admin.Exec(ctx, "drop database "+name+" with (force)")
		if err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (still refused after %d attempts over 30s: a session of another role, an autovacuum worker, holds the database and this login may not terminate it)", err, attempt+1)
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// RandomName is prefix with a random suffix, so two databases of one prefix never collide. It is
// exported for internal/testpg, which names its template and each clone with it.
func RandomName(prefix string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(suffix[:]), nil
}

// DatabaseURL is the URL of database on the server dsn names: dsn with its path replaced, so the
// URL keeps dsn's query. Every database a test binary of this module names from
// LEGION_TEST_PG_DSN is named here — create above, internal/testpg's prepare and Clone, and the
// live proofs' takeOwnDatabase (internal/daemon/live_agent_proof_test.go) — so one place reads
// each rewrite back, and no caller checks itself. Delayed's proxy URL is the one other rewrite
// of a DSN this package makes, and it takes the same check (rewriteDSN below). dsn is what
// LEGION_TEST_PG_DSN holds, which every caller read from that variable, and a refusal names it.
//
// A rewrite pgx reads another host, port or user from than dsn is refused, and so is one pgx
// reads another database from than database. Go's net/url ends the user name at the last `@` and
// it starts the query at the first `?`. libpq and pgx do neither. So a DSN whose host libpq reads
// as loopback, which is the only DSN the live proofs' shell guard admits
// (scripts/e2e/lib/loopback-dsn.sh), can rewrite into a URL pgx connects to another machine with.
//
// A refusal names which of host, port, user and database pgx reads from the rewrite other than
// the connection the rewrite was asked for. It prints no value and no text of either parser,
// because the value it would print is the misreading itself: a password a parser reads as a
// host, a port or a database is printed as that host, port or database, and the text a parser
// quotes is the part of the string it stopped in. A string a parser refuses earns a fixed line
// naming which string it was, the caller's DSN or the rewrite built from it, and nothing more.
//
// What that costs a reader of a refusal: they learn which fields moved, and not what they moved
// to. The DSN is the caller's own, and the caller can read it.
//
// This check is the second of the two frames that hold a live proof to the server its caller
// named, and it is the one place the Go half of those frames is written. The first frame is that
// shell guard: it reads the caller's DSN with libpq before anything starts, and it reaches no
// further than the shell. The comment above require_loopback_dsn is the one place the shell half
// is written. testdb_test.go drives the cases of this check.
func DatabaseURL(dsn, database string) (string, error) {
	return rewriteDSN("LEGION_TEST_PG_DSN", dsn, rewrite{database: database})
}

// rewrite is what a caller of rewriteDSN changes about the DSN it was given: the database the
// rewrite must name (DatabaseURL), or the address it must send the connection to (Delayed). A
// field left empty is one the rewrite keeps.
type rewrite struct {
	database string
	host     string
	port     uint16
}

// applied is read with this rewrite in it, which is what pgx must read from the rewritten DSN.
func (r rewrite) applied(read reading) reading {
	if r.database != "" {
		read.database = r.database
	}
	if r.host != "" {
		read.host, read.port = r.host, r.port
	}
	return read
}

// reading is what pgconn.ParseConfig read from a DSN: the four fields a rewrite keeps or sets,
// and the four a refusal names. It holds no password, and a refusal prints no value of it.
type reading struct {
	host     string
	port     uint16
	user     string
	database string
}

// moved names the fields of this reading that are not asked's, in the order the fields are
// written above, and nothing where the two readings are the same. It is what a refusal says of a
// rewrite: which fields moved, and no value of one.
func (r reading) moved(asked reading) string {
	var fields []string
	if r.host != asked.host {
		fields = append(fields, "host")
	}
	if r.port != asked.port {
		fields = append(fields, "port")
	}
	if r.user != asked.user {
		fields = append(fields, "user")
	}
	if r.database != asked.database {
		fields = append(fields, "database")
	}
	switch len(fields) {
	case 0:
		return ""
	case 1:
		return fields[0]
	default:
		return strings.Join(fields[:len(fields)-1], ", ") + " and " + fields[len(fields)-1]
	}
}

// readingOf is what pgx read from a DSN it took.
func readingOf(cfg *pgconn.Config) reading {
	return reading{host: cfg.Host, port: cfg.Port, user: cfg.User, database: cfg.Database}
}

// rewriteDSN is dsn rewritten as want says: one rewrite of dsn's URL, read back with
// pgconn.ParseConfig, which is the parser pgx connects through. It answers that rewrite where pgx
// reads it as the reading of dsn with want in it, and a refusal and no DSN at all otherwise. name
// is what the caller read dsn from, and it starts the refusal.
//
// Three strings go through a parser here: dsn through net/url, dsn through pgx, and the rewrite
// through pgx. A parser that refuses one of them earns the fixed line that names which string it
// was. Where pgx reads both strings, this function compares the rewrite's reading with dsn's
// reading with want in it, and the refusal names the fields that differ. DatabaseURL's comment
// says why no line holds a value.
//
// The answer stays a URL, which is the form the DSN came in. A reader downstream takes that form
// and no keyword/value connection string (host=… dbname=…): packages/pi-legion's rig scenarios
// read a run's database out of a DSN with new URL(), and Bun.SQL takes a URL.
func rewriteDSN(name, dsn string, want rewrite) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("%s: net/url refuses it", name)
	}
	if want.database != "" {
		parsed.Path = "/" + want.database
	}
	if want.host != "" {
		parsed.Host = net.JoinHostPort(want.host, strconv.Itoa(int(want.port)))
	}
	rewritten := parsed.String()
	before, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("%s: pgx refuses it", name)
	}
	after, err := pgconn.ParseConfig(rewritten)
	if err != nil {
		return "", fmt.Errorf("the rewrite of %s: pgx refuses it", name)
	}
	if moved := readingOf(after).moved(want.applied(readingOf(before))); moved != "" {
		return "", fmt.Errorf(
			"the rewrite of %s is not the connection it was asked for: pgx reads another %s from it",
			name, moved)
	}
	return rewritten, nil
}

// runServer is the server RunDatabaseMain made the run's database on, named by serverKey, and
// runDatabase the URL of that database; both are empty until a RunDatabaseMain with
// LEGION_TEST_PG_DSN set runs.
var (
	runServer   string
	runDatabase string
)

// serverKey is the part of a DSN that names the server rather than a database on it, so two DSNs
// that differ only in the database they name have one key. It reads the DSN with
// pgconn.ParseConfig, the parser DatabaseURL and the driver read it with, so the key is the host,
// the port and the user pgx connects as. A DSN pgx refuses is its own key: every key below holds
// two NUL bytes, and a DSN a test read out of its environment holds none, so no refused DSN
// equals a key.
func serverKey(dsn string) string {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return dsn
	}
	return fmt.Sprintf("%s\x00%d\x00%s", cfg.Host, cfg.Port, cfg.User)
}

// RunDatabaseMain makes one database named <prefix>_<random> for the whole run on the server
// LEGION_TEST_PG_DSN names, runs run, drops that database and returns run's code; a drop that
// fails prints why and turns a passing run into exit 1. A package whose tests migrate into the
// database their DSN names calls it from TestMain, where every test reads the run's database
// through RunDatabaseFor, so two runs of the package on one server never write each other's rows
// and neither writes the database that server serves. With LEGION_TEST_PG_DSN unset it makes
// nothing and returns run(), so those tests skip as they do without a server.
//
// It is not internal/testpg's Main, which prepares the template Clone copies: the two are
// different databases for different needs, and a package that wants both nests them, each under a
// prefix of its own so a census of either name says which it counted:
//
//	os.Exit(testpg.Main("legion_daemon_test", func() int {
//		return testdb.RunDatabaseMain("legion_daemon_run", m.Run)
//	}))
//
// It takes run rather than *testing.M so that it wraps another TestMain helper.
func RunDatabaseMain(prefix string, run func() int) int {
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		return run()
	}
	database, drop, err := create(dsn, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "make the package's run database: %v\n", err)
		return 1
	}
	runServer, runDatabase = serverKey(dsn), database
	code := run()
	if err := drop(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// RunDatabaseFor is the URL of the database RunDatabaseMain made for this run when dsn names the
// server that database is on, which is the host, the port and the user pgx reads from both
// (serverKey), and dsn itself otherwise: a test that points LEGION_TEST_PG_DSN at a server of its
// own keeps what it set. Without a run database — no RunDatabaseMain, or LEGION_TEST_PG_DSN
// unset — it is dsn.
func RunDatabaseFor(dsn string) string {
	if runDatabase == "" || serverKey(dsn) != runServer {
		return dsn
	}
	return runDatabase
}

// DSNFile is a 0600 file under the test's temporary directory holding Database's URL, as the
// providers Secret's key holds the session database's.
func DSNFile(t testing.TB, prefix string) (dsn, file string) {
	t.Helper()
	dsn = Database(t, prefix)
	file = filepath.Join(t.TempDir(), "OMP_SESSION_SQL_DSN")
	if err := os.WriteFile(file, []byte(dsn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dsn, file
}
