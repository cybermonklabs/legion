package storetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Every database this package hands a test runs with JIT off, as production's does, and the
// setting is the database's own, not the server's: a server whose own jit were off would answer off
// from its default. Open's clone answers it on its own pool and on a second one, and Empty's
// database on a connection of its own. Dispatch's pools send jit = off at startup
// (store.sessionSettings), so a session of Open's pools reads the setting from the client, and the
// database's own setting is read from the server's catalog (pg_db_role_setting), where WithoutJIT
// writes it; a server default is never there. Open's template refuses connections, so its own
// setting is read from the catalog too.
func TestEveryDatabaseRunsWithoutJIT(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	requireNoJIT := func(what string, row pgx.Row) {
		t.Helper()
		var setting, source string
		var own bool
		if err := row.Scan(&setting, &source, &own); err != nil {
			t.Fatalf("read jit on %s: %v", what, err)
		}
		if setting != "off" || !own {
			t.Errorf("%s runs with jit = %s, from the %s, and the database holds jit = off of its own: %t; want off, set on the database", what, setting, source, own)
		}
	}
	const read = `select s.setting, s.source, coalesce((
		select bool_or(c = 'jit=off')
		from pg_database d
		join pg_db_role_setting r on r.setdatabase = d.oid and r.setrole = 0, unnest(r.setconfig) c
		where d.datname = current_database()), false)
	from pg_settings s where s.name = 'jit'`
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
