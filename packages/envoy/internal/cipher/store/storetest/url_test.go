package storetest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Each URL is a database of the test's own, a copy of the migrated template: it records every
// migration the store embeds, a table one test makes is not in another's database, and the
// database is gone once its test ends. Control: a URL that handed two tests one database (or
// one schema of one database) fails the second check, and one that skipped the copy's migrated
// tables fails the first.
func TestEachURLIsAMigratedDatabaseOfItsOwn(t *testing.T) {
	t.Parallel()
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
