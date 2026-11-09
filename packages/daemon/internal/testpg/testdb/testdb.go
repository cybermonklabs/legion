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
// every caller of it is here or in internal/testpg, but for internal/daemon's live proofs, which
// make a database of their own for a run (live_agent_proof_test.go). RandomName is exported for
// internal/testpg alone, which names its template and each clone of it with it.
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
		if _, err := conn.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
		return nil
	}
	return database, drop, nil
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
// The refusal says what pgx read of each string, and prints no password.
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
// and the four a refusal prints. It holds no password.
type reading struct {
	host     string
	port     uint16
	user     string
	database string
}

func (r reading) String() string {
	return fmt.Sprintf("host %s, port %d, user %s, database %s", r.host, r.port, r.user, r.database)
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
// The answer stays a URL, which is the form the DSN came in. A reader downstream takes that form
// and no keyword/value connection string (host=… dbname=…): packages/pi-legion's rig scenarios
// read a run's database out of a DSN with new URL(), and Bun.SQL takes a URL.
func rewriteDSN(name, dsn string, want rewrite) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is printed.
		return "", fmt.Errorf("read %s as a URL: %s", name, parseReason(err))
	}
	if want.database != "" {
		parsed.Path = "/" + want.database
	}
	if want.host != "" {
		parsed.Host = net.JoinHostPort(want.host, strconv.Itoa(int(want.port)))
	}
	rewritten := parsed.String()
	before, beforeErr := pgconn.ParseConfig(dsn)
	after, afterErr := pgconn.ParseConfig(rewritten)
	if beforeErr == nil && afterErr == nil && readingOf(after) == want.applied(readingOf(before)) {
		return rewritten, nil
	}
	refusal := fmt.Sprintf("the rewrite of %s is not the connection it was asked for:\n"+
		"\tpgx reads %s as %s\n"+
		"\tpgx reads the rewrite as %s",
		name, name, dsnReading(before, beforeErr), dsnReading(after, afterErr))
	if beforeErr == nil {
		refusal += "\n\tthe rewrite must be " + want.applied(readingOf(before)).String()
	}
	return "", errors.New(refusal)
}

// dsnReading is what pgx read from one DSN, in one line: the four fields rewriteDSN compares, or
// the refusal that stands in for them. It prints no password, since a refusal names the
// connection string it refused and the string holds one.
func dsnReading(cfg *pgconn.Config, err error) string {
	if err != nil {
		return "nothing, because pgx refuses it: " + parseReason(err)
	}
	return readingOf(cfg).String()
}

// parseReason is why a parser refused a DSN, with no connection string in it. net/url names the
// URL it refused (url.Error) and pgx quotes the connection string back ahead of its own reason
// (pgconn.ParseConfigError, "cannot parse `<the DSN>`: <the reason>"), and either text holds the
// password. pgx redacts that password itself, and says in pgconn/errors.go that the redaction is
// best effort, so this cuts the string out rather than trust it.
func parseReason(err error) string {
	var badURL *url.Error
	if errors.As(err, &badURL) && badURL.Err != nil {
		return badURL.Err.Error()
	}
	var refused *pgconn.ParseConfigError
	if errors.As(err, &refused) {
		if quoted := strings.LastIndex(err.Error(), "`: "); quoted >= 0 {
			return err.Error()[quoted+len("`: "):]
		}
	}
	return err.Error()
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
