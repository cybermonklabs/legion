package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// A run gives its advisory key back after each migration, so another boot can record any version
// of the set between two migrations of the run, after the run's prepare read the table. Before
// each migration the runner judges the table again under the key (pgmigrate.RecheckRecorded): the
// version's own row where there is one, and else every row against the whole set, selecting the
// identity columns the prepare added rather than reading the table's columns again. Here a run
// applies 0001, 0002 and 0003 through migrate, and once 0001's statement has run
// (afterNonTransactionalStatement: 0001 is a concurrent build) another session records version 3.
// Under another file's identity, the run refuses before it applies 0002, and leaves 0002 and 0003
// unapplied. Under the set's own identity, with 0003's insert made as the boot that applied it
// made it, the run applies 0002 and does not apply 0003 a second time: one row of step 3, and
// version 3 keeps the other session's row.
//
// Each case runs with 0002 in either apply mode (applyMigrationOnce and
// applyNonTransactionalMigration), and over either version table: the one the runner creates, and
// one an earlier runner created before the versions became timestamps, whose version column is an
// integer and which has no identity column until the prepare adds them. The set holds no
// timestamped version, so the run leaves that column an integer (prepareVersionTable), and the
// recheck's statements and the record's insert, sent with pgx.QueryExecModeExec, compare and store
// the version in an integer column too.
//
// Control: make RecheckRecorded answer "not applied" without reading every row where the version
// has no row, and the refusal rows fail: 0002 is applied beside a version 3 the set refuses. Make
// it answer "not applied" without reading the version's own row, and the own-identity rows fail:
// the run applies 0003 again, and its record collides with the other session's.
func TestARunJudgesTheVersionsAnotherBootRecordsBetweenTwoOfItsMigrations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	otherFile := pgmigrate.Identity{Name: "0003_other.up.sql", Checksum: strings.Repeat("0", 64)}
	otherSessionAppliedAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, table := range []struct {
		name, create, versionType string
	}{
		{name: "the runner's own table", versionType: "bigint"},
		{name: "an integer table from before the widening", versionType: "integer",
			create: "create table schema_migrations (version integer primary key, applied_at timestamptz not null default now())"},
	} {
		for _, second := range []struct {
			mode, text, applied string
		}{
			{"transactional", "insert into rn_steps (step) values (2)", "select exists(select 1 from rn_steps where step = 2)"},
			{"non-transactional", "-- pgmigrate:non-transactional\ncreate index concurrently if not exists rn_steps_second on rn_steps (step);",
				"select to_regclass('rn_steps_second') is not null"},
		} {
			for _, ownIdentity := range []bool{false, true} {
				label := table.name + ", " + second.mode + " 0002, version 3 recorded under another file"
				if ownIdentity {
					label = table.name + ", " + second.mode + " 0002, version 3 recorded under the set's own identity"
				}
				t.Run(label, func(t *testing.T) {
					t.Parallel()
					store := openEmptyTestStore(t)
					for _, statement := range []string{"create table rn_steps (step integer)", table.create} {
						if statement == "" {
							continue
						}
						if _, err := store.Pool.Exec(ctx, statement); err != nil {
							t.Fatalf("%s: %v", statement, err)
						}
					}
					set := fstest.MapFS{
						"migrations/0001_first.up.sql":  {Data: []byte("-- pgmigrate:non-transactional\ncreate index concurrently if not exists rn_steps_first on rn_steps (step);")},
						"migrations/0002_second.up.sql": {Data: []byte(second.text)},
						"migrations/0003_third.up.sql":  {Data: []byte("insert into rn_steps (step) values (3)")},
					}
					migrations, err := pgmigrate.Load(set, "migrations")
					if err != nil {
						t.Fatalf("load the set: %v", err)
					}
					third := migrations[2]
					// What the other session records of version 3, and what the run leaves: a refusal
					// that leaves 0002 and 0003 unapplied, or 0002 applied beside the other session's
					// 0003.
					recorded, wantVersions, wantThirdSteps := otherFile, []int{1, 3}, 0
					if ownIdentity {
						recorded, wantVersions, wantThirdSteps = third.RecordedIdentity(), []int{1, 2, 3}, 1
					}
					// The hook runs after each concurrent build's statement: 0001's, and 0002's where
					// it is one. The other session writes once, after 0001's, on a pooled
					// connection: the runner's own was hijacked out of the pool.
					builds := 0
					store.afterNonTransactionalStatement = func(context.Context, *pgx.Conn) error {
						builds++
						if builds > 1 {
							return nil
						}
						if ownIdentity {
							if _, err := store.Pool.Exec(ctx, "insert into rn_steps (step) values (3)"); err != nil {
								return err
							}
						}
						_, err := store.Pool.Exec(ctx, "insert into schema_migrations (version, applied_at, name, checksum, applied_checksum) values (3, $1, $2, $3, $4)",
							otherSessionAppliedAt, recorded.Name, recorded.Checksum, third.Checksum)
						return err
					}

					err = store.migrate(ctx, set)
					if builds == 0 {
						t.Fatalf("migrate = %v, and no concurrent build's statement ran: the other session recorded nothing, so the row proves nothing", err)
					}
					if !ownIdentity {
						if !pgmigrate.IsRefusal(err) || !strings.Contains(err.Error(), "version 3 is recorded as "+otherFile.Name) {
							t.Errorf("migrate = %v; want a refusal of version 3, recorded as %s", err, otherFile.Name)
						}
					} else if err != nil {
						t.Fatalf("migrate: %v", err)
					}
					if got := recordedVersions(t, store); !slices.Equal(got, wantVersions) {
						t.Errorf("schema_migrations records %v; want %v", got, wantVersions)
					}
					var secondApplied bool
					if err := store.Pool.QueryRow(ctx, second.applied).Scan(&secondApplied); err != nil {
						t.Fatalf("look for what 0002 makes: %v", err)
					}
					if secondApplied != ownIdentity {
						t.Errorf("0002 applied %v; want %v", secondApplied, ownIdentity)
					}
					var thirdSteps int
					if err := store.Pool.QueryRow(ctx, "select count(*) from rn_steps where step = 3").Scan(&thirdSteps); err != nil {
						t.Fatalf("count 0003's rows: %v", err)
					}
					if thirdSteps != wantThirdSteps {
						t.Errorf("rn_steps holds %d rows of step 3; want %d, the other session's alone", thirdSteps, wantThirdSteps)
					}
					var name string
					var appliedAt time.Time
					if err := store.Pool.QueryRow(ctx, "select name, applied_at from schema_migrations where version = 3").Scan(&name, &appliedAt); err != nil {
						t.Fatalf("read version 3's row: %v", err)
					}
					if name != recorded.Name || !appliedAt.Equal(otherSessionAppliedAt) {
						t.Errorf("version 3 is recorded as %s at %s; want the other session's row, %s at %s", name, appliedAt, recorded.Name, otherSessionAppliedAt)
					}
					if got := versionColumnType(t, store); got != table.versionType {
						t.Errorf("schema_migrations.version is %s after the run; want %s", got, table.versionType)
					}
				})
			}
		}
	}
}
