package store

import (
	"context"
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/pgmigrate/pgmigratetest"
)

// identityStore is the runner and the database pgmigratetest's identity checks drive.
type identityStore struct{ store *Store }

func (s identityStore) Migrate(ctx context.Context, fsys fs.FS) error {
	return s.store.migrate(ctx, fsys)
}

func (s identityStore) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return s.store.Pool.Query(ctx, sql, args...)
}

func (s identityStore) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.store.Pool.QueryRow(ctx, sql, args...)
}

func (s identityStore) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return s.store.Pool.Exec(ctx, sql, args...)
}

// Dispatch's runner judges what schema_migrations records of each version against the set's file
// of that version before it applies anything, by the rule pgmigratetest.RunIdentityChecks holds it
// to.
func TestMigrateJudgesTheRecordedIdentityOfEveryVersion(t *testing.T) {
	pgmigratetest.RunIdentityChecks(t, versionTable, func(t *testing.T) pgmigratetest.IdentityStore {
		return identityStore{openEmptyTestStore(t)}
	})
}

// A database that main's runner from before identities migrated, as every production row is
// today, records a version for every file of the shipped set and no name: the first boot of this
// runner fills every row and refuses none (pgmigratetest.CheckVersionsOfAnEarlierRunner).
func TestTheShippedSetMatchesADatabaseAnEarlierRunnerMigrated(t *testing.T) {
	store := openEmptyTestStore(t)
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("load the shipped set: %v", err)
	}
	databaseURL := store.Pool.Config().ConnString()
	pgmigratetest.CheckVersionsOfAnEarlierRunner(t, versionTable, identityStore{store}, store.Migrate,
		func(ctx context.Context) (*pgmigrate.VersionCheck, error) { return CheckVersions(ctx, databaseURL) }, len(migrations))
}
