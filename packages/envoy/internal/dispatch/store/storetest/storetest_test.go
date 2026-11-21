package storetest

import (
	"context"
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// Open hands each test a database of its own carrying this build's whole migration set, cloned
// from the template named after that set. The harness's own mechanics - the file copy, the drop,
// the lock and the build - are internal/testpg/pgtemplate's tests; this is the part that is
// Dispatch's: the set it migrates. Broken, Dispatch's tests run on a schema that is not this
// build's.
func TestOpenHandsEachTestThisBuildsWholeSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	first := Open(t) // finds or builds the template, or skips
	second := Open(t)
	var firstName, secondName string
	if err := first.Pool.QueryRow(ctx, "select current_database()").Scan(&firstName); err != nil {
		t.Fatalf("read the first database's name: %v", err)
	}
	if err := second.Pool.QueryRow(ctx, "select current_database()").Scan(&secondName); err != nil {
		t.Fatalf("read the second database's name: %v", err)
	}
	if firstName == secondName {
		t.Fatalf("two calls were handed the same database %s", firstName)
	}

	migrations, err := pgmigrate.Load(os.DirFS(".."), "migrations")
	if err != nil {
		t.Fatalf("load the migration set: %v", err)
	}
	var recorded int
	if err := first.Pool.QueryRow(ctx, "select count(*) from schema_migrations").Scan(&recorded); err != nil {
		t.Fatalf("count the database's recorded migrations: %v", err)
	}
	if recorded != len(migrations) {
		t.Errorf("the database records %d migrations, want the set's %d", recorded, len(migrations))
	}
}
