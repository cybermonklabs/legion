package storetest

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/lockwaittest"
)

// A server URL that does not parse fails the template's build with no part of the URL in its
// error: not the first part of a password holding an unescaped '#', which even the error url.Parse
// wraps quotes. Control: that wrapped error does quote it, so a build that printed it would fail
// this test.
func TestATemplateServerURLThatDoesNotParseIsNeverPrinted(t *testing.T) {
	t.Parallel()
	const planted = "plantedfirstpart"
	raw := "postgres://u:" + planted + "#rest@db.example:5432/postgres"
	if _, err := url.Parse(raw); err == nil || !strings.Contains(errors.Unwrap(err).Error(), planted) {
		t.Fatalf("control: url.Parse of the planted URL answers %v; want an error whose wrapped error quotes the password's first part, or this test proves nothing", err)
	}
	_, err := makeTemplate(raw)
	if err == nil {
		t.Fatal("makeTemplate of a URL that does not parse answered no error")
	}
	for _, part := range []string{planted, "rest", "db.example"} {
		if strings.Contains(err.Error(), part) {
			t.Errorf("makeTemplate's error %q prints %q of the URL it could not parse; want no part of it", err, part)
		}
	}
}

// The template lock is the server's whatever database a builder's URL names: a builder that names
// the server through a database of its own waits for a holder of the lock on the server's postgres
// database, and a holder on the builder's own database holds it up not at all. Postgres keeps an
// advisory lock per database, so a lock taken in the database the URL names let two builders
// naming different databases hold two locks, and either one's sweep of unfinished builds could drop
// the other's build while it migrated. A lock taken where the URL points fails both rows: the first
// sees the builder return while the holder holds, the second sees it wait.
//
// The test runs on a Postgres server of its own (ownServer) and reads neither serverURL nor
// DatabaseURLEnv. The lock its first row holds is the one every broker test binary on a shared
// server takes to build its template, so on the DatabaseURLEnv server that row would hold up every
// other binary's build while it held the lock, and another binary's build would hold up this
// test's builders. Each builder connects as a login of its own (newLogin), whose template is not on
// the server, so it reaches the lock: a builder finding its template there would return before it
// took the lock (template), and a login's template is there once a builder of that login has run,
// so the second of two rows that shared a login would find it.
func TestTheTemplateLockIsTheServersWhateverDatabaseTheURLNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := ownServer(t)
	// builder is a URL naming a database of the test's own on the server, as a login of the
	// test's own that has no template yet, whose connections carry an application_name of their
	// own, so the waiters read are this builder's.
	builder := func(t *testing.T) (string, string) {
		t.Helper()
		login := newLogin(t, server, "broker_lock_test")
		app, err := testdb.NewDatabaseName("broker_lock_test")
		if err != nil {
			t.Fatal(err)
		}
		return testdb.DatabaseOn(t, login.String(), "", "broker_lock_test", "application_name="+app), app
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
			where application_name = $1 and `+lockwaittest.WaitingOn("advisory")+` and $2 = any(pg_blocking_pids(pid))`,
			app, int32(holder.PgConn().PID())).Scan(&n); err != nil {
			t.Fatalf("read the builder's lock waits: %v", err)
		}
		return n > 0
	}

	t.Run("a holder on the server's postgres database holds up a builder that names another", func(t *testing.T) {
		databaseURL, app := builder(t)
		holder := holdTemplateLock(t, testdb.DatabaseURL(server, lockDatabase))
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
		holder := holdTemplateLock(t, databaseURL)
		done := build(databaseURL)
		err := testwait.Until(t, "the builder to return while a holder on its own database holds the lock", func() (error, bool) {
			select {
			case err := <-done:
				return err, true
			default:
			}
			if waiting(t, holder, app) {
				t.Fatal("the builder waits behind the holder on its own database: its lock is not the server's")
			}
			return nil, false
		})
		if err != nil {
			t.Fatalf("the builder: %v", err)
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
	want := embeddedMigrations(t)
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
	// pg_database is the server's, the same from any of its databases, so it is read in an empty
	// database of this test's own, never in the one the server's URL names.
	admin := connect(t, testdb.DatabaseOn(t, serverURL(t), "", "broker_url_test"))
	var left bool
	if err := admin.QueryRow(ctx, "select exists (select from pg_database where datname = $1)", first).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left {
		t.Fatalf("database %s is still on the server after its test ended", first)
	}
}
