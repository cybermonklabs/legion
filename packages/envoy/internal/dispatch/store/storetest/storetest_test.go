package storetest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// Open clones the template by copying its files (the package comment says why). The test reads
// the write-ahead log of the one backend that runs a clone (pg_stat_get_backend_wal, Postgres 18),
// so what other sessions on the server write does not count.
func TestACloneWritesTheLogOfAFileCopyNotOfTheTemplate(t *testing.T) {
	Open(t) // makes Open's template, or skips the test
	templates.Lock()
	template := templates.byKey[openKey].name
	templates.Unlock()
	ctx := context.Background()
	conn, err := shared.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an admin connection: %v", err)
	}
	defer conn.Release()
	var templateBytes int64
	if err := conn.QueryRow(ctx, "select pg_database_size($1)", template).Scan(&templateBytes); err != nil {
		t.Fatalf("read the template's size: %v", err)
	}

	name := randomName(t, "dispatch_test_")
	before := backendWALBytes(t, conn)
	if _, err := conn.Exec(ctx, cloneStatement(name, template)); err != nil {
		t.Fatalf("clone the template: %v", err)
	}
	dropDatabase(name)
	written := backendWALBytes(t, conn) - before

	if written > templateBytes/100 {
		t.Fatalf("one clone wrote %d bytes of write-ahead log, more than a hundredth of the %d-byte template: it went through the log", written, templateBytes)
	}
	t.Logf("one clone wrote %d bytes of write-ahead log; the template holds %d bytes", written, templateBytes)
}

// backendWALBytes is the write-ahead log conn's backend has written. pg_stat_get_backend_wal
// reads the counters the backend last flushed, and the forced flush runs as its own statement ends.
func backendWALBytes(t *testing.T, conn *pgxpool.Conn) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "select pg_stat_force_next_flush()"); err != nil {
		t.Fatalf("force the backend's statistics flush: %v", err)
	}
	var written int64
	if err := conn.QueryRow(ctx, "select wal_bytes::bigint from pg_stat_get_backend_wal(pg_backend_pid())").Scan(&written); err != nil {
		t.Fatalf("read the backend's write-ahead log: %v", err)
	}
	return written
}

// TestNoDatabaseIsDroppedWhileATestOfThePackageHoldsOne: a drop holds a transaction id while it
// forces a checkpoint, and the live readers' event horizon waits for every transaction id open on
// the server (events.Horizon), so a test's database is not dropped while another test of the
// package holds one. The parent holds a database; its subtest opens one and ends; once every drop
// started so far has ended, the subtest's database is still there, and the parent's live views
// had no drop to wait on.
//
// Control: the database dropped in the background as its test ends (the harness before hold), and
// the subtest's database is gone while the parent still holds its own.
func TestNoDatabaseIsDroppedWhileATestOfThePackageHoldsOne(t *testing.T) {
	Open(t)
	var child string
	t.Run("child", func(t *testing.T) { child = currentDatabase(t, Open(t)) })
	shared.drops.Wait()
	if !databaseExists(t, child) {
		t.Errorf("the subtest's database %s was dropped while its parent still held a database: a drop holds every live view of the package behind its checkpoint", child)
	}
}

// TestAFinishedDatabaseIsDroppedOnceNoTestHoldsOne: a test that asks for a database while no test
// of the package holds one drops the databases whose tests ended, once dropBatch of them wait,
// before its own is made. With dropBatch at 1, the first subtest's database is gone once the
// second subtest has made its own; the test itself holds none.
//
// Control: hold never dropping a finished database, and the first subtest's database is still
// there.
func TestAFinishedDatabaseIsDroppedOnceNoTestHoldsOne(t *testing.T) {
	batch := dropBatch
	dropBatch = 1
	t.Cleanup(func() { dropBatch = batch })
	var first string
	t.Run("first", func(t *testing.T) { first = currentDatabase(t, Open(t)) })
	t.Run("second", func(t *testing.T) { Open(t) })
	if databaseExists(t, first) {
		t.Errorf("the first subtest's database %s is still there after the second made its own while no test held one", first)
	}
}

// currentDatabase is the name of the database database's pool connects to.
func currentDatabase(t *testing.T, database *store.Store) string {
	t.Helper()
	var name string
	if err := database.Pool.QueryRow(context.Background(), "select current_database()").Scan(&name); err != nil {
		t.Fatalf("read the database's name: %v", err)
	}
	return name
}

// databaseExists reports whether the server holds a database named name.
func databaseExists(t *testing.T, name string) bool {
	t.Helper()
	var exists bool
	if err := shared.admin.QueryRow(context.Background(), "select exists (select 1 from pg_database where datname = $1)", name).Scan(&exists); err != nil {
		t.Fatalf("read whether %s exists: %v", name, err)
	}
	return exists
}
