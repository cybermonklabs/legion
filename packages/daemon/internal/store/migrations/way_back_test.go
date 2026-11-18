package migrations

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The way back a migration's header carries is the run of "-- " lines that closes with the line
// deleting that migration's own version row. The prose above it, which a blank "--" line ends,
// and the statements below it are none of it, so the block is the statements an operator runs
// and nothing else.
func TestAWayBackIsTheCommentRunThatDeletesItsOwnVersion(t *testing.T) {
	const version = 20990101000001
	set := []Migration{{Version: version, Name: "20990101000001_example.up.sql", SQL: strings.Join([]string{
		"-- 20990101000001_example.up.sql — what the migration does.",
		"--",
		"-- The way back below re-arms it: run it with the daemon stopped.",
		"--",
		"-- insert into kept (issue) select key from issues;",
		"-- delete from schema_version where version = 20990101000001;",
		"insert into applied (issue) values ('LEGION-1');",
	}, "\n")}}
	got, err := wayBack(set, version)
	want := "insert into kept (issue) select key from issues;\ndelete from schema_version where version = 20990101000001;"
	if err != nil || got != want {
		t.Fatalf("the way back of %d = %q, %v; want the two statements of its block alone:\n%s", version, got, err, want)
	}
}

// A header with no block closing with its own version's delete, and a version the set does not
// hold, answer an error, so no caller runs an empty block against a store as though it were the
// way back.
func TestAWayBackThatIsNotThereIsAnError(t *testing.T) {
	set := []Migration{{Version: 20990101000002, Name: "20990101000002_no_way_back.up.sql",
		SQL: "-- 20990101000002_no_way_back.up.sql — nothing to undo.\n--\n-- delete from schema_version where version = 20990101000001;\nselect 1;\n"}}
	for _, tc := range []struct {
		version int
		names   string
	}{
		{20990101000002, "20990101000002_no_way_back.up.sql"},
		{20990101000003, "20990101000003"},
	} {
		if got, err := wayBack(set, tc.version); err == nil || got != "" || !strings.Contains(err.Error(), tc.names) {
			t.Errorf("the way back of %d = %q, %v; want no block and an error naming %s", tc.version, got, err, tc.names)
		}
	}
}

// The roll forward of a rollback of the release that records each step carries its way back in
// its header: two statements on five lines, the insert that keeps each issue's witness and the
// delete of the roll forward's own version row. StepRecordsRollForward, the version every test of
// that block names, is the version of the roll forward's own file.
func TestTheStepRecordsRollForwardCarriesItsWayBack(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	const file = "_step_starts_roll_forward.up.sql"
	if !slices.ContainsFunc(all, func(m Migration) bool {
		return m.Version == StepRecordsRollForward && m.Name == fmt.Sprint(StepRecordsRollForward)+file
	}) {
		t.Fatalf("no embedded migration is %d%s: StepRecordsRollForward names the roll forward's version", StepRecordsRollForward, file)
	}
	block, err := WayBack(StepRecordsRollForward)
	if err != nil {
		t.Fatalf("read the way back of the step records' roll forward: %v", err)
	}
	lines := strings.Split(block, "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "insert into step_starts_rollback ") ||
		lines[4] != fmt.Sprintf("delete from schema_version where version = %d;", StepRecordsRollForward) {
		t.Fatalf("the way back of the step records' roll forward is %d lines, want the five from its insert into step_starts_rollback to the delete of its own version row:\n%s",
			len(lines), block)
	}
}
