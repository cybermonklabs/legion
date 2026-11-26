package testpg

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// Every database this harness hands a test runs with JIT off, and the setting is the database's
// own (pg_settings.source 'database'), not the server's: a server whose own jit were off would
// answer off from its default, and the source tells the two apart. Clone's database answers it,
// and so does testdb.Database's, each read through a pool, since Clone's URL carries the pool keys
// only a pgx pool reads. The template refuses connections, so its own setting is read from the
// server's catalog (pg_db_role_setting), where WithoutJIT writes it.
func TestEveryDatabaseRunsWithoutJIT(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for what, databaseURL := range map[string]string{
		"Clone's database":           Clone(t, "legion_testpg_jit"),
		"testdb.Database's database": testdb.Database(t, "legion_testpg_jit"),
	} {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			t.Fatalf("open a pool on %s: %v", what, err)
		}
		var setting, source string
		err = pool.QueryRow(ctx, `select setting, source from pg_settings where name = 'jit'`).Scan(&setting, &source)
		pool.Close()
		if err != nil {
			t.Fatalf("read jit on %s: %v", what, err)
		}
		if setting != "off" || source != "database" {
			t.Errorf("%s runs with jit = %s, from the %s; want off, set on the database", what, setting, source)
		}
	}
	admin, err := pgxpool.New(ctx, Clone(t, "legion_testpg_jit"))
	if err != nil {
		t.Fatalf("open a pool on a clone: %v", err)
	}
	defer admin.Close()
	var templateOff bool
	if err := admin.QueryRow(ctx, `
		select coalesce(bool_or(setting = 'jit=off'), false)
		from pg_database d
		join pg_db_role_setting s on s.setdatabase = d.oid and s.setrole = 0, unnest(s.setconfig) setting
		where d.datname = $1`, shared.template).Scan(&templateOff); err != nil {
		t.Fatalf("read the settings of the template %s: %v", shared.template, err)
	}
	if !templateOff {
		t.Errorf("the template %s holds no jit = off of its own (pg_db_role_setting); want the one WithoutJIT writes", shared.template)
	}
}
