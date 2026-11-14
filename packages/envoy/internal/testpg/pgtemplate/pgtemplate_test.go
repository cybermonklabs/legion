package pgtemplate

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// dispatch is the harness these tests run, made as internal/dispatch/store/storetest makes its
// own: its template is the one every Dispatch test process on the server shares, so these tests
// build nothing a Dispatch run would not build, and leave nothing it would not leave.
var dispatch = New(Dispatch, store.MigrationsFingerprint, func(ctx context.Context, url string) error {
	database, err := store.Open(ctx, url, store.WithMaxConns(16))
	if err != nil {
		return err
	}
	defer database.Pool.Close()
	return database.Migrate(ctx)
})

func TestMain(m *testing.M) { os.Exit(dispatch.Main(m)) }

// serverURL is the Postgres server the Dispatch tests run against, skipping t when none is named.
func serverURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run the template harness's tests")
	}
	return raw
}

// connect opens a connection to databaseURL, closed when t ends: before the database's drop,
// which t.Cleanup's reverse order gives a database cloned before the connection opened.
func connect(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to a test database: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// A clone copies the template's files (the package comment says why). The test reads the
// write-ahead log of the one backend that runs a clone (pg_stat_get_backend_wal, Postgres 18), so
// what other sessions on the server write does not count.
func TestACloneWritesTheLogOfAFileCopyNotOfTheTemplate(t *testing.T) {
	dispatch.Clone(t, serverURL(t), "dispatch_test_") // finds or builds the template
	ctx := context.Background()
	conn, err := dispatch.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an admin connection: %v", err)
	}
	defer conn.Release()
	var templateBytes int64
	if err := conn.QueryRow(ctx, "select pg_database_size($1)", dispatch.name).Scan(&templateBytes); err != nil {
		t.Fatalf("read the template's size: %v", err)
	}

	name := randomName("dispatch_test_")
	before := backendWALBytes(t, conn)
	if _, err := conn.Exec(ctx, cloneStatement(name, dispatch.name)); err != nil {
		t.Fatalf("clone the template: %v", err)
	}
	dispatch.drop(name)
	written := backendWALBytes(t, conn) - before

	if written > templateBytes/100 {
		t.Fatalf("one clone wrote %d bytes of write-ahead log, more than a hundredth of the %d-byte template: it went through the log", written, templateBytes)
	}
	t.Logf("one clone wrote %d bytes of write-ahead log; the template holds %d bytes", written, templateBytes)
}

// Every test is handed a database of its own, carrying the migrations and nothing another test
// wrote, and that database leaves the server when the test ends. Broken, a test reads another
// test's rows, or a run leaves its databases behind on a server the next run has to share.
func TestEachTestIsHandedADatabaseOfItsOwnWhichIsDroppedAfterwards(t *testing.T) {
	ctx := context.Background()
	raw := serverURL(t)
	first := connect(t, dispatch.Clone(t, raw, "dispatch_test_"))
	second := connect(t, dispatch.Clone(t, raw, "dispatch_test_"))
	var firstName, secondName string
	if err := first.QueryRow(ctx, "select current_database()").Scan(&firstName); err != nil {
		t.Fatalf("read the first database's name: %v", err)
	}
	if err := second.QueryRow(ctx, "select current_database()").Scan(&secondName); err != nil {
		t.Fatalf("read the second database's name: %v", err)
	}
	if firstName == secondName {
		t.Fatalf("two tests were handed the same database %s", firstName)
	}

	// Every project names a default assignee since main's 20261111210103 migration, so the row
	// this writes names one too.
	if _, err := first.Exec(ctx, `insert into projects (key, name, default_assignee) values ('TEST', 'Test', 'alice@d.example')`); err != nil {
		t.Fatalf("write a row in the first database: %v", err)
	}
	var elsewhere int
	if err := second.QueryRow(ctx, `select count(*) from projects where key = 'TEST'`).Scan(&elsewhere); err != nil {
		t.Fatalf("read the second database: %v", err)
	}
	if elsewhere != 0 {
		t.Fatalf("what one test wrote is in another test's database: it holds %d such row(s)", elsewhere)
	}

	// The drop runs in the background when the inner test ends, and Main waits for it; what this
	// reads is that the cleanup is registered at all, on a database of a subtest of its own.
	var dropped string
	t.Run("a database of its own", func(t *testing.T) {
		inner := connect(t, dispatch.Clone(t, raw, "dispatch_test_"))
		if err := inner.QueryRow(ctx, "select current_database()").Scan(&dropped); err != nil {
			t.Fatalf("read the inner database's name: %v", err)
		}
	})
	conn, err := dispatch.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an admin connection: %v", err)
	}
	defer conn.Release()
	dispatch.drops.Wait()
	var held int
	if err := conn.QueryRow(ctx, "select count(*) from pg_database where datname = $1", dropped).Scan(&held); err != nil {
		t.Fatalf("read whether the server still holds %s: %v", dropped, err)
	}
	if held != 0 {
		t.Fatalf("%s is still on the server after the test that took it ended", dropped)
	}
}

// The template is the migration set's, whole, and left on the server: that is what lets the next
// test process clone it instead of applying every migration again, which is most of the Postgres
// work a package used to do. Broken, either every process migrates a template of its own again,
// or - worse - a process clones a database that is not this build's schema.
func TestTheTemplateIsTheMigrationSetsAndStaysForTheNextProcess(t *testing.T) {
	ctx := context.Background()
	clone := connect(t, dispatch.Clone(t, serverURL(t), "dispatch_test_")) // finds or builds the template

	fingerprint, err := store.MigrationsFingerprint()
	if err != nil {
		t.Fatalf("fingerprint the migration set: %v", err)
	}
	if want := Dispatch.Prefix + fingerprint; dispatch.name != want {
		t.Fatalf("the template is %s, want %s: a process whose set differs would clone this one", dispatch.name, want)
	}

	// What a process arriving second does: it takes the lock, finds the template already there,
	// and builds nothing.
	unlock, err := dispatch.lock(ctx)
	if err != nil {
		t.Fatalf("take the template lock a second time: %v", err)
	}
	defer unlock()
	built, err := dispatch.exists(ctx, dispatch.name)
	if err != nil {
		t.Fatal(err)
	}
	if !built {
		t.Fatalf("%s is not on the server, so the next process would migrate one of its own", dispatch.name)
	}
	var halfBuilt int
	if err := dispatch.admin.QueryRow(ctx, "select count(*) from pg_database where starts_with(datname, $1)", dispatch.buildingPrefix()).Scan(&halfBuilt); err != nil {
		t.Fatalf("count the half-built templates: %v", err)
	}
	if halfBuilt != 0 {
		t.Errorf("%d database(s) named %s* are still on the server: a build left its scratch behind", halfBuilt, dispatch.buildingPrefix())
	}

	// The clone carries the whole set, which is what a second process is promised by finding the
	// template already named.
	migrations, err := pgmigrate.Load(os.DirFS("../../dispatch/store"), "migrations")
	if err != nil {
		t.Fatalf("load the migration set: %v", err)
	}
	var recorded int
	if err := clone.QueryRow(ctx, "select count(*) from schema_migrations").Scan(&recorded); err != nil {
		t.Fatalf("count the clone's recorded migrations: %v", err)
	}
	if recorded != len(migrations) {
		t.Errorf("the clone records %d migrations, want the set's %d", recorded, len(migrations))
	}
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
