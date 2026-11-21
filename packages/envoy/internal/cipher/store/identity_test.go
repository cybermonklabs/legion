package store

import (
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/pgmigrate/pgmigratetest"
)

// The broker's runner judges what broker_schema_migrations records of each version against the
// set's file of that version before it applies anything, by the rule
// pgmigratetest.RunIdentityChecks holds it to.
func TestMigrateJudgesTheRecordedIdentityOfEveryVersion(t *testing.T) {
	pgmigratetest.RunIdentityChecks(t, versionTable, func(t *testing.T) (pgmigratetest.Database, pgmigratetest.Migrate) {
		s := emptyDatabaseStore(t)
		return s.Pool, s.migrate
	})
}

// The table main's runner from before identities leaves, as every production row is today,
// records a version for every file of the shipped set and no name: the first boot of this runner
// fills every row and refuses none (pgmigratetest.CheckVersionsOfAnEarlierRunner, which simulates
// that table's shape with this runner and does not run main's). The check is the one
// `envoy-broker versions` runs, on the store's own pool.
func TestTheShippedSetMatchesADatabaseAnEarlierRunnerMigrated(t *testing.T) {
	s := emptyDatabaseStore(t)
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("load the shipped set: %v", err)
	}
	pgmigratetest.CheckVersionsOfAnEarlierRunner(t, versionTable, s.Pool, s.Migrate, s.CheckVersions, len(migrations))
}
