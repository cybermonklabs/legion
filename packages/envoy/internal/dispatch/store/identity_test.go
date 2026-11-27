package store

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/pgmigrate/pgmigratetest"
)

// Dispatch's runner judges what schema_migrations records of each version against the set's file
// of that version before it applies anything, by the rule pgmigratetest.RunIdentityChecks holds it
// to.
func TestMigrateJudgesTheRecordedIdentityOfEveryVersion(t *testing.T) {
	pgmigratetest.RunIdentityChecks(t, versionTable, func(t *testing.T) (pgmigratetest.Database, pgmigratetest.Migrate) {
		store := openEmptyTestStore(t)
		return store.Pool, store.migrate
	})
}

// The table main's runner from before identities leaves, as every production row is today,
// records a version for every file of the shipped set and no name: the first boot of this runner
// fills every row and refuses none (pgmigratetest.CheckVersionsOfAnEarlierRunner, which simulates
// that table's shape with this runner and does not run main's). The check is the one
// `envoy-dispatch versions` runs.
func TestTheShippedSetMatchesADatabaseAnEarlierRunnerMigrated(t *testing.T) {
	t.Parallel()
	store := openEmptyTestStore(t)
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("load the shipped set: %v", err)
	}
	databaseURL := store.Pool.Config().ConnString()
	pgmigratetest.CheckVersionsOfAnEarlierRunner(t, versionTable, store.Pool, store.Migrate,
		func(ctx context.Context) (*pgmigrate.VersionCheck, error) { return CheckVersions(ctx, databaseURL) }, len(migrations))
}

// Dispatch's runner gives its advisory key back between its first judgment of schema_migrations
// (prepareVersionTable) and each migration, so another boot can record any version in between.
// Before it applies a migration, the runner judges every row again against its whole set, so it
// applies nothing while the table records a version under an identity the set refuses: here a
// boot carrying 0001, 0002 and 0003 prepares, another boot then records version 3 under another
// file, and the first boot's application of the pending 0002 is refused and leaves nothing, in
// either apply mode. The control is the same sequence without the other boot's row, which applies
// 0002.
//
// Control of the check itself: make pgmigrate.RecheckRecorded answer "not applied" without the
// whole-table judgment when the version has no row, as it did when it read that version alone,
// and the refused rows fail: 0002 is applied beside a version 3 the set refuses.
func TestMigrateAppliesNothingWhileAnotherBootRecordsAConflictingVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, second := range map[string]struct {
		text, applied string
	}{
		"transactional":     {"create table race_second (id integer)", "select to_regclass('race_second') is not null"},
		"non-transactional": {"-- pgmigrate:non-transactional\ncreate index concurrently if not exists race_first_id on race_first (id);", "select to_regclass('race_first_id') is not null"},
	} {
		for _, conflicting := range []bool{true, false} {
			label := name + ", control: no other boot"
			if conflicting {
				label = name + ", another boot records version 3 under another file"
			}
			t.Run(label, func(t *testing.T) {
				store := openEmptyTestStore(t)
				first := fstest.MapFS{"migrations/0001_first.up.sql": {Data: []byte("create table race_first (id integer)")}}
				if err := store.migrate(ctx, first); err != nil {
					t.Fatalf("migrate 0001: %v", err)
				}
				set := fstest.MapFS{
					"migrations/0001_first.up.sql":  first["migrations/0001_first.up.sql"],
					"migrations/0002_second.up.sql": {Data: []byte(second.text)},
					"migrations/0003_third.up.sql":  {Data: []byte("create table race_third (id integer)")},
				}
				migrations, err := pgmigrate.Load(set, "migrations")
				if err != nil {
					t.Fatalf("load the set: %v", err)
				}
				if err := store.prepareVersionTable(ctx, migrations); err != nil {
					t.Fatalf("prepare: %v", err)
				}
				if conflicting {
					if _, err := store.Pool.Exec(ctx, "insert into schema_migrations (version, name, checksum) values (3, '0003_other.up.sql', $1)", strings.Repeat("0", 64)); err != nil {
						t.Fatalf("record version 3 as the other boot: %v", err)
					}
				}
				err = store.applyMigration(ctx, migrations, migrations[1])
				var applied, recorded bool
				if err := store.Pool.QueryRow(ctx, second.applied).Scan(&applied); err != nil {
					t.Fatalf("look for what 0002 makes: %v", err)
				}
				if err := store.Pool.QueryRow(ctx, "select exists(select 1 from schema_migrations where version = 2)").Scan(&recorded); err != nil {
					t.Fatalf("look for version 2: %v", err)
				}
				if !conflicting {
					if err != nil || !applied || !recorded {
						t.Fatalf("control: applyMigration = %v, applied %v, recorded %v; want 0002 applied and recorded", err, applied, recorded)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "version 3 is recorded as 0003_other.up.sql") {
					t.Errorf("applyMigration = %v; want a refusal naming version 3 and 0003_other.up.sql", err)
				}
				if applied || recorded {
					t.Errorf("0002 applied %v, recorded %v, beside a version 3 the set refuses; want neither", applied, recorded)
				}
			})
		}
	}
}
