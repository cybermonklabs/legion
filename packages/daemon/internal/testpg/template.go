// Clone, CloneStore and Main give a package's tests one migrated database each at the cost of one
// create database, rather than one migration run each.
//
// Running every migration, each in its own committed transaction, is most of the Postgres work a
// test of this module does, so a package's wall time scaled with its test count times the speed of
// the Postgres it ran against. Here the process migrates one template database (prepare) and each
// test clones it.
//
// A clone copies the template's files (strategy file_copy). Postgres's default from 15, strategy
// wal_log, writes every page of the template into the write-ahead log instead, which for a schema
// this size is megabytes of log per test. A file copy writes a few hundred bytes of log; Postgres
// forces a checkpoint before and after the copy.
//
// A clone comes from internal/testpg/testdb's pool and goes back to it when its test ends, reset
// and read against the template (that package's pool.go): a run clones as many databases as it
// holds at once rather than one per test, because a create and a drop each force a checkpoint and
// every package testing against the same server queues on its one checkpointer.

package testpg

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// shared is this test process's one template database and the clone drops still in flight. Main
// sets prefix and main before the package's tests run; Clone refuses to run without them, since
// only Main waits for the drops and drops the template.
var shared struct {
	main   bool
	prefix string

	once     sync.Once
	err      error
	server   string
	admin    *pgxpool.Pool
	template string

	drops     sync.WaitGroup
	dropMu    sync.Mutex
	dropFails []string
}

// Clone is the URL of a database carrying every migration, cloned from this process's template and
// dropped at the test's end. A pool opened on it holds testdb.PoolConns connections unless
// LEGION_TEST_PG_DSN sets a size of its own, which it keeps (testdb.PoolBounded). Without
// LEGION_TEST_PG_DSN the test is skipped, as testdb.Database's is.
func Clone(t testing.TB, prefix string) string {
	t.Helper()
	if !shared.main {
		t.Fatal("testpg.Clone needs the package's TestMain to be: func TestMain(m *testing.M) { os.Exit(testpg.Main(\"<template prefix>\", m.Run)) }. Main's prefix names the one template of this test binary, which no clone's prefix has to equal: a package may clone under prefixes of its own, and does.")
	}
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(testdb.SkipMessage)
	}
	shared.once.Do(func() { shared.err = prepare(dsn) })
	if shared.err != nil {
		t.Fatal(shared.err)
	}
	return testdb.TemplateCopy(t, shared.server, shared.template, prefix)
}

// CloneStore is Clone opened, its pool closed when the test ends — before the clone is dropped,
// since Clone registered that cleanup first.
func CloneStore(t testing.TB, prefix string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), Clone(t, prefix))
	if err != nil {
		t.Fatalf("open the cloned database: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// ClonePool is Clone opened as a pool, closed when the test ends — before the clone is dropped,
// since Clone registered that cleanup first. The pool is built from store.PoolConfig, as the
// daemon's own is, so a test that wraps it with store.NewPool writes through the store's text
// rule.
func ClonePool(t testing.TB, prefix string) *pgxpool.Pool {
	t.Helper()
	config, err := store.PoolConfig(Clone(t, prefix))
	if err != nil {
		t.Fatalf("configure a pool on the cloned database: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a pool on the cloned database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Main runs a package's tests, waits for their clones to drop, drops the template Clone copied,
// and answers run's code — 1 instead of 0 when a drop failed, so a database left behind is a red
// run rather than a surprise on the next one. prefix names this package's template.
//
// It takes run rather than *testing.M so a package that has a TestMain of its own composes the
// two:
//
//	func TestMain(m *testing.M) { os.Exit(testpg.Main("legion_intake_test", func() int { return testnats.Main(m) })) }
func Main(prefix string, run func() int) int {
	shared.prefix, shared.main = prefix, true
	code := run()
	if shared.admin == nil {
		return code
	}
	defer shared.admin.Close()
	shared.drops.Wait()
	for _, failure := range shared.dropFails {
		fmt.Fprintf(os.Stderr, "testpg: %s\n", failure)
		if code == 0 {
			code = 1
		}
	}
	if shared.template == "" {
		return code
	}
	// Through testdb.Drop: the template takes no connection (prepare), so a forced drop of it ends
	// nothing, and Drop's retry waits out a backend that has not exited yet.
	if err := testdb.Drop(context.Background(), shared.admin, shared.template); err != nil {
		fmt.Fprintf(os.Stderr, "testpg: drop the template database %s: %v\n", shared.template, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// adminConns is the admin pool's size, whatever pool size LEGION_TEST_PG_DSN sets (adminConfig):
// the pool is this file's own bookkeeping, for the template it migrates, not a pool a test opens.
// Every clone a test takes comes from internal/testpg/testdb's pool, on that package's admin pool.
const adminConns = 4

// prepare opens the admin pool and migrates the template every Clone copies. It logs one line to
// stderr naming the package's prefix and how long the template took: a run that logs two has lost
// the memoisation this file exists for, which is what the test of it counts.
//
// The admin database's URL and the template's are testdb.DatabaseURL's, as each clone's is
// (Clone), and this file makes no rewrite of its own: that helper's comment says what it refuses.
// The template's pool takes the pool keys testdb.PoolBounded gives it; the admin pool takes none
// of them and holds adminConns connections (adminConfig).
// The template's URL is made before the `create database`, so a refused rewrite leaves no
// database behind.
func prepare(dsn string) error {
	shared.server = dsn
	config, err := adminConfig(dsn)
	if err != nil {
		return err
	}
	ctx := context.Background()
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("open the admin pool: %w", err)
	}
	shared.admin = admin

	started := time.Now()
	name, err := testdb.RandomName(shared.prefix + "_template")
	if err != nil {
		return err
	}
	templateURL, err := testdb.DatabaseURL(dsn, name)
	if err != nil {
		return err
	}
	// The template is opened only by the store that migrates it.
	if templateURL, err = testdb.PoolBounded(dsn, templateURL); err != nil {
		return err
	}
	// A file copy of template1, for the reason each clone of it is one.
	if _, err := admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		return fmt.Errorf("create the template database: %w", err)
	}
	shared.template = name
	st, err := store.Open(ctx, templateURL)
	if err != nil {
		return fmt.Errorf("open the template database: %w", err)
	}
	_, err = st.Migrate(ctx)
	st.Close()
	if err != nil {
		return fmt.Errorf("migrate the template database: %w", err)
	}
	fmt.Fprintf(os.Stderr, "testpg: migrated template %s in %s\n", shared.prefix, time.Since(started).Round(time.Millisecond))
	// Postgres refuses to clone a database another session is connected to. Refusing connections
	// to the template keeps a stray session from making a clone wait or fail.
	if _, err := admin.Exec(ctx, "alter database "+name+" with allow_connections false"); err != nil {
		return fmt.Errorf("close the template database to connections: %w", err)
	}
	return nil
}

// adminConfig is the admin pool's configuration on the server dsn names: that server's postgres
// database through testdb.DatabaseURL, which takes out every pool key the DSN sets, and
// adminConns connections, whatever pool size the DSN sets for the tests' own pools.
func adminConfig(dsn string) (*pgxpool.Config, error) {
	adminURL, err := testdb.DatabaseURL(dsn, "postgres")
	if err != nil {
		return nil, err
	}
	config, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		return nil, fmt.Errorf("parse the admin database URL: %w", err)
	}
	config.MaxConns = adminConns
	return config, nil
}
