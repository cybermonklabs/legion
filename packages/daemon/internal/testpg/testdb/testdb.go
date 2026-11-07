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
// RandomName and DatabaseURL are exported for internal/testpg itself, which names its template
// and each clone of it with them; no other package needs either.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
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
// URL and the function that drops it, which also closes the admin connection create opened. The
// URL is dsn with its path replaced, so it keeps dsn's query. Database is its one caller.
func create(dsn, prefix string) (string, func() error, error) {
	base, err := url.Parse(dsn)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is printed.
		return "", nil, fmt.Errorf("parse LEGION_TEST_PG_DSN: %w", errors.Unwrap(err))
	}
	admin := *base
	admin.Path = "/postgres"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return "", nil, fmt.Errorf("connect to the admin database: %w", err)
	}
	name, err := RandomName(prefix)
	if err != nil {
		_ = conn.Close(ctx)
		return "", nil, err
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
	return DatabaseURL(*base, name), drop, nil
}

// dropWindow and dropPause bound the plain drop's retry: how long a drop is tried again while
// Postgres says the database is still in use, and the wait between tries.
const (
	dropWindow = 15 * time.Second
	dropPause  = 200 * time.Millisecond
)

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
	return fmt.Sprintf("testdb: %d drops, %d needed a retry, %d reached the force fallback, longest %s",
		dropCounts.drops.Load(), dropCounts.retried.Load(), dropCounts.forced.Load(),
		time.Duration(dropCounts.longest.Load()).Round(time.Millisecond))
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

// DatabaseURL is server with its path replaced by database, so the URL keeps server's query. It is
// exported for internal/testpg, which addresses its template and each clone with it.
func DatabaseURL(server url.URL, database string) string {
	server.Path = "/" + database
	return server.String()
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
