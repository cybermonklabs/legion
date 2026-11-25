package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// btreeGinVersion creates the extension btree_gin, whose operator classes a GIN index needs to hold
// a btree column beside an array, and holds no other statement.
const btreeGinVersion = 20261125125703

// btreeGinExtversion answers the version pg_extension records for btree_gin in store's database,
// and "" where the database has no btree_gin.
func btreeGinExtversion(t *testing.T, store *Store) string {
	t.Helper()
	var version string
	err := store.Pool.QueryRow(context.Background(), `select extversion from pg_extension where extname = 'btree_gin'`).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read btree_gin from pg_extension: %v", err)
	}
	return version
}

// Every migration, run on a new database, leaves pg_extension holding btree_gin. A new database
// holds none before the migrations run; one that did would come from a template that carries the
// extension, and on it this test could not tell whether a migration creates it.
func TestEveryMigrationLeavesANewDatabaseHoldingBtreeGin(t *testing.T) {
	if version := btreeGinExtversion(t, openEmptyTestStore(t)); version != "" {
		t.Fatalf("a new database holds btree_gin %s before any migration runs: its template carries the extension, so this test cannot tell whether a migration creates it", version)
	}
	if version := btreeGinExtversion(t, openMigratedTestStore(t, latestVersion)); version == "" {
		t.Fatal("every migration ran on a new database, and pg_extension holds no btree_gin after them")
	}
}

// A deployment where btree_gin was created by hand before the migrations ran keeps it as it was:
// every migration runs without error, and the extension keeps its version. The extension is created
// at 1.0, its first version, which is older than the version a create with no version clause
// installs (1.3 from PostgreSQL 11 on), so a migration that updated the extension, or dropped it and
// created it again, would change the version this test reads.
func TestTheMigrationsKeepTheVersionOfABtreeGinCreatedBeforeThem(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if _, err := store.Pool.Exec(ctx, `create extension btree_gin version '1.0'`); err != nil {
		t.Fatalf("create btree_gin 1.0 before any migration runs: %v", err)
	}
	before := btreeGinExtversion(t, store)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate a database where btree_gin %s was created before the migrations: %v", before, err)
	}
	var recorded bool
	if err := store.Pool.QueryRow(ctx, `select exists (select from schema_migrations where version = $1)`, btreeGinVersion).Scan(&recorded); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if !recorded {
		t.Fatalf("the migrations ran and did not record %d, so none of them met the extension created before them", btreeGinVersion)
	}
	if after := btreeGinExtversion(t, store); after != before {
		t.Errorf("btree_gin is at version %q after the migrations, want the %q it was created at", after, before)
	}
}

// The migration's one statement is create extension if not exists btree_gin, comments and white
// space aside. A version clause would name a version that a server may not ship. A schema clause
// would name a schema of its own, where the migrations create every other object, vector included,
// in the first schema of the search_path the runner pins. Any other statement can name a table,
// which the pre-deploy census refuses above its limit
// (TestCensusAdmitsTheBtreeGinMigrationAtAnyTableSize).
func TestTheBtreeGinMigrationIsOneCreateExtensionStatement(t *testing.T) {
	const want = "create extension if not exists btree_gin"
	for _, migration := range shippedMigrations(t) {
		if migration.Version != btreeGinVersion {
			continue
		}
		statement := strings.TrimSuffix(strings.Join(strings.Fields(pgmigrate.WithoutComments(migration.SQL)), " "), ";")
		if statement != want {
			t.Fatalf("%s holds %q, comments and white space aside; want the one statement %q, with no version clause and no schema clause", migration.Name, statement, want)
		}
		return
	}
	t.Fatalf("the shipped set has no migration of version %d", btreeGinVersion)
}
