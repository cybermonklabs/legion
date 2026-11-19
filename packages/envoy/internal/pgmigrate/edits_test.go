package pgmigrate

import (
	"strings"
	"testing"
	"testing/fstest"
)

// loadSet is the set Load gives of files, each name a file of the migrations directory and each
// value its text.
func loadSet(t *testing.T, files map[string]string) []Migration {
	t.Helper()
	set := fstest.MapFS{}
	for name, text := range files {
		set["migrations/"+name] = &fstest.MapFile{Data: []byte(text)}
	}
	migrations, err := Load(set, "migrations")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return migrations
}

// checkEdits runs CheckEdits on base and head and requires the refusal count, the unchanged and
// the accepted count, and that the report says each of refused.
func checkEdits(t *testing.T, base, head map[string]string, unchanged, declared int, refused ...string) {
	t.Helper()
	check := CheckEdits("dispatch", loadSet(t, base), loadSet(t, head))
	var report strings.Builder
	check.Write(&report)
	t.Logf("report:\n%s", report.String())
	refusals := 0
	if len(refused) > 0 {
		refusals = 1
	}
	if check.Compared != len(base) || check.Unchanged != unchanged || check.Declared != declared || len(check.Refusals) != refusals {
		t.Errorf("compared %d, unchanged %d, declared %d, refusals %v; want %d, %d, %d and %d refusal(s)",
			check.Compared, check.Unchanged, check.Declared, check.Refusals, len(base), unchanged, declared, refusals)
	}
	for _, want := range refused {
		if !strings.Contains(report.String(), want) {
			t.Errorf("report = %q, want it to say %q", report.String(), want)
		}
	}
}

// CheckEdits compares each file a base holds with the head's file of its version. The control is
// a head whose files are the base's own, which compares every file and refuses none. A
// comment-only edit, a rename and a deleted file are each refused alone, naming both identities
// and the line to put first; the same edit or rename with that line is accepted.
func TestCheckEditsRefusesAnEditOfAFileTheBaseHoldsThatDeclaresNothing(t *testing.T) {
	const first, second = "create table first (id integer)", "create table second (id integer)"
	base := map[string]string{"0001_first.up.sql": first, "0002_second.up.sql": second}
	baseSecond := Identity{Name: "0002_second.up.sql", Checksum: sha256Hex(second)}
	declares := "\n-- replaces: " + baseSecond.String() + "\n"
	for name, tc := range map[string]struct {
		head      map[string]string
		refused   []string
		unchanged int
		declared  int
	}{
		"control: the base's own files": {head: map[string]string{"0001_first.up.sql": first, "0002_second.up.sql": second, "0003_third.up.sql": "select 3"}, unchanged: 2},
		"a comment-only edit": {head: map[string]string{"0001_first.up.sql": first, "0002_second.up.sql": second + "\n-- a comment\n"}, unchanged: 1,
			refused: []string{"0002_second.up.sql is 0002_second.up.sql sha256:", "records version 2 as " + baseSecond.String(), "make this line its first replaces line, before any other: -- replaces: " + baseSecond.String()}},
		"a rename": {head: map[string]string{"0001_first.up.sql": first, "0002_renamed.up.sql": second}, unchanged: 1,
			refused: []string{"0002_renamed.up.sql is 0002_renamed.up.sql sha256:", "records version 2 as " + baseSecond.String()}},
		"a deleted file": {head: map[string]string{"0001_first.up.sql": first}, unchanged: 1,
			refused: []string{"the base holds " + baseSecond.String() + " and this head has no migration of version 2"}},
		"an edit that declares the base":  {head: map[string]string{"0001_first.up.sql": first, "0002_second.up.sql": second + "\n-- a comment" + declares}, unchanged: 1, declared: 1},
		"a rename that declares the base": {head: map[string]string{"0001_first.up.sql": first, "0002_renamed.up.sql": second + declares}, unchanged: 1, declared: 1},
	} {
		t.Run(name, func(t *testing.T) {
			checkEdits(t, base, tc.head, tc.unchanged, tc.declared, tc.refused...)
		})
	}
	t.Run("a base that holds no migration compares nothing and says so", func(t *testing.T) {
		check := CheckEdits("broker", nil, nil)
		var report strings.Builder
		check.Write(&report)
		if check.Compared != 0 || !strings.Contains(report.String(), "NOTHING COMPARED") {
			t.Errorf("report = %q; want NOTHING COMPARED", report.String())
		}
	})
}

// A base whose file already declares a replaces line. A database the base migrated records the
// first identity, and, where it applied the file, the base's own text. A second edit that keeps
// the base's line first and adds one naming the base's text is accepted. One that drops the base's
// line, or puts it after another, changes what a runner records, and one that keeps it first but
// names no text of the base's refuses a database that applied the base's file: each is refused,
// and the message says which line comes first. A revert to the original text is refused for the
// same reason: a database that applied the base's edit holds that text. The control is the base's
// own file.
func TestCheckEditsHoldsTheFirstReplacesLineOfAChain(t *testing.T) {
	const original, firstEdit = "create table second (id integer)", "create table second (id integer, note text)"
	originalIdentity := Identity{Name: "0002_second.up.sql", Checksum: sha256Hex(original)}
	baseText := firstEdit + "\n-- replaces: " + originalIdentity.String() + "\n"
	baseIdentity := Identity{Name: "0002_second.up.sql", Checksum: sha256Hex(baseText)}
	secondEdit := firstEdit + "\n-- a second edit\n"
	base := map[string]string{"0002_second.up.sql": baseText}
	for name, tc := range map[string]struct {
		head     string
		declared int
		refused  string
	}{
		"control: the base's own file":    {head: baseText},
		"the chain kept in order":         {head: secondEdit + "-- replaces: " + originalIdentity.String() + "\n-- replaces: " + baseIdentity.String() + "\n", declared: 1},
		"the base's first line dropped":   {head: secondEdit + "-- replaces: " + baseIdentity.String() + "\n", refused: "make this line its first replaces line, before any other: -- replaces: " + originalIdentity.String()},
		"the base's first line put after": {head: secondEdit + "-- replaces: " + baseIdentity.String() + "\n-- replaces: " + originalIdentity.String() + "\n", refused: "records version 2 as " + originalIdentity.String()},
		"the base's own text not named":   {head: secondEdit + "-- replaces: " + originalIdentity.String() + "\n", refused: "may have applied the base's text of version 2, " + baseIdentity.String() + ", which this head's file does not accept"},
		"a revert to the original text":   {head: original, refused: "add these two lines, in this order: -- replaces: " + originalIdentity.String() + " and -- replaces: " + baseIdentity.String()},
	} {
		t.Run(name, func(t *testing.T) {
			var refused []string
			if tc.refused != "" {
				refused = append(refused, tc.refused)
			}
			unchanged := 0
			if tc.head == baseText {
				unchanged = 1
			}
			checkEdits(t, base, map[string]string{"0002_second.up.sql": tc.head}, unchanged, tc.declared, refused...)
		})
	}
}
