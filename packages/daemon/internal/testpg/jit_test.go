package testpg

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// Every database this harness hands a test runs with JIT off, as production's does, and the
// setting is the database's own (pg_settings.source 'database'), not the server's: a server whose
// own jit were off would answer off from its default, and the source tells the two apart. Clone's
// database answers it, and so does testdb.Database's. Each is read through a pool, since Clone's
// URL carries the pool keys only a pgx pool reads.
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
}
