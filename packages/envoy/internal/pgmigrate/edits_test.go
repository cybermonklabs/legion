package pgmigrate

import (
	"strings"
	"testing"
	"testing/fstest"
)

// CheckEdits compares each file a base holds with the head's file of its version. The control is
// a head whose files are the base's own, which compares every file and refuses none. A
// comment-only edit, a rename and a deleted file are each refused alone, naming both identities
// and the line to add; the same edit or rename with a replaces line naming the base's identity is
// accepted.
func TestCheckEditsRefusesAnEditOfAFileTheBaseHoldsThatDeclaresNothing(t *testing.T) {
	const first, second = "create table first (id integer)", "create table second (id integer)"
	base := []BaseFile{{Name: "0001_first.up.sql", Text: []byte(first)}, {Name: "0002_second.up.sql", Text: []byte(second)}}
	baseSecond := Identity{Name: "0002_second.up.sql", Checksum: sha256Hex(second)}
	declares := "\n-- replaces: " + baseSecond.String() + "\n"
	for name, tc := range map[string]struct {
		head      map[string]string
		refused   []string
		unchanged int
		declared  int
	}{
		"control: the base's own files":   {head: map[string]string{"0001_first.up.sql": first, "0002_second.up.sql": second, "0003_third.up.sql": "select 3"}, unchanged: 2},
		"a comment-only edit":             {head: map[string]string{"0001_first.up.sql": first, "0002_second.up.sql": second + "\n-- a comment\n"}, unchanged: 1, refused: []string{"0002_second.up.sql is 0002_second.up.sql sha256:", "the base holds " + baseSecond.String() + " under version 2", "add this line after them in 0002_second.up.sql: -- replaces: " + baseSecond.String()}},
		"a rename":                        {head: map[string]string{"0001_first.up.sql": first, "0002_renamed.up.sql": second}, unchanged: 1, refused: []string{"0002_renamed.up.sql is 0002_renamed.up.sql sha256:", "the base holds " + baseSecond.String()}},
		"a deleted file":                  {head: map[string]string{"0001_first.up.sql": first}, unchanged: 1, refused: []string{"the base holds " + baseSecond.String() + " and this head has no migration of version 2"}},
		"an edit that declares the base":  {head: map[string]string{"0001_first.up.sql": first, "0002_second.up.sql": second + "\n-- a comment" + declares}, unchanged: 1, declared: 1},
		"a rename that declares the base": {head: map[string]string{"0001_first.up.sql": first, "0002_renamed.up.sql": second + declares}, unchanged: 1, declared: 1},
	} {
		t.Run(name, func(t *testing.T) {
			set := fstest.MapFS{}
			for file, text := range tc.head {
				set["migrations/"+file] = &fstest.MapFile{Data: []byte(text)}
			}
			head, err := Load(set, "migrations")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			check, err := CheckEdits("dispatch", base, head)
			if err != nil {
				t.Fatalf("CheckEdits: %v", err)
			}
			var report strings.Builder
			check.Write(&report)
			t.Logf("report:\n%s", report.String())
			if check.Compared != 2 || check.Unchanged != tc.unchanged || check.Declared != tc.declared || len(check.Refusals) != len(tc.refused[:min(len(tc.refused), 1)]) {
				t.Errorf("compared %d, unchanged %d, declared %d, refusals %v; want 2, %d, %d and %d refusal(s)", check.Compared, check.Unchanged, check.Declared, check.Refusals, tc.unchanged, tc.declared, min(len(tc.refused), 1))
			}
			for _, want := range tc.refused {
				if !strings.Contains(report.String(), want) {
					t.Errorf("report = %q, want it to say %q", report.String(), want)
				}
			}
		})
	}
	t.Run("a base that holds no migration compares nothing and says so", func(t *testing.T) {
		check, err := CheckEdits("broker", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var report strings.Builder
		check.Write(&report)
		if check.Compared != 0 || !strings.Contains(report.String(), "NOTHING COMPARED") {
			t.Errorf("report = %q; want NOTHING COMPARED", report.String())
		}
	})
}

// A base whose file already declares a replaces line: a second edit keeps that line first and
// adds the base's own identity after it, and is accepted, since every release of the chain accepts
// the first identity, which a runner records. The same edit with the base's line dropped, or put
// after the new one, changes what a runner records and is refused. The control is the base's own
// file, unchanged.
func TestCheckEditsHoldsTheFirstReplacesLineOfAChain(t *testing.T) {
	const original, firstEdit = "create table second (id integer)", "create table second (id integer, note text)"
	originalIdentity := Identity{Name: "0002_second.up.sql", Checksum: sha256Hex(original)}
	baseText := firstEdit + "\n-- replaces: " + originalIdentity.String() + "\n"
	base := []BaseFile{{Name: "0002_second.up.sql", Text: []byte(baseText)}}
	baseIdentity := Identity{Name: "0002_second.up.sql", Checksum: sha256Hex(baseText)}
	secondEdit := firstEdit + "\n-- a second edit\n"
	for name, tc := range map[string]struct {
		head     string
		declared int
		refused  string
	}{
		"control: the base's own file":         {head: baseText},
		"the chain kept in order":              {head: secondEdit + "-- replaces: " + originalIdentity.String() + "\n-- replaces: " + baseIdentity.String() + "\n", declared: 1},
		"the base's first line dropped":        {head: secondEdit + "-- replaces: " + baseIdentity.String() + "\n", refused: "0002_second.up.sql's first replaces line names " + baseIdentity.String() + ", and a runner records version 2 as " + originalIdentity.String()},
		"the base's first line put after":      {head: secondEdit + "-- replaces: " + baseIdentity.String() + "\n-- replaces: " + originalIdentity.String() + "\n", refused: "keep every replaces line the base's 0002_second.up.sql has, in its order, before the new one"},
		"the base's own identity not declared": {head: secondEdit + "-- replaces: " + originalIdentity.String() + "\n", refused: "the base holds " + baseIdentity.String() + " under version 2"},
	} {
		t.Run(name, func(t *testing.T) {
			head, err := Load(fstest.MapFS{"migrations/0002_second.up.sql": {Data: []byte(tc.head)}}, "migrations")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			check, err := CheckEdits("dispatch", base, head)
			if err != nil {
				t.Fatalf("CheckEdits: %v", err)
			}
			var report strings.Builder
			check.Write(&report)
			t.Logf("report:\n%s", report.String())
			if tc.refused == "" {
				if len(check.Refusals) != 0 || check.Declared != tc.declared {
					t.Errorf("declared %d, refusals %v; want %d and none", check.Declared, check.Refusals, tc.declared)
				}
				return
			}
			if len(check.Refusals) != 1 || !strings.Contains(check.Refusals[0].Error(), tc.refused) {
				t.Errorf("refusals %v; want one saying %q", check.Refusals, tc.refused)
			}
		})
	}
}
