package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// TestSeedRefusesAnotherDatabaseBeforeItMigratesIt: seed pointed at a database that holds a
// project of somebody else's, and no Dispatch schema, refuses it naming that project, and leaves
// it as it found it: no schema_migrations, which a migration creates before it applies anything.
func TestSeedRefusesAnotherDatabaseBeforeItMigratesIt(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	for _, statement := range []string{
		`drop schema public cascade`,
		`create schema public`,
		`create table projects (key text primary key, owner text not null)`,
		`insert into projects (key, owner) values ('ELSE', 'somebody else')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	var stdout, stderr bytes.Buffer
	status := run(ctx, []string{"seed", "--database-url", database.Pool.Config().ConnString()}, &stdout, &stderr)
	if status != 1 || !strings.Contains(stderr.String(), "the database holds the projects ELSE") {
		t.Errorf("seed answered %d, %q; want 1 and the refusal naming the project ELSE", status, stderr.String())
	}
	var migrated bool
	if err := database.Pool.QueryRow(ctx, `select to_regclass('schema_migrations') is not null`).Scan(&migrated); err != nil {
		t.Fatalf("look for schema_migrations: %v", err)
	}
	if migrated {
		t.Error("seed migrated the database it refused: it holds schema_migrations")
	}
}
