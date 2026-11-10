// Package testdb gives a test a Postgres database of its own on the devbox and CI Postgres that
// LEGION_TEST_PG_DSN names, dropped when the test ends, so no test sees another's rows, and a way
// to reach it across a network's delay (Delayed). A test is skipped without it: there is nothing
// to run against.
//
// Database is an empty database. A test that needs the daemon's schema takes internal/testpg's
// Clone or CloneStore instead, which copy a template that package migrated once.
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
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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
// server ends the call with no connection and no database. Database is its one caller: one
// database per test. Its admin connection is opened through OnConnect, as every connection this
// package's fixtures make is, so a socket the server resets under the churn is tried again and
// the count below says how often.
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
	var conn *pgx.Conn
	if err := OnConnect(func() error {
		var opened error
		conn, opened = pgx.Connect(ctx, admin)
		return opened
	}); err != nil {
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
		return Drop(context.Background(), conn, name)
	}
	return database, drop, nil
}

// dropWindow and dropPause bound the plain drop's retry: how long a drop is tried again while
// Postgres says the database is still in use, and the wait between tries.
const (
	dropWindow = 15 * time.Second
	dropPause  = 200 * time.Millisecond
)

// connectTries and connectPause bound OnConnect's retry: how many times a connection is opened
// while the server resets it, and the wait between attempts.
const (
	connectTries = 3
	connectPause = 200 * time.Millisecond
)

// connectCounts is what every connection this test process opened did, printed beside the drops:
// a run whose connects all went at the first attempt says the churn costs nothing, and one that
// needed retries says how many, which is the measurement rather than a guess about the server.
var connectCounts struct {
	opened  atomic.Int64
	retried atomic.Int64
}

// OnConnect runs open until it answers no error, the server refuses it for a reason a second
// attempt would meet again, or connectTries attempts are spent.
//
// It exists because this package's fixtures open about 790 connections in a run of
// internal/daemon — a database per test, each with its own admin connection — where the serial
// suite opened them one at a time, and a connection that the server resets at accept under that
// churn ended a test at its first statement: measured once in 20 shuffled runs of 2026-11-09 and
// once in about 90 runs the day before, each time "read: connection reset by peer" at 0.12 s with
// no assertion reached and the postmaster unrestarted. A retry is the instrument rather than a
// criterion: the count below says whether the churn is costing connections, where excluding the
// run from a campaign would say nothing and could hide a defect of this very conversion.
//
// Only a reset, a refusal or a closed socket is retried. A server that ANSWERED — a bad password,
// a database that does not exist, a connection refused for max_connections — fails at once,
// because a second attempt meets the same answer and a retry there would turn a defect into a
// wait (transientConnect).
func OnConnect(open func() error) error {
	var err error
	for try := 1; ; try++ {
		connectCounts.opened.Add(1)
		if err = open(); err == nil {
			if try > 1 {
				connectCounts.retried.Add(1)
			}
			return nil
		}
		if try == connectTries || !transientConnect(err) {
			return err
		}
		time.Sleep(connectPause)
	}
}

// transientConnect reports whether err is the socket going rather than the server answering: a
// reset, a refusal, a broken pipe or an end of file, each of which a later attempt may not meet.
// A *pgconn.PgError is the server's own answer and is never transient, whatever it says — the
// connection cap's own refusal (SQLSTATE 53300, "sorry, too many clients already") above all,
// since a retry that absorbed that would make k6's headroom true by construction and leave the
// campaign's zero refusals saying nothing.
func transientConnect(err error) bool {
	var answered *pgconn.PgError
	if errors.As(err, &answered) {
		return false
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}

// execer is what Drop needs of a connection or a pool, which pgx.Conn and pgxpool.Pool both are.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// dropCounts is what every drop of this test process did, which a package's Main prints: a run
// whose drops all went plainly needs no right to end a backend, and one that reached the force
// fallback says how often and how long it waited first. The counters are the evidence that the
// plain drop carries the fixture, rather than one green run.
var dropCounts struct {
	drops   atomic.Int64
	retried atomic.Int64
	forced  atomic.Int64
	longest atomic.Int64
}

// unwarmedConnects is how many connections the pools of this test process opened that the fixture
// did not open itself: each pool's pgxpool Stat().NewConnsCount() at teardown, less the ones the
// fixture established through OnConnect when it warmed the pool.
//
// It is the denominator the reset this retry exists for has no rate without. OnConnect covers
// every establishment the fixture makes; a pool that grows past its warmed set, or replaces a
// connection a health check destroyed, establishes one through pgxpool's own Constructor, which
// has no retry (measured: mapping_refresh_test.go:846 died of a reset while its run's line read
// "1220 connects, 0 needed a retry"). Two reset events across a campaign say nothing about how
// wide that path is; this count is how many chances it had.
var unwarmedConnects atomic.Int64

// shedWarmConns is how many warm connections pgxpool destroyed under a pool this process warmed:
// its Stat's MaxIdleDestroyCount and MaxLifetimeDestroyCount at teardown.
//
// It must read zero. A warm connection is destroyed only when it is older than MaxConnLifetime (an
// hour by default) or has been idle longer than MaxConnIdleTime (thirty minutes) — pgxpool's
// checkConnsHealth, which merely releases every other idle connection — and no test of this module
// lives that long, so the warmed set is the set the test uses. That is a reading of the library's
// defaults; this counter is the measurement of it, and the reason to keep it is the day somebody
// changes them: a warm set that quietly stopped covering anything would leave
// unwarmedConnects reading zero while every replacement went through the unretried Constructor.
var shedWarmConns atomic.Int64

// CountUnwarmedConnects records what a warmed pool did with its connections, read from its Stat at
// teardown: how many it opened beyond the ones the fixture warmed, and how many warm ones pgxpool
// destroyed for age or idleness. internal/testpg calls it when a pool it warmed is torn down.
func CountUnwarmedConnects(made, warmed, shed int64) {
	if beyond := made - warmed; beyond > 0 {
		unwarmedConnects.Add(beyond)
	}
	shedWarmConns.Add(shed)
}

// Drop drops the database name through exec: plainly, then again while Postgres answers SQLSTATE
// 55006 (object_in_use) up to dropWindow, and `with (force)` as the last attempt.
//
// `with (force)` ends every backend still attached, which needs pg_signal_backend or a superuser
// login: CI's test login is a superuser and a Legion issue pod's is not, so a fixture that always
// forced could not finish a package on a pod. A caller closes its own pool before this runs — both
// fixtures of this module register the drop first, and t.Cleanup is LIFO — but Postgres tears a
// backend down after the client has closed its socket, so a drop landing inside that window finds
// a connection and answers 55006 rather than a permission error. Retrying the plain drop covers
// that window; the force is left for a connection that really is somebody else's, and is the only
// attempt that needs the right.
//
// Measured on this issue's pod, PostgreSQL 18.6 with fsync off, ten clones of the daemon's
// migrated template: the forced drop means 18.8 ms against a plain drop's 17.1 ms, so the clause
// never bought speed.
func Drop(ctx context.Context, exec execer, name string) error {
	started := time.Now()
	defer func() {
		dropCounts.drops.Add(1)
		elapsed := int64(time.Since(started))
		for {
			longest := dropCounts.longest.Load()
			if elapsed <= longest || dropCounts.longest.CompareAndSwap(longest, elapsed) {
				return
			}
		}
	}()
	deadline := time.Now().Add(dropWindow)
	var err error
	tries := 0
	for {
		if _, err = exec.Exec(ctx, "drop database "+name); err == nil {
			if tries > 0 {
				dropCounts.retried.Add(1)
			}
			return nil
		}
		tries++
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "55006" || time.Now().After(deadline) {
			break
		}
		time.Sleep(dropPause)
	}
	dropCounts.forced.Add(1)
	if _, forced := exec.Exec(ctx, "drop database "+name+" with (force)"); forced != nil {
		// The forced drop ended the cleanup, so its error is the one reported; the plain drop's
		// says what the force was for.
		return fmt.Errorf("drop %s: %w, after the plain drop answered: %v", name, forced, err)
	}
	return nil
}

// DropLine is one line naming what this process's drops did, for the package Main that prints it.
func DropLine() string {
	return fmt.Sprintf("testdb: %d drops, %d needed a retry, %d reached the force fallback, longest %s; %d connects, %d needed a retry, %d opened outside the warm set, %d warm connections shed",
		dropCounts.drops.Load(), dropCounts.retried.Load(), dropCounts.forced.Load(),
		time.Duration(dropCounts.longest.Load()).Round(time.Millisecond),
		connectCounts.opened.Load(), connectCounts.retried.Load(), unwarmedConnects.Load(), shedWarmConns.Load())
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
