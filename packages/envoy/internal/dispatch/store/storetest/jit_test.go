package storetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Every database this package hands a test runs with JIT off, as production's does, and the
// setting is the database's own (pg_settings.source 'database'), not the server's: a server whose
// own jit were off would answer off from its default, and the source tells the two apart. Open's
// clone answers it on its own pool and on a second one, and Empty's database on a connection of
// its own. Open's template refuses connections, so its own setting is read from the server's
// catalog (pg_db_role_setting), where WithoutJIT writes it.
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
	templates.Lock()
	template := templates.byKey[openKey].name
	templates.Unlock()
	var templateOff bool
	if err := database.Pool.QueryRow(ctx, `
		select coalesce(bool_or(setting = 'jit=off'), false)
		from pg_database d
		join pg_db_role_setting s on s.setdatabase = d.oid and s.setrole = 0, unnest(s.setconfig) setting
		where d.datname = $1`, template).Scan(&templateOff); err != nil {
		t.Fatalf("read the settings of Open's template %s: %v", template, err)
	}
	if !templateOff {
		t.Errorf("Open's template %s holds no jit = off of its own (pg_db_role_setting); want the one WithoutJIT writes", template)
	}
	conn, err := pgx.Connect(ctx, Empty(t))
	if err != nil {
		t.Fatalf("connect to Empty's database: %v", err)
	}
	requireNoJIT("Empty's database", conn.QueryRow(ctx, read))
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close the connection to Empty's database: %v", err)
	}
}
