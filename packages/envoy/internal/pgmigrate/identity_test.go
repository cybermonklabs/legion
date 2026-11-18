package pgmigrate

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// sha256Hex is the checksum a runner records of text, computed apart from checksumOf so the test
// does not read the code under test back to itself.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Load gives each file its name and the sha256 of its whole text, and reads every replaces line,
// however many there are, as Postgres reads a comment: a line of the same form inside a string
// literal or a dollar-quoted body is the literal's text, and declares nothing. The control is a
// file with no replaces line, which declares none.
func TestLoadReadsEachFilesIdentityAndItsReplacesLines(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	text := "-- replaces: 0002_second.up.sql sha256:" + a + "\n" +
		"create table second (id integer);\n" +
		"-- replaces: 0002_old_name.up.sql sha256:" + b + "\n" +
		"select '\n-- replaces: 0002_in_a_literal.up.sql sha256:" + a + "\n';\n" +
		"create function f() returns int language sql as $$\n-- replaces: 0002_in_a_body.up.sql sha256:" + b + "\nselect 1 $$;\n"
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
		t.Errorf("0002's replaces = %+v, want %+v: the two comment lines, not the literal's or the body's", got[1].Replaces, want)
	}
	if id := got[1].Identity(); id.Name != "0002_second.up.sql" || id.Checksum != sha256Hex(text) {
		t.Errorf("Identity() = %+v", id)
	}
}

// A comment that begins with replaces:, in any case, is a replaces line, and one not written in
// the form a runner can match refuses the whole set at load, naming the file, its line and the
// form: a runner that skipped it would refuse a database the author meant the line to save, or
// read a wrong checksum as a promise. A line naming a file of another version is refused too,
// since a runner judges the row of a version against the file of that version alone. The control
// is the same file with the well-formed line, which loads.
func TestLoadRefusesAMalformedReplacesLine(t *testing.T) {
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
	own := Identity{Name: "0002_second.up.sql", Checksum: strings.Repeat("1", 64)}
	earlier := Identity{Name: "0002_second.up.sql", Checksum: strings.Repeat("2", 64)}
	other := Identity{Name: "0002_other.up.sql", Checksum: strings.Repeat("1", 64)}
	set := []Migration{{Version: 2, Name: own.Name, Checksum: own.Checksum, Replaces: []Identity{earlier}}}
	for name, tc := range map[string]struct {
		row     Recorded
		fixes   []Fix
		refused []string
	}{
		"its own identity":              {row: Recorded{Version: 2, Identity: own}},
		"no name":                       {row: Recorded{Version: 2}, fixes: []Fix{{Version: 2, To: own}}},
		"a replaced identity":           {row: Recorded{Version: 2, Identity: earlier}, fixes: []Fix{{Version: 2, From: earlier, To: own}}},
		"a version with no file":        {row: Recorded{Version: 7, Identity: other}},
		"another name":                  {row: Recorded{Version: 2, Identity: other}, refused: []string{"version 2 is recorded as " + other.String(), "file of version 2 is " + own.String(), "-- replaces: " + other.String()}},
		"the replaced name, other text": {row: Recorded{Version: 2, Identity: Identity{Name: own.Name, Checksum: strings.Repeat("3", 64)}}, refused: []string{"whose replaces lines name only " + earlier.String()}},
	} {
		t.Run(name, func(t *testing.T) {
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
		})
	}
	// One refused row refuses every fix the other rows needed: nothing is written.
	fixes, err := Reconcile(set, []Recorded{{Version: 2}, {Version: 2, Identity: other}})
	if err == nil || fixes != nil {
		t.Errorf("Reconcile of a row to fill beside a refused one = %+v, %v; want the refusal and no fix", fixes, err)
	}
}
