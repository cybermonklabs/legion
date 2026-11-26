package storetest

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The template lock is the server's whatever database a builder's URL names: a builder that names
// the server through a database of its own waits for a holder of the lock on the server's postgres
// database, and a holder on the builder's own database holds it up not at all. Postgres keeps an
// advisory lock per database, so a lock taken in the database the URL names let two builders
// naming different databases hold two locks, and either one's sweep of unfinished builds could drop
// the other's build while it migrated. A lock taken where the URL points fails both rows: the first
// sees the builder return while the holder holds, the second sees it wait.
func TestTheTemplateLockIsTheServersWhateverDatabaseTheURLNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := serverURL(t)
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", DatabaseURLEnv, errors.Unwrap(err))
	}
	// builder is a URL naming a database of the test's own on the server, whose connections carry
	// an application_name of their own, so the waiters read are this builder's and no other
	// binary's.
	builder := func(t *testing.T) (string, string) {
		t.Helper()
		app, err := testdb.NewDatabaseName("broker_lock_test")
		if err != nil {
			t.Fatal(err)
		}
		return testdb.DatabaseOn(t, raw, "", "broker_lock_test", "application_name="+app), app
	}
	// hold takes templateLockKey on a connection of its own to databaseURL, closed when t ends.
	hold := func(t *testing.T, databaseURL string) *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			t.Fatalf("connect the holder: %v", err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		if _, err := conn.Exec(ctx, "select pg_advisory_lock(hashtext($1))", templateLockKey); err != nil {
			t.Fatalf("take the template lock: %v", err)
		}
		return conn
	}
	build := func(databaseURL string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := makeTemplate(databaseURL)
			done <- err
		}()
		return done
	}
	waiting := func(t *testing.T, holder *pgx.Conn, app string) bool {
		t.Helper()
		var n int
		if err := holder.QueryRow(ctx, `select count(*) from pg_stat_activity
			where application_name = $1 and wait_event_type = 'Lock' and $2 = any(pg_blocking_pids(pid))`,
			app, int32(holder.PgConn().PID())).Scan(&n); err != nil {
			t.Fatalf("read the builder's lock waits: %v", err)
		}
		return n > 0
	}

	t.Run("a holder on the server's postgres database holds up a builder that names another", func(t *testing.T) {
		databaseURL, app := builder(t)
		holder := hold(t, testdb.DatabaseURL(*server, lockDatabase))
		done := build(databaseURL)
		testwait.Until(t, "the builder to wait behind the holder of the template lock, or to return", func() (bool, bool) {
			select {
			case err := <-done:
				t.Fatalf("the builder returned (%v) while a holder on the server's %s database held the template lock: its lock is not the server's", err, lockDatabase)
			default:
			}
			return true, waiting(t, holder, app)
		})
		if _, err := holder.Exec(ctx, "select pg_advisory_unlock(hashtext($1))", templateLockKey); err != nil {
			t.Fatalf("give the template lock back: %v", err)
		}
		if err := testwait.Receive(t, "the builder to return once the holder gave the lock back", done); err != nil {
			t.Fatalf("the builder, once the lock was given back: %v", err)
		}
	})
	t.Run("control: a holder on the builder's own database holds it up not at all", func(t *testing.T) {
		databaseURL, app := builder(t)
		holder := hold(t, databaseURL)
		done := build(databaseURL)
		if err := testwait.Receive(t, "the builder to return while a holder on its own database holds the lock", done); err != nil {
			t.Fatalf("the builder: %v", err)
		}
		if waiting(t, holder, app) {
			t.Fatal("the builder's connection still waits behind the holder on its own database")
		}
	})
}

// Each URL is a database of the test's own, a copy of the migrated template: it records every
// migration the store embeds, a table one test makes is not in another's database, and the
// database is gone once its test ends. Control: a URL that handed two tests one database (or
// one schema of one database) fails the second check, and one that skipped the copy's migrated
// tables fails the first.
func TestEachURLIsAMigratedDatabaseOfItsOwn(t *testing.T) {
	ctx := context.Background()
	entries, err := os.ReadDir("../migrations")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".up.sql") {
			want++
		}
	}
	if want == 0 {
		t.Fatal("../migrations holds no .up.sql file, so there is nothing to compare the copy with")
	}
	connect := func(t *testing.T, dsn string) *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(ctx) })
		return conn
	}
	var first string
	t.Run("first", func(t *testing.T) {
		conn := connect(t, URL(t))
		var recorded int
		if err := conn.QueryRow(ctx, "select count(*) from broker_schema_migrations").Scan(&recorded); err != nil {
			t.Fatalf("read the copy's applied migrations: %v", err)
		}
		if recorded != want {
			t.Fatalf("the copy records %d migrations, want the %d the store embeds", recorded, want)
		}
		if err := conn.QueryRow(ctx, "select current_database()").Scan(&first); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "create table storetest_probe (x int)"); err != nil {
			t.Fatal(err)
		}
		other := connect(t, URL(t))
		var database string
		var probe *string
		if err := other.QueryRow(ctx, "select current_database(), to_regclass('storetest_probe')::text").Scan(&database, &probe); err != nil {
			t.Fatal(err)
		}
		if database == first || probe != nil {
			t.Fatalf("a second URL of the same test is database %s and sees storetest_probe as %v; want a database other than %s, without it", database, probe, first)
		}
	})
	admin := connect(t, serverURL(t))
	var left bool
	if err := admin.QueryRow(ctx, "select exists (select from pg_database where datname = $1)", first).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left {
		t.Fatalf("database %s is still on the server after its test ended", first)
	}
}
