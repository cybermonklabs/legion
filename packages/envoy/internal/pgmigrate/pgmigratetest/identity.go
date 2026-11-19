package pgmigratetest

import (
	"context"
	"io/fs"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// Database is the database a runner records its versions in, as a store's pool reaches it.
type Database interface {
	pgmigrate.Querier
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// Migrate runs a store's runner over fsys's migrations directory.
type Migrate func(ctx context.Context, fsys fs.FS) error

// The files of RunIdentityChecks's sets. secondEdited is second as an edit in place leaves it: the
// same name, other text. Neither is applied again once its version is recorded, so the edit only
// changes the file's checksum.
const (
	identityFirst        = "create table identity_first (id integer)"
	identitySecond       = "create table identity_second (id integer)"
	identitySecondEdited = "create table identity_second (id integer, note text)"
	identityThird        = "create table identity_third (id integer)"
)

// The log message of pgmigrate.LogFixes, which RunIdentityChecks counts.
const backfillMessage = "pgmigrate: recorded the identity of a migration applied before the runners recorded identities"

// identitySet is a migrations directory holding files, each name a file of it and each value its
// text.
func identitySet(files map[string]string) fstest.MapFS {
	set := fstest.MapFS{}
	for name, text := range files {
		set["migrations/"+name] = &fstest.MapFile{Data: []byte(text)}
	}
	return set
}

// sum is the sha256 of text in lower-case hex, as the runners record a file's checksum.
func sum(text string) string { return pgmigrate.Checksum([]byte(text)) }

// identity is the identity of the file name holding text.
func identity(name, text string) pgmigrate.Identity {
	return pgmigrate.Identity{Name: name, Checksum: sum(text)}
}

// earlierRunner leaves table on db as a runner from before identities left it: none of the
// identity columns, every row's version and applied_at as they were.
func earlierRunner(t *testing.T, db Database, table string) {
	t.Helper()
	drops := make([]string, len(pgmigrate.IdentityColumnNames))
	for i, column := range pgmigrate.IdentityColumnNames {
		drops[i] = "drop column " + column
	}
	if _, err := db.Exec(context.Background(), "alter table "+table+" "+strings.Join(drops, ", ")); err != nil {
		t.Fatalf("take the identity columns off %s: %v", table, err)
	}
}

// RunIdentityChecks holds one store's runner to the rule pgmigrate.Reconcile states, against the
// version table table of each empty database fresh opens, with the runner fresh answers for it.
// Most rows first migrate the original set, 0001_first and 0002_second, and then boot a set that
// adds a pending 0003_third, whose table says whether the runner applied anything. The control
// boots the same files under their own names and texts and is accepted; each other row changes one
// thing about 0002 or about the rows the table holds.
func RunIdentityChecks(t *testing.T, table string, fresh func(t *testing.T) (Database, Migrate)) {
	t.Helper()
	ctx := context.Background()
	original := map[string]string{"0001_first.up.sql": identityFirst, "0002_second.up.sql": identitySecond}
	withThird := func(second, text string) fs.FS {
		return identitySet(map[string]string{"0001_first.up.sql": identityFirst, second: text, "0003_third.up.sql": identityThird})
	}
	// prepared is a fresh database the original set has been applied to, by this runner.
	prepared := func(t *testing.T) (Database, Migrate) {
		t.Helper()
		db, migrate := fresh(t)
		if err := migrate(ctx, identitySet(original)); err != nil {
			t.Fatalf("migrate the original set: %v", err)
		}
		return db, migrate
	}
	want := func(t *testing.T, db Database, rows ...pgmigrate.Recorded) {
		t.Helper()
		got, err := pgmigrate.ReadRecorded(ctx, db, table)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		if !reflect.DeepEqual(got, rows) {
			t.Errorf("%s records %+v, want %+v", table, got, rows)
		}
	}
	applied := func(t *testing.T, db Database) bool {
		t.Helper()
		var exists bool
		if err := db.QueryRow(ctx, "select to_regclass('identity_third') is not null").Scan(&exists); err != nil {
			t.Fatalf("look for identity_third: %v", err)
		}
		return exists
	}
	refused := func(t *testing.T, db Database, err error, wants ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("the runner accepted the boot; want a refusal naming %q", wants)
		}
		t.Logf("refusal: %v", err)
		for _, want := range append(wants, "reset it (drop it, create it empty and let the server migrate it)") {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %q, want it to say %q", err, want)
			}
		}
		if applied(t, db) {
			t.Error("the refused boot applied 0003_third; want nothing applied")
		}
	}
	logs := func(t *testing.T) func(message string) int {
		t.Helper()
		records := &logtest.Recorder{}
		logtest.SetDefault(t, records)
		return func(message string) int {
			n := 0
			for _, record := range records.Records() {
				if record.Message != message {
					continue
				}
				record.Attrs(func(attr slog.Attr) bool {
					if attr.Key == "table" && attr.Value.String() == table {
						n++
					}
					return true
				})
			}
			return n
		}
	}
	// row is version's row as a runner records it: identity, and the text it applied, "" where
	// the row records none.
	row := func(version int, identity pgmigrate.Identity, applied string) pgmigrate.Recorded {
		return pgmigrate.Recorded{Version: version, Identity: identity, Applied: applied}
	}
	first, second := identity("0001_first.up.sql", identityFirst), identity("0002_second.up.sql", identitySecond)
	third := identity("0003_third.up.sql", identityThird)
	edited := identity("0002_second.up.sql", identitySecondEdited)
	declaring := identitySecondEdited + "\n-- replaces: " + second.String() + "\n"
	preparedRows := []pgmigrate.Recorded{row(1, first, first.Checksum), row(2, second, second.Checksum)}

	t.Run("control: the same files under their own names and texts boot clean", func(t *testing.T) {
		db, migrate := prepared(t)
		count := logs(t)
		for boot := range 2 {
			if err := migrate(ctx, withThird("0002_second.up.sql", identitySecond)); err != nil {
				t.Fatalf("boot %d of the same files: %v", boot+1, err)
			}
		}
		if !applied(t, db) {
			t.Error("the boot did not apply 0003_third")
		}
		want(t, db, append(preparedRows, row(3, third, third.Checksum))...)
		if n := count(backfillMessage); n != 0 {
			t.Errorf("the boots logged %d fills; want none", n)
		}
	})

	t.Run("a: a version recorded under another name is refused, naming both", func(t *testing.T) {
		db, migrate := prepared(t)
		renamed := identity("0002_renamed.up.sql", identitySecond)
		refused(t, db, migrate(ctx, withThird("0002_renamed.up.sql", identitySecond)),
			"version 2 is recorded as "+second.String(), "this build's file of version 2 is "+renamed.String(),
			"if the edit is intended, add this line to", "-- replaces: "+second.String())
		want(t, db, preparedRows...)
	})

	t.Run("b: the same name with another checksum is refused, naming both", func(t *testing.T) {
		db, migrate := prepared(t)
		refused(t, db, migrate(ctx, withThird("0002_second.up.sql", identitySecondEdited)),
			"version 2 is recorded as "+second.String(), "this build's file of version 2 is "+edited.String(),
			"if the edit is intended, add this line to", "-- replaces: "+second.String())
		want(t, db, preparedRows...)
	})

	t.Run("c: a row with no name is filled once and never refused, and a second boot changes nothing", func(t *testing.T) {
		db, migrate := prepared(t)
		// A version recorded with no file in the set, as one an image rollback leaves: the runner
		// leaves it as it is, as it always has.
		if _, err := db.Exec(ctx, "insert into "+table+" (version) values (99)"); err != nil {
			t.Fatalf("record a version with no file: %v", err)
		}
		earlierRunner(t, db, table)
		count := logs(t)
		filled := []pgmigrate.Recorded{row(1, first, ""), row(2, second, ""), row(3, third, third.Checksum), {Version: 99, Unnamed: true}}
		for boot := range 2 {
			if err := migrate(ctx, withThird("0002_second.up.sql", identitySecond)); err != nil {
				t.Fatalf("boot %d over rows that record no name: %v", boot+1, err)
			}
			want(t, db, filled...)
		}
		if n := count(backfillMessage); n != 2 {
			t.Errorf("the two boots logged %d fills; want 2, one for each of versions 1 and 2, all from the first boot", n)
		}
	})

	// The row keeps the identity and text it records, so the set it was recorded under, the
	// release before, boots over the database again; a second edit carries a replaces line for the
	// first identity and one for the first edit's text, the chain that covers both.
	t.Run("d1: a file declaring the recorded identity boots and leaves the row as it is, and the set before boots again", func(t *testing.T) {
		for name, file := range map[string]struct{ name, text string }{
			"an edit in place": {"0002_second.up.sql", declaring},
			"a rename":         {"0002_renamed.up.sql", "-- replaces: " + second.String() + "\n" + identitySecond},
		} {
			t.Run(name, func(t *testing.T) {
				db, migrate := prepared(t)
				count := logs(t)
				if err := migrate(ctx, withThird(file.name, file.text)); err != nil {
					t.Fatalf("boot a file declaring the recorded identity: %v", err)
				}
				if !applied(t, db) {
					t.Error("the boot did not apply 0003_third")
				}
				rows := append(preparedRows, row(3, third, third.Checksum))
				want(t, db, rows...)
				if n := count(backfillMessage); n != 0 {
					t.Errorf("the boot logged %d fills; want none: the row is left as it is", n)
				}
				if err := migrate(ctx, withThird("0002_second.up.sql", identitySecond)); err != nil {
					t.Fatalf("the set before the edit, booted again over the database the edit booted: %v", err)
				}
				later := identity(file.name, file.text)
				chained := identitySecondEdited + "-- a second edit\n-- replaces: " + second.String() + "\n-- replaces: " + later.String() + "\n"
				if err := migrate(ctx, withThird("0002_second.up.sql", chained)); err != nil {
					t.Fatalf("a second edit whose replaces lines name the first identity and the first edit: %v", err)
				}
				want(t, db, rows...)
			})
		}
	})

	// A runner fills a row that records no name with the version's first identity and no applied
	// text, since nothing says which text ran, so the release before boots a database the edited
	// set filled. The control records the edited file's own identity instead, as a runner that
	// recorded each file's own did: the set before refuses it.
	t.Run("d2: a database the edited set filled records the first identity, and the set before boots it", func(t *testing.T) {
		db, migrate := prepared(t)
		earlierRunner(t, db, table)
		if err := migrate(ctx, identitySet(map[string]string{"0001_first.up.sql": identityFirst, "0002_second.up.sql": declaring})); err != nil {
			t.Fatalf("boot the edited set over rows that record no name: %v", err)
		}
		want(t, db, row(1, first, ""), row(2, second, ""))
		if err := migrate(ctx, identitySet(original)); err != nil {
			t.Fatalf("the set before the edit, booted over the database the edited set filled: %v", err)
		}
		if _, err := db.Exec(ctx, "update "+table+" set checksum = $1 where version = 2", sum(declaring)); err != nil {
			t.Fatalf("control: record the edited file's own identity: %v", err)
		}
		refused(t, db, migrate(ctx, withThird("0002_second.up.sql", identitySecond)),
			"version 2 is recorded as "+identity("0002_second.up.sql", declaring).String())
	})

	// A database the edited set created records the first identity and the edited text it applied.
	// A build whose file names no such text refuses it: the original file, as the release before an
	// edit is, and another branch's edit under the same first identity. The edited set itself, and
	// a later edit that declares the edited text, boot it.
	t.Run("d3: a database an edit created is refused by a file that names no text of it, the original included", func(t *testing.T) {
		db, migrate := fresh(t)
		if err := migrate(ctx, identitySet(map[string]string{"0001_first.up.sql": identityFirst, "0002_second.up.sql": declaring})); err != nil {
			t.Fatalf("create the database with the edited set: %v", err)
		}
		want(t, db, row(1, first, first.Checksum), row(2, second, sum(declaring)))
		appliedText := "the text applied under it has sha256 " + sum(declaring)
		refused(t, db, migrate(ctx, withThird("0002_second.up.sql", identitySecond)), appliedText, "a later release created this database from empty")
		another := "create table identity_second (id bigint, other jsonb)\n-- replaces: " + second.String() + "\n"
		refused(t, db, migrate(ctx, withThird("0002_second.up.sql", another)), appliedText,
			"add this line to 0002_second.up.sql, after its replaces lines: -- replaces: 0002_second.up.sql sha256:"+sum(declaring))
		if err := migrate(ctx, identitySet(map[string]string{"0001_first.up.sql": identityFirst, "0002_second.up.sql": declaring})); err != nil {
			t.Fatalf("control: the edited set itself: %v", err)
		}
		later := another + "-- replaces: 0002_second.up.sql sha256:" + sum(declaring) + "\n"
		if err := migrate(ctx, withThird("0002_second.up.sql", later)); err != nil {
			t.Fatalf("control: a later edit that declares the edited text: %v", err)
		}
		want(t, db, row(1, first, first.Checksum), row(2, second, sum(declaring)), row(3, third, third.Checksum))
	})

	t.Run("e: a file declaring a different identity is refused", func(t *testing.T) {
		db, migrate := prepared(t)
		other := pgmigrate.Identity{Name: "0002_second.up.sql", Checksum: sum("some other text")}
		text := identitySecondEdited + "\n-- replaces: " + other.String() + "\n"
		refused(t, db, migrate(ctx, withThird("0002_second.up.sql", text)),
			"version 2 is recorded as "+second.String(), "this build's file of version 2 is "+identity("0002_second.up.sql", text).String(),
			"whose replaces lines name only "+other.String())
		want(t, db, preparedRows...)
	})

	// An applied migration edited in place, a comment among it, before this runner first records
	// identities: its null row is filled from the edited file and never refused. The release before
	// that is a runner from before identities, which reads no identity and boots over any row; this
	// runner boots the same set again, and a later edit that keeps the edited text as its first
	// replaces line boots too, as the pre-merge check requires of it.
	t.Run("f1: rows with no name, then an edited file, fill the edited identity and refuse nothing", func(t *testing.T) {
		db, migrate := prepared(t)
		earlierRunner(t, db, table)
		if err := migrate(ctx, withThird("0002_second.up.sql", identitySecondEdited)); err != nil {
			t.Fatalf("boot an edited file over rows that record no name: %v", err)
		}
		rows := []pgmigrate.Recorded{row(1, first, ""), row(2, edited, ""), row(3, third, third.Checksum)}
		want(t, db, rows...)
		if err := migrate(ctx, withThird("0002_second.up.sql", identitySecondEdited)); err != nil {
			t.Fatalf("boot the same set again over the rows it filled: %v", err)
		}
		later := identitySecondEdited + "\n-- a later comment\n-- replaces: " + edited.String() + "\n"
		if err := migrate(ctx, withThird("0002_second.up.sql", later)); err != nil {
			t.Fatalf("a later edit that declares the edited text first: %v", err)
		}
		want(t, db, rows...)
	})

	t.Run("f2: the original files first, then the edited file, is refused without a replaces line and accepted with one", func(t *testing.T) {
		db, migrate := prepared(t)
		refused(t, db, migrate(ctx, withThird("0002_second.up.sql", identitySecondEdited)),
			"version 2 is recorded as "+second.String(), "this build's file of version 2 is "+edited.String())
		if err := migrate(ctx, withThird("0002_second.up.sql", declaring)); err != nil {
			t.Fatalf("boot the edited file with its replaces line: %v", err)
		}
		want(t, db, append(preparedRows, row(3, third, third.Checksum))...)
	})
}

// CheckVersionsOfAnEarlierRunner holds pgmigrate.CheckVersions, through check, to the shape of the
// table an earlier runner leaves after it applied the whole set: migrate, this branch's own
// runner, applies the set to db, and the identity columns are then taken off table, so every row
// records a version and no name, as on a database that main's runner from before identities
// migrated. This simulates that table's shape; it does not run main's runner. The check finds a
// file for every version and a fill for every row and refuses nothing; after one more boot of
// this runner every row records what the runner records of its file. Two controls, each alone,
// show that the check can fail for each reason it exists for: a row recording another file's name
// is refused, and, with that row put back, a version with no file in the set is named.
func CheckVersionsOfAnEarlierRunner(t *testing.T, table string, db Database, migrate func(context.Context) error, check func(context.Context) (*pgmigrate.VersionCheck, error), files int) {
	t.Helper()
	ctx := context.Background()
	if err := migrate(ctx); err != nil {
		t.Fatalf("migrate the shipped set: %v", err)
	}
	earlierRunner(t, db, table)
	read := func(stage string) *pgmigrate.VersionCheck {
		t.Helper()
		result, err := check(ctx)
		if err != nil {
			t.Fatalf("%s: check the versions: %v", stage, err)
		}
		var report strings.Builder
		result.Write(&report)
		t.Logf("%s:\n%s", stage, report.String())
		return result
	}
	before := read("as an earlier runner left it")
	if !before.OK() || before.Recorded != files || len(before.Backfills) != files || before.Matching != 0 || len(before.Pending) != 0 {
		t.Errorf("an earlier runner's table: ok %v, %d recorded, %d fills, %d matching, pending %v; want ok, every one of the %d files recorded and filled, none matching yet, none pending",
			before.OK(), before.Recorded, len(before.Backfills), before.Matching, before.Pending, files)
	}
	if err := migrate(ctx); err != nil {
		t.Fatalf("boot this runner over the earlier runner's table: %v", err)
	}
	after := read("after this runner's boot")
	if !after.OK() || after.Recorded != files || after.Matching != files || len(after.Backfills) != 0 {
		t.Errorf("after the boot: ok %v, %d recorded, %d matching, %d fills; want ok and every one of the %d files matching",
			after.OK(), after.Recorded, after.Matching, len(after.Backfills), files)
	}
	var first int
	var name string
	if err := db.QueryRow(ctx, "select version, name from "+table+" order by version limit 1").Scan(&first, &name); err != nil {
		t.Fatalf("control: read the first row: %v", err)
	}
	if _, err := db.Exec(ctx, "update "+table+" set name = 'renamed.up.sql' where version = $1", first); err != nil {
		t.Fatalf("control: record another name: %v", err)
	}
	refused := read("control: a refused row alone")
	if refused.OK() || len(refused.Refusals) != 1 || !strings.Contains(refused.Refusals[0].Error(), "renamed.up.sql") || len(refused.NoFile) != 0 {
		t.Errorf("control, a refused row alone: ok %v, refusals %v, no file %v; want not ok, the renamed row refused and no version without a file",
			refused.OK(), refused.Refusals, refused.NoFile)
	}
	if _, err := db.Exec(ctx, "update "+table+" set name = $2 where version = $1", first, name); err != nil {
		t.Fatalf("control: put the name back: %v", err)
	}
	if _, err := db.Exec(ctx, "insert into "+table+" (version) values (2147483000)"); err != nil {
		t.Fatalf("control: record a version with no file: %v", err)
	}
	noFile := read("control: a version with no file alone")
	if noFile.OK() || len(noFile.Refusals) != 0 || len(noFile.NoFile) != 1 || noFile.NoFile[0] != 2147483000 {
		t.Errorf("control, a version with no file alone: ok %v, refusals %v, no file %v; want not ok, no refusal, and version 2147483000 named as having no file",
			noFile.OK(), noFile.Refusals, noFile.NoFile)
	}
}
