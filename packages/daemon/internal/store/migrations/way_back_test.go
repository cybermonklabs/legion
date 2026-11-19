package migrations

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The way back a migration's header carries is the last paragraph of the header: the "-- " lines
// after its last blank "--" line, up to the migration's first statement. The prose paragraphs above
// it are none of it, so the block is the statements an operator runs and nothing else.
func TestAWayBackIsTheLastParagraphOfItsHeader(t *testing.T) {
	const version = 20990101000001
	for _, header := range []struct {
		shape string
		sql   []string
		want  string
	}{
		{"a block that deletes its own version row", []string{
			"-- 20990101000001_example.up.sql — what the migration does.",
			"--",
			"-- The way back below re-arms it: run it with the daemon stopped.",
			"--",
			"-- insert into kept (issue) select key from issues;",
			"-- delete from schema_version where version = 20990101000001;",
			"insert into applied (issue) values ('LEGION-1');",
		}, "insert into kept (issue) select key from issues;\ndelete from schema_version where version = 20990101000001;"},
		{"a block of SQL with no row delete, below a paragraph of prose", []string{
			"-- 20990101000001_example.up.sql — what the migration does.",
			"--",
			"-- A rollback deletes the rows of the kind first: the way back is the block below.",
			"--",
			"-- -- The rows of the kind.",
			"-- delete from outbox where kind = 'example';",
			"alter table outbox add column example text;",
		}, "-- The rows of the kind.\ndelete from outbox where kind = 'example';"},
		{"a block of one statement that deletes no row", []string{
			"-- 20990101000001_example.up.sql — what the migration does.",
			"--",
			"-- insert into kept (issue) select key from issues;",
			"select 1;",
		}, "insert into kept (issue) select key from issues;"},
		{"a comment after the first statement", []string{
			"-- 20990101000001_example.up.sql — what the migration does.",
			"--",
			"-- select 1;",
			"insert into applied (issue) values ('LEGION-1');",
			"-- delete from schema_version where version = 20990101000001;",
		}, "select 1;"},
	} {
		set := []Migration{{Version: version, Name: "20990101000001_example.up.sql", SQL: strings.Join(header.sql, "\n")}}
		if got, err := wayBack(set, version); err != nil || got != header.want {
			t.Errorf("%s: the way back of %d = %q, %v; want its header's last paragraph alone:\n%s", header.shape, version, got, err,
				header.want)
		}
	}
}

// A header whose last paragraph is prose carries no way back, even where a paragraph above it is a
// block, and a version the set does not hold has none: each answers an error, so no caller runs an
// empty block, or a block the header does not offer as its way back, against a store.
func TestAWayBackThatIsNotThereIsAnError(t *testing.T) {
	set := []Migration{{Version: 20990101000002, Name: "20990101000002_no_way_back.up.sql",
		SQL: "-- 20990101000002_no_way_back.up.sql — nothing to undo.\n--\n" +
			"-- delete from schema_version where version = 20990101000002;\n--\n" +
			"-- Nothing above is a way back: a rollback leaves this migration's change in place.\nselect 1;\n"}}
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
// delete of the roll forward's own version row. That last line is what makes the roll forward run
// again, as the newer image's Migrate applies each migration whose row is missing, and WayBack
// reads no statement of a block, so this test is what holds it. StepRecordsRollForward, the
// version every test of that block names, is the version of the roll forward's own file.
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
