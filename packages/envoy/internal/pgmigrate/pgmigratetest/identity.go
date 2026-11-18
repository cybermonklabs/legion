package pgmigratetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// IdentityStore is what RunIdentityChecks drives of one store: its runner, over a set of the
// test's own, and the database the runner records its versions in.
type IdentityStore interface {
	// Migrate runs the store's runner over fsys's migrations directory.
	Migrate(ctx context.Context, fsys fs.FS) error
	pgmigrate.Querier
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// The files of RunIdentityChecks's sets. secondEdited is second as an edit in place leaves it: the
// same name, other text. Neither is applied again once its version is recorded, so the edit only
// changes the file's checksum.
const (
	identityFirst        = "create table identity_first (id integer)"
	identitySecond       = "create table identity_second (id integer)"
	identitySecondEdited = "create table identity_second (id integer, note text)"
	identityThird        = "create table identity_third (id integer)"
)

// The log messages of pgmigrate.LogFixes, which RunIdentityChecks counts.
const (
	backfillMessage = "pgmigrate: recorded the identity of a migration applied before the runners recorded identities"
	moveMessage     = "pgmigrate: moved a migration's recorded identity to the file that replaces it"
)

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
func sum(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// identity is the identity a runner records of the file name holding text.
func identity(name, text string) pgmigrate.Identity {
	return pgmigrate.Identity{Name: name, Checksum: sum(text)}
}

// RunIdentityChecks holds one store's runner to the rule pgmigrate.Reconcile states, against the
// version table table of each empty database fresh opens. Every row first migrates the original
// set, 0001_first and 0002_second, and then boots a set that adds a pending 0003_third, whose table
// says whether the runner applied anything. The control boots the same files under their own names
// and texts and is accepted; each other row changes one thing about 0002 or about the rows the
// table holds.
func RunIdentityChecks(t *testing.T, table string, fresh func(t *testing.T) IdentityStore) {
	t.Helper()
	ctx := context.Background()
	original := map[string]string{"0001_first.up.sql": identityFirst, "0002_second.up.sql": identitySecond}
	withThird := func(second, text string) fs.FS {
		return identitySet(map[string]string{"0001_first.up.sql": identityFirst, second: text, "0003_third.up.sql": identityThird})
	}
	// prepared is a fresh database the original set has been applied to, by this runner.
	prepared := func(t *testing.T) IdentityStore {
		t.Helper()
		s := fresh(t)
		if err := s.Migrate(ctx, identitySet(original)); err != nil {
			t.Fatalf("migrate the original set: %v", err)
		}
		return s
	}
	// earlierRunner leaves the table as a runner from before identities left it: no name and no
	// checksum on any row, every row's version and applied_at as they were.
	earlierRunner := func(t *testing.T, s IdentityStore) {
		t.Helper()
		if _, err := s.Exec(ctx, "alter table "+table+" drop column name, drop column checksum"); err != nil {
			t.Fatalf("take the identity columns off %s: %v", table, err)
		}
	}
	recorded := func(t *testing.T, s IdentityStore) []pgmigrate.Recorded {
		t.Helper()
		rows, err := pgmigrate.ReadRecorded(ctx, s, table)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		return rows
	}
	want := func(t *testing.T, s IdentityStore, rows ...pgmigrate.Recorded) {
		t.Helper()
		if got := recorded(t, s); !reflect.DeepEqual(got, rows) {
			t.Errorf("%s records %+v, want %+v", table, got, rows)
		}
	}
	applied := func(t *testing.T, s IdentityStore) bool {
		t.Helper()
		var exists bool
		if err := s.QueryRow(ctx, "select to_regclass('identity_third') is not null").Scan(&exists); err != nil {
			t.Fatalf("look for identity_third: %v", err)
		}
		return exists
	}
	refused := func(t *testing.T, s IdentityStore, err error, wants ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("the runner accepted the boot; want a refusal naming %q", wants)
		}
		t.Logf("refusal: %v", err)
		for _, want := range append(wants,
			"reset it (drop it, create it empty and let the server migrate it)",
			"if the edit is intended, add this line to") {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %q, want it to say %q", err, want)
			}
		}
		if applied(t, s) {
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
	first, second := identity("0001_first.up.sql", identityFirst), identity("0002_second.up.sql", identitySecond)
	third := identity("0003_third.up.sql", identityThird)
	edited := identity("0002_second.up.sql", identitySecondEdited)

	t.Run("control: the same files under their own names and texts boot clean", func(t *testing.T) {
		s := prepared(t)
		count := logs(t)
		for boot := range 2 {
			if err := s.Migrate(ctx, withThird("0002_second.up.sql", identitySecond)); err != nil {
				t.Fatalf("boot %d of the same files: %v", boot+1, err)
			}
		}
		if !applied(t, s) {
			t.Error("the boot did not apply 0003_third")
		}
		want(t, s, pgmigrate.Recorded{Version: 1, Identity: first}, pgmigrate.Recorded{Version: 2, Identity: second}, pgmigrate.Recorded{Version: 3, Identity: third})
		if n := count(backfillMessage) + count(moveMessage); n != 0 {
			t.Errorf("the boots logged %d fills or moves; want none", n)
		}
	})

	t.Run("a: a version recorded under another name is refused, naming both", func(t *testing.T) {
		s := prepared(t)
		renamed := identity("0002_renamed.up.sql", identitySecond)
		refused(t, s, s.Migrate(ctx, withThird("0002_renamed.up.sql", identitySecond)),
			"version 2 is recorded as "+second.String(), "this build's file of version 2 is "+renamed.String(),
			"-- replaces: "+second.String())
		want(t, s, pgmigrate.Recorded{Version: 1, Identity: first}, pgmigrate.Recorded{Version: 2, Identity: second})
	})

	t.Run("b: the same name with another checksum is refused, naming both", func(t *testing.T) {
		s := prepared(t)
		refused(t, s, s.Migrate(ctx, withThird("0002_second.up.sql", identitySecondEdited)),
			"version 2 is recorded as "+second.String(), "this build's file of version 2 is "+edited.String(),
			"-- replaces: "+second.String())
		want(t, s, pgmigrate.Recorded{Version: 1, Identity: first}, pgmigrate.Recorded{Version: 2, Identity: second})
	})

	t.Run("c: a row with no name is filled once and never refused, and a second boot changes nothing", func(t *testing.T) {
		s := prepared(t)
		// A version recorded with no file in the set, as one an image rollback leaves: the runner
		// leaves it as it is, as it always has.
		if _, err := s.Exec(ctx, "insert into "+table+" (version) values (99)"); err != nil {
			t.Fatalf("record a version with no file: %v", err)
		}
		earlierRunner(t, s)
		count := logs(t)
		if err := s.Migrate(ctx, withThird("0002_second.up.sql", identitySecond)); err != nil {
			t.Fatalf("boot over rows that record no name: %v", err)
		}
		want(t, s, pgmigrate.Recorded{Version: 1, Identity: first}, pgmigrate.Recorded{Version: 2, Identity: second},
			pgmigrate.Recorded{Version: 3, Identity: third}, pgmigrate.Recorded{Version: 99, Unnamed: true})
		if n := count(backfillMessage); n != 2 {
			t.Errorf("the boot logged %d fills; want 2, one for each of versions 1 and 2", n)
		}
		if err := s.Migrate(ctx, withThird("0002_second.up.sql", identitySecond)); err != nil {
			t.Fatalf("second boot: %v", err)
		}
		want(t, s, pgmigrate.Recorded{Version: 1, Identity: first}, pgmigrate.Recorded{Version: 2, Identity: second},
			pgmigrate.Recorded{Version: 3, Identity: third}, pgmigrate.Recorded{Version: 99, Unnamed: true})
		if n := count(backfillMessage); n != 2 {
			t.Errorf("after the second boot the fills logged are %d; want the first boot's 2 and no more", n)
		}
	})

	t.Run("d: a file declaring the recorded identity boots, and the row moves to the file's own identity", func(t *testing.T) {
		for name, file := range map[string]struct{ name, text string }{
			"an edit in place": {"0002_second.up.sql", identitySecondEdited + "\n-- replaces: " + second.String() + "\n"},
			"a rename":         {"0002_renamed.up.sql", "-- replaces: " + second.String() + "\n" + identitySecond},
		} {
			t.Run(name, func(t *testing.T) {
				s := prepared(t)
				count := logs(t)
				if err := s.Migrate(ctx, withThird(file.name, file.text)); err != nil {
					t.Fatalf("boot a file declaring the recorded identity: %v", err)
				}
				if !applied(t, s) {
					t.Error("the boot did not apply 0003_third")
				}
				want(t, s, pgmigrate.Recorded{Version: 1, Identity: first},
					pgmigrate.Recorded{Version: 2, Identity: identity(file.name, file.text)}, pgmigrate.Recorded{Version: 3, Identity: third})
				if n := count(moveMessage); n != 1 {
					t.Errorf("the boot logged %d moves; want 1", n)
				}
			})
		}
	})

	t.Run("e: a file declaring a different identity is refused", func(t *testing.T) {
		s := prepared(t)
		other := pgmigrate.Identity{Name: "0002_second.up.sql", Checksum: sum("some other text")}
		text := identitySecondEdited + "\n-- replaces: " + other.String() + "\n"
		refused(t, s, s.Migrate(ctx, withThird("0002_second.up.sql", text)),
			"version 2 is recorded as "+second.String(), "this build's file of version 2 is "+identity("0002_second.up.sql", text).String(),
			"whose replaces lines name only "+other.String())
		want(t, s, pgmigrate.Recorded{Version: 1, Identity: first}, pgmigrate.Recorded{Version: 2, Identity: second})
	})

	t.Run("f: rows with no name, then an edited file, fill the edited identity and refuse nothing", func(t *testing.T) {
		s := prepared(t)
		earlierRunner(t, s)
		if err := s.Migrate(ctx, withThird("0002_second.up.sql", identitySecondEdited)); err != nil {
			t.Fatalf("boot an edited file over rows that record no name: %v", err)
		}
		want(t, s, pgmigrate.Recorded{Version: 1, Identity: first}, pgmigrate.Recorded{Version: 2, Identity: edited}, pgmigrate.Recorded{Version: 3, Identity: third})
	})

	t.Run("f: the original files first, then the edited file, is refused without a replaces line and accepted with one", func(t *testing.T) {
		s := prepared(t)
		refused(t, s, s.Migrate(ctx, withThird("0002_second.up.sql", identitySecondEdited)),
			"version 2 is recorded as "+second.String(), "this build's file of version 2 is "+edited.String())
		text := identitySecondEdited + "\n-- replaces: " + second.String() + "\n"
		if err := s.Migrate(ctx, withThird("0002_second.up.sql", text)); err != nil {
			t.Fatalf("boot the edited file with its replaces line: %v", err)
		}
		want(t, s, pgmigrate.Recorded{Version: 1, Identity: first},
			pgmigrate.Recorded{Version: 2, Identity: identity("0002_second.up.sql", text)}, pgmigrate.Recorded{Version: 3, Identity: third})
	})
}

// CheckVersionsOfAnEarlierRunner holds pgmigrate.CheckVersions, through check, to the shape of the
// table an earlier runner leaves after it applied the whole set: migrate, this branch's own
// runner, applies the set, and the identity columns are then taken off table, so every row
// records a version and no name, as on a database that main's runner from before identities
// migrated. This simulates that table's shape; it does not run main's runner. The check finds a
// file for every version and a fill for every row and refuses nothing; after one more boot of
// this runner every row records its file's own identity. Two controls, each alone, show that the
// check can fail for each reason it exists for: a row recording another file's name is refused,
// and, with that row put back, a version with no file in the set is named.
func CheckVersionsOfAnEarlierRunner(t *testing.T, table string, s IdentityStore, migrate func(context.Context) error, check func(context.Context) (*pgmigrate.VersionCheck, error), files int) {
	t.Helper()
	ctx := context.Background()
	if err := migrate(ctx); err != nil {
		t.Fatalf("migrate the shipped set: %v", err)
	}
	if _, err := s.Exec(ctx, "alter table "+table+" drop column name, drop column checksum"); err != nil {
		t.Fatalf("take the identity columns off %s: %v", table, err)
	}
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
	if err := s.QueryRow(ctx, "select version, name from "+table+" order by version limit 1").Scan(&first, &name); err != nil {
		t.Fatalf("control: read the first row: %v", err)
	}
	if _, err := s.Exec(ctx, "update "+table+" set name = 'renamed.up.sql' where version = $1", first); err != nil {
		t.Fatalf("control: record another name: %v", err)
	}
	refused := read("control: a refused row alone")
	if refused.OK() || len(refused.Refusals) != 1 || !strings.Contains(refused.Refusals[0].Error(), "renamed.up.sql") || len(refused.NoFile) != 0 {
		t.Errorf("control, a refused row alone: ok %v, refusals %v, no file %v; want not ok, the renamed row refused and no version without a file",
			refused.OK(), refused.Refusals, refused.NoFile)
	}
	if _, err := s.Exec(ctx, "update "+table+" set name = $2 where version = $1", first, name); err != nil {
		t.Fatalf("control: put the name back: %v", err)
	}
	if _, err := s.Exec(ctx, "insert into "+table+" (version) values (2147483000)"); err != nil {
		t.Fatalf("control: record a version with no file: %v", err)
	}
	noFile := read("control: a version with no file alone")
	if noFile.OK() || len(noFile.Refusals) != 0 || len(noFile.NoFile) != 1 || noFile.NoFile[0] != 2147483000 {
		t.Errorf("control, a version with no file alone: ok %v, refusals %v, no file %v; want not ok, no refusal, and version 2147483000 named as having no file",
			noFile.OK(), noFile.Refusals, noFile.NoFile)
	}
}
