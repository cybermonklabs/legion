package pgmigrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg"
)

// sha256Hex is the checksum a runner records of text, computed apart from Checksum so the test
// does not read the code under test back to itself.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Load gives each file its name and the sha256 of its whole text, and reads every replaces line,
// however many there are, as Postgres reads a comment. A literal that holds the same words in the
// middle of a line is the literal's text and declares nothing. The control is a file with no
// replaces line, which declares none.
func TestLoadReadsEachFilesIdentityAndItsReplacesLines(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	text := "-- replaces: 0002_second.up.sql sha256:" + a + "\n" +
		"create table second (id integer);\n" +
		"-- replaces: 0002_old_name.up.sql sha256:" + b + "\n" +
		"select 'see -- replaces: 0002_in_a_literal.up.sql sha256:" + a + "';\n"
	set := fstest.MapFS{
		"migrations/0001_first.up.sql":  file("create table first (id integer)"),
		"migrations/0002_second.up.sql": file(text),
	}
	got, err := Load(set, "migrations")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got[0].Checksum != sha256Hex("create table first (id integer)") || got[0].Replaces != nil {
		t.Errorf("0001 = checksum %s, replaces %v; want the sha256 of its text and no replaces line (control)", got[0].Checksum, got[0].Replaces)
	}
	if got[1].Checksum != sha256Hex(text) {
		t.Errorf("0002's checksum = %s, want %s, the sha256 of its whole text", got[1].Checksum, sha256Hex(text))
	}
	want := []Identity{{Name: "0002_second.up.sql", Checksum: a}, {Name: "0002_old_name.up.sql", Checksum: b}}
	if !reflect.DeepEqual(got[1].Replaces, want) {
		t.Errorf("0002's replaces = %+v, want %+v: the two comment lines, not the literal's text", got[1].Replaces, want)
	}
	if id := got[1].Identity(); id.Name != "0002_second.up.sql" || id.Checksum != sha256Hex(text) {
		t.Errorf("Identity() = %+v", id)
	}
}

// A file with CRLF line endings declares its replaces lines as one with LF endings does: the
// carriage return ends the line and is not part of the checksum's hex. The control is a line with
// a stray character after the checksum, which is still refused.
func TestLoadReadsAReplacesLineOfAFileWithCRLFLineEndings(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 64)
	text := "create table second (id integer);\r\n-- replaces: 0002_second.up.sql sha256:" + a + "\r\nselect 1;\r\n"
	got, err := Load(fstest.MapFS{"migrations/0002_second.up.sql": file(text)}, "migrations")
	if err != nil {
		t.Fatalf("Load refused a CRLF file: %v", err)
	}
	if want := []Identity{{Name: "0002_second.up.sql", Checksum: a}}; !reflect.DeepEqual(got[0].Replaces, want) {
		t.Errorf("replaces = %+v, want %+v", got[0].Replaces, want)
	}
	stray := strings.Replace(text, a+"\r\n", a+"x\r\n", 1)
	if _, err := Load(fstest.MapFS{"migrations/0002_second.up.sql": file(stray)}, "migrations"); err == nil || !strings.Contains(err.Error(), "is not a replaces line") {
		t.Errorf("control: Load = %v; want the line with a stray character refused", err)
	}
}

// A line that reads as a replaces line but that Postgres reads as part of a literal is refused at
// load, naming the file and the line: inside a string, inside a dollar-quoted body, and between
// the parts of a continued string, whose comment Postgres reads inside the string's token. Read
// as declaring nothing, it would neither save the database its author meant it to nor be refused
// when malformed. The control is the same line on a line of its own outside the literal, which
// loads and declares the identity.
func TestLoadRefusesAReplacesLinePostgresReadsAsPartOfALiteral(t *testing.T) {
	t.Parallel()
	line := "-- replaces: 0002_second.up.sql sha256:" + strings.Repeat("0", 64)
	for name, text := range map[string]string{
		"inside a string":             "select '\n" + line + "\n';\n",
		"inside a dollar-quoted body": "create function f() returns int language sql as $$\n" + line + "\nselect 1 $$;\n",
		"inside a continued string":   "select 'a'\n" + line + "\n'b';\n",
		"indented inside a string":    "select '\n  " + line + "\n';\n",
	} {
		t.Run(name, func(t *testing.T) {
			migrations, err := Load(fstest.MapFS{"migrations/0002_second.up.sql": file(text)}, "migrations")
			if err == nil {
				t.Fatalf("Load accepted %q: %+v", text, migrations)
			}
			if !strings.Contains(err.Error(), "migration 0002_second.up.sql line 2 reads as a replaces line, but Postgres reads it as part of") {
				t.Errorf("Load = %v, want it to name the file, line 2 and the literal", err)
			}
		})
	}
	t.Run("control: the same line outside the literal", func(t *testing.T) {
		got, err := Load(fstest.MapFS{"migrations/0002_second.up.sql": file("select 'a'\n'b';\n" + line + "\n")}, "migrations")
		if err != nil {
			t.Fatalf("Load refused the line outside the literal: %v", err)
		}
		if len(got[0].Replaces) != 1 {
			t.Errorf("replaces = %+v, want the one line", got[0].Replaces)
		}
	})
}

// A comment that begins with replaces:, in any case, is a replaces line, and one not written in
// the form a runner can match refuses the whole set at load, naming the file, its line and the
// form: a runner that skipped it would refuse a database the author meant the line to save, or
// read a wrong checksum as a promise. A line naming a file of another version is refused too,
// since a runner judges the row of a version against the file of that version alone. The control
// is the same file with the well-formed line, which loads.
func TestLoadRefusesAMalformedReplacesLine(t *testing.T) {
	t.Parallel()
	good := "-- replaces: 0002_second.up.sql sha256:" + strings.Repeat("0", 64)
	for name, line := range map[string]string{
		"upper-case hex":               "-- replaces: 0002_second.up.sql sha256:" + strings.Repeat("A", 64),
		"a short checksum":             "-- replaces: 0002_second.up.sql sha256:" + strings.Repeat("0", 63),
		"no sha256: prefix":            "-- replaces: 0002_second.up.sql " + strings.Repeat("0", 64),
		"no name":                      "-- replaces: sha256:" + strings.Repeat("0", 64),
		"a name with a directory":      "-- replaces: migrations/0002_second.up.sql sha256:" + strings.Repeat("0", 64),
		"a capital marker":             "-- Replaces: 0002_second.up.sql sha256:" + strings.Repeat("0", 64),
		"no space after the dashes":    "--replaces: 0002_second.up.sql sha256:" + strings.Repeat("0", 64),
		"trailing words":               good + " because",
		"after a statement":            "select 1; " + good,
		"a file of another version":    "-- replaces: 0003_third.up.sql sha256:" + strings.Repeat("0", 64),
		"a name that is not a .up.sql": "-- replaces: 0002_second.down.sql sha256:" + strings.Repeat("0", 64),
	} {
		t.Run(name, func(t *testing.T) {
			set := fstest.MapFS{"migrations/0002_second.up.sql": file("create table second (id integer);\n" + line + "\n")}
			migrations, err := Load(set, "migrations")
			if err == nil {
				t.Fatalf("Load accepted %q: %+v", line, migrations)
			}
			if !strings.Contains(err.Error(), "migration 0002_second.up.sql line 2") {
				t.Errorf("Load = %v, want it to name the file and line 2", err)
			}
		})
	}
	t.Run("control: the well-formed line", func(t *testing.T) {
		set := fstest.MapFS{"migrations/0002_second.up.sql": file("create table second (id integer);\n" + good + "\n")}
		if _, err := Load(set, "migrations"); err != nil {
			t.Fatalf("Load refused the well-formed line: %v", err)
		}
	})
}

// Reconcile's rule, row by row, without a database; the stores' tests hold each runner to it
// against one (pgmigratetest.RunIdentityChecks).
func TestReconcileJudgesEachRecordedRowAgainstItsFile(t *testing.T) {
	t.Parallel()
	own := Identity{Name: "0002_second.up.sql", Checksum: strings.Repeat("1", 64)}
	earlier := Identity{Name: "0002_second.up.sql", Checksum: strings.Repeat("2", 64)}
	other := Identity{Name: "0002_other.up.sql", Checksum: strings.Repeat("1", 64)}
	another := strings.Repeat("4", 64)
	set := []Migration{{Version: 2, Name: own.Name, Checksum: own.Checksum, Replaces: []Identity{earlier}}}
	original := []Migration{{Version: 2, Name: earlier.Name, Checksum: earlier.Checksum}}
	for name, tc := range map[string]struct {
		set     []Migration
		row     Recorded
		fixes   []Fix
		refused []string
	}{
		"its own identity": {row: Recorded{Version: 2, Identity: own}},
		"a null name, filled with the first identity the file declares": {row: Recorded{Version: 2, Unnamed: true}, fixes: []Fix{{Version: 2, To: earlier}}},
		"an empty name that is not null":                                {row: Recorded{Version: 2, Identity: Identity{Checksum: own.Checksum}}, refused: []string{`version 2 is recorded as "" sha256 "` + own.Checksum + `"`, "no replaces line can name what this row records, so a reset is the one remedy"}},
		"a name and a null checksum":                                    {row: Recorded{Version: 2, Identity: Identity{Name: own.Name}}, refused: []string{`version 2 is recorded as "0002_second.up.sql" sha256 ""`, "no replaces line can name what this row records, so a reset is the one remedy"}},
		"a replaced identity":                                           {row: Recorded{Version: 2, Identity: earlier}},
		"a version with no file":                                        {row: Recorded{Version: 7, Identity: other}},
		"another name":                                                  {row: Recorded{Version: 2, Identity: other}, refused: []string{"version 2 is recorded as " + other.String(), "file of version 2 is " + own.String(), "after the replaces lines it has", "-- replaces: " + other.String()}},
		"the replaced name, other text":                                 {row: Recorded{Version: 2, Identity: Identity{Name: own.Name, Checksum: strings.Repeat("3", 64)}}, refused: []string{"whose replaces lines name only " + earlier.String()}},
		"the first identity, applied from the file's own text":          {row: Recorded{Version: 2, Identity: earlier, Applied: own.Checksum}},
		"the first identity, applied from a text a replaces line names": {row: Recorded{Version: 2, Identity: earlier, Applied: earlier.Checksum}},
		"the first identity, applied from a text the file does not name": {row: Recorded{Version: 2, Identity: earlier, Applied: another},
			refused: []string{"the text applied under it has sha256 " + another + ", which neither that file nor its replaces lines name", "a later release created this database from empty", "add this line to 0002_second.up.sql, after its replaces lines: -- replaces: 0002_second.up.sql sha256:" + another}},
		"an original file, applied from a later edit's text": {set: original, row: Recorded{Version: 2, Identity: earlier, Applied: own.Checksum},
			refused: []string{"the text applied under it has sha256 " + own.Checksum, "add these two lines to 0002_second.up.sql, in this order: -- replaces: " + earlier.String() + " and -- replaces: 0002_second.up.sql sha256:" + own.Checksum}},
		"an original file, applied from its own text": {set: original, row: Recorded{Version: 2, Identity: earlier, Applied: earlier.Checksum}},
		"a null applied text, as a filled row has":    {set: original, row: Recorded{Version: 2, Identity: earlier}},
	} {
		t.Run(name, func(t *testing.T) {
			set := set
			if tc.set != nil {
				set = tc.set
			}
			fixes, err := Reconcile(set, []Recorded{tc.row})
			if len(tc.refused) == 0 {
				if err != nil || !reflect.DeepEqual(fixes, tc.fixes) {
					t.Errorf("Reconcile = %+v, %v; want %+v and no refusal", fixes, err, tc.fixes)
				}
				return
			}
			if err == nil || fixes != nil {
				t.Fatalf("Reconcile = %+v, %v; want a refusal and no fix", fixes, err)
			}
			for _, want := range tc.refused {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to say %q", err, want)
				}
			}
			if !IsRefusal(err) {
				t.Errorf("IsRefusal(%v) = false; a refusal must be told from a failed read or write", err)
			}
			// Every line a refusal offers is one Load accepts in the file it names.
			if offered := offeredLines.FindAllString(err.Error(), -1); len(offered) > 0 {
				set := fstest.MapFS{"migrations/" + own.Name: file("select 1;\n" + strings.Join(offered, "\n") + "\n")}
				if _, err := Load(set, "migrations"); err != nil {
					t.Errorf("the refusal offers %q, which Load refuses: %v", offered, err)
				}
			}
		})
	}
	// One refused row refuses every fix the other rows needed: nothing is written.
	fixes, err := Reconcile(set, []Recorded{{Version: 2, Unnamed: true}, {Version: 2, Identity: other}})
	if err == nil || fixes != nil {
		t.Errorf("Reconcile of a row to fill beside a refused one = %+v, %v; want the refusal and no fix", fixes, err)
	}
}

// offeredLines finds each replaces line a refusal offers.
var offeredLines = regexp.MustCompile(`-- replaces: \S+\.up\.sql sha256:[0-9a-f]{64}`)

// Two branches edit the same shipped migration, each declaring its first identity as CheckEdits
// requires, so a runner records the same identity of either. A database branch X's runner
// migrated records X's text as applied, so branch Y's file, which names no text of X's, refuses
// it, and so does the original file. The controls: X's own file, and a later edit of X that
// declares X's text, accept the same row.
func TestReconcileRefusesAnotherEditsTextUnderTheSameFirstIdentity(t *testing.T) {
	t.Parallel()
	const original = "create table a (id int)"
	first := Identity{Name: "0002_a.up.sql", Checksum: sha256Hex(original)}
	line := "\n-- replaces: " + first.String() + "\n"
	x := "create table a (id int, x text)" + line
	y := "create table a (id bigint, y jsonb)" + line
	load := func(t *testing.T, text string) []Migration {
		t.Helper()
		set, err := Load(fstest.MapFS{"migrations/0002_a.up.sql": file(text)}, "migrations")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set
	}
	xSet := load(t, x)
	if xSet[0].RecordedIdentity() != load(t, y)[0].RecordedIdentity() {
		t.Fatal("the two edits record different identities; the case needs the same one")
	}
	row := Recorded{Version: 2, Identity: xSet[0].RecordedIdentity(), Applied: xSet[0].Checksum}
	for name, tc := range map[string]struct {
		text    string
		refused bool
	}{
		"branch Y's edit":              {text: y, refused: true},
		"the original file":            {text: original, refused: true},
		"control: branch X's own file": {text: x},
		"control: a later edit of X's": {text: "create table a (id int, x text, z text)" + line + "-- replaces: 0002_a.up.sql sha256:" + sha256Hex(x) + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			fixes, err := Reconcile(load(t, tc.text), []Recorded{row})
			if !tc.refused {
				if err != nil || len(fixes) != 0 {
					t.Errorf("Reconcile = %+v, %v; want the row accepted as it is", fixes, err)
				}
				return
			}
			if err == nil || !IsRefusal(err) || !strings.Contains(err.Error(), "the text applied under it has sha256 "+sha256Hex(x)) {
				t.Errorf("Reconcile = %+v, %v; want a refusal naming branch X's applied text", fixes, err)
			}
		})
	}
}

// CheckVersions says "not OK" for each fault alone, and for a check that compared nothing, and
// refuses a connection whose search_path names no schema. Each row builds one database: the
// control, a version table recording both files under their own identities, reads OK; then one
// fault each, which must turn the verdict by itself. An absent table and an empty one each read as
// nothing checked, not as a clean audit, so a check pointed at the wrong database cannot pass.
func TestCheckVersionsSaysNotOKForEachFaultAloneAndForNothingChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	set := []Migration{
		{Version: 1, Name: "0001_a.up.sql", Checksum: sha256Hex("a")},
		{Version: 2, Name: "0002_b.up.sql", Checksum: sha256Hex("b")},
		{Version: 3, Name: "0003_c.up.sql", Checksum: sha256Hex("c2"), Replaces: []Identity{{Name: "0003_c.up.sql", Checksum: sha256Hex("c")}}},
	}
	const table = "versions_check"
	create := "create table " + table + " (version bigint primary key, applied_at timestamptz not null default now(), " + IdentityColumns + ")"
	// As a runner records the set: the third file's first replaces line, and each text applied.
	both := "insert into " + table + " (version, name, checksum, applied_checksum) values (1, '0001_a.up.sql', '" + sha256Hex("a") + "', '" + sha256Hex("a") + "'), (2, '0002_b.up.sql', '" + sha256Hex("b") + "', '" + sha256Hex("b") + "'), (3, '0003_c.up.sql', '" + sha256Hex("c") + "', '" + sha256Hex("c2") + "')"
	for name, tc := range map[string]struct {
		setup      []string
		ok         bool
		refusals   int
		noFile     int
		nothing    bool
		matching   int
		reportLine string
	}{
		"control: every row as this build's runner records it": {setup: []string{create, both}, ok: true, matching: 3, reportLine: "versions: ok"},
		"a refused row alone": {setup: []string{create, both, "update " + table + " set name = '0002_other.up.sql' where version = 2"},
			refusals: 1, matching: 2, reportLine: "versions: REFUSED version 2 is recorded as 0002_other.up.sql"},
		"a text the file does not name alone": {setup: []string{create, both, "update " + table + " set applied_checksum = '" + sha256Hex("other") + "' where version = 2"},
			refusals: 1, matching: 2, reportLine: "versions: REFUSED version 2 is recorded as 0002_b.up.sql sha256:" + sha256Hex("b") + ", which this build's file of version 2"},
		"a version with no file alone": {setup: []string{create, both, "insert into " + table + " (version) values (4)"},
			noFile: 1, matching: 3, reportLine: "versions: NO FILE version 4"},
		"an empty table":  {setup: []string{create}, nothing: true, reportLine: "versions: NOTHING CHECKED: " + table + " records no version"},
		"an absent table": {nothing: true, reportLine: "versions: NOTHING CHECKED: " + table + " records no version"},
	} {
		t.Run(name, func(t *testing.T) {
			conn := connect(t, ctx, testpg.URL(t))
			for _, statement := range tc.setup {
				if _, err := conn.Exec(ctx, statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			check, err := CheckVersions(ctx, conn, set, table)
			if err != nil {
				t.Fatalf("CheckVersions: %v", err)
			}
			var report strings.Builder
			check.Write(&report)
			t.Logf("report:\n%s", report.String())
			if check.OK() != tc.ok || len(check.Refusals) != tc.refusals || len(check.NoFile) != tc.noFile || (check.Recorded == 0) != tc.nothing || check.Matching != tc.matching {
				t.Errorf("ok %v, refusals %d, no file %v, recorded %d, matching %d; want ok %v, refusals %d, no file %d, nothing recorded %v, matching %d",
					check.OK(), len(check.Refusals), check.NoFile, check.Recorded, check.Matching, tc.ok, tc.refusals, tc.noFile, tc.nothing, tc.matching)
			}
			if !strings.Contains(report.String(), tc.reportLine) {
				t.Errorf("report = %q, want it to say %q", report.String(), tc.reportLine)
			}
			if wantVerdict := map[bool]string{true: "versions: ok\n", false: "versions: NOT OK\n"}[tc.ok]; !strings.HasSuffix(report.String(), wantVerdict) {
				t.Errorf("report ends %q, want %q", report.String(), wantVerdict)
			}
		})
	}
	t.Run("a search_path that names no schema", func(t *testing.T) {
		url := testpg.URL(t)
		config, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		config.RuntimeParams["search_path"] = "no_such_schema_4520"
		conn, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		check, err := CheckVersions(ctx, conn, set, table)
		if err == nil || !strings.Contains(err.Error(), "current_schema() is null") {
			t.Fatalf("CheckVersions = %+v, %v; want a refusal naming the null current_schema()", check, err)
		}
	})
}
