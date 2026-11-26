package storetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Every database this package hands a test runs with JIT off, as production's does, and the
// setting is the database's own (pg_settings.source 'database'), not the server's: a server whose
// own jit were off would answer off from its default, and the source tells the two apart. Open's
// clone answers it on its own pool and on a second one, and so do Empty's database and
// OtherDatabase's, each on a connection of its own.
func TestEveryDatabaseRunsWithoutJIT(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	requireNoJIT := func(what string, row pgx.Row) {
		t.Helper()
		var setting, source string
		if err := row.Scan(&setting, &source); err != nil {
			t.Fatalf("read jit on %s: %v", what, err)
		}
		if setting != "off" || source != "database" {
			t.Errorf("%s runs with jit = %s, from the %s; want off, set on the database", what, setting, source)
		}
	}
	const read = `select setting, source from pg_settings where name = 'jit'`
	database := Open(t)
	requireNoJIT("Open's clone", database.Pool.QueryRow(ctx, read))
	requireNoJIT("a second pool on Open's clone", SecondProcess(t, database).Pool.QueryRow(ctx, read))
	for what, databaseURL := range map[string]string{"Empty's database": Empty(t), "OtherDatabase's database": OtherDatabase(t)} {
		conn, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			t.Fatalf("connect to %s: %v", what, err)
		}
		requireNoJIT(what, conn.QueryRow(ctx, read))
		if err := conn.Close(ctx); err != nil {
			t.Fatalf("close the connection to %s: %v", what, err)
		}
	}
}
