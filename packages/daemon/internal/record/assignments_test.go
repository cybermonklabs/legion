package record

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// treeLifecyclesToken is the project tree_lifecycles is keyed by in these tests: the normalized
// runtime token, where the issue records are under the Dispatch project key LEGION. The hand-over
// is handed the token, so a lifecycle row of another project's tree of the same key is not this
// project's.
const treeLifecyclesToken = "legion"

// assignedAs is an assignment as a reader sees it, so two rows are compared whole and a failure
// names what each holds.
func assignedAs(assignment Assignment) string {
	at := "no draw"
	if assignment.DrawnAt != nil {
		at = assignment.DrawnAt.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s (%s) at variant %q drawn as %q at %s, carried %t",
		assignment.Experiment, assignment.Unit, assignment.Variant, assignment.Mode, at, assignment.Carried)
}

// assignmentsAs is every row of a unit in the order it is read back.
func assignmentsAs(assignments []Assignment) []string {
	read := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		read = append(read, assignedAs(assignment))
	}
	return read
}

// An assignment is written once and then kept. The first PutAssignments of a unit writes a row for
// each experiment it is given and reports how many; a second pass over the same experiments writes
// nothing and reports it, whatever the project's file says by then, so a unit keeps the arm its
// work has been running under; and a write for a key the daemon has no record of — an operator's
// spawn — writes nothing and is no error. AssignmentsOf reads one unit's rows in experiment order,
// and AssignmentsFor reads many units' in one statement, with no entry for a unit that holds none
// and nothing read at all for an empty list.
func TestAnExperimentAssignmentIsWrittenOnceAndReadBack(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	drawnAt := time.Date(2026, 11, 11, 4, 56, 36, 123456000, time.UTC)
	later := drawnAt.Add(time.Hour)

	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range []string{"LEGION-1", "LEGION-2"} {
			putRoot(t, ctx, records, tx, key)
		}
		read := func(key string) []string {
			t.Helper()
			assignments, err := records.AssignmentsOf(ctx, tx, key)
			must(t, err)
			return assignmentsAs(assignments)
		}
		if got := read("LEGION-1"); len(got) != 0 {
			t.Fatalf("a freshly recorded root holds %v, want no assignment: nothing has drawn for it", got)
		}

		// The draw writes a row for each experiment and says how many.
		drawn := []Assignment{
			{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &drawnAt},
			{Experiment: "review-depth", Unit: "tree", Variant: "", Mode: "off"},
		}
		wrote, err := records.PutAssignments(ctx, tx, "LEGION-1", drawn)
		must(t, err)
		if wrote != 2 {
			t.Fatalf("the first PutAssignments wrote %d rows, want both of the unit's experiments", wrote)
		}
		want := []string{
			assignedAs(Assignment{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &drawnAt}),
			assignedAs(Assignment{Experiment: "review-depth", Unit: "tree", Mode: "off"}),
		}
		if got := read("LEGION-1"); !reflect.DeepEqual(got, want) {
			t.Fatalf("after the draw the root holds %v, want %v in experiment order", got, want)
		}

		// A second pass changes nothing and reports that it wrote nothing, so a later pass under
		// another mode leaves the unit on the arm it has been running under.
		again, err := records.PutAssignments(ctx, tx, "LEGION-1", []Assignment{
			{Experiment: "memory", Unit: "tree", Variant: "none", Mode: "off", DrawnAt: &later},
			{Experiment: "review-depth", Unit: "tree", Variant: "deep", Mode: "on", DrawnAt: &later},
		})
		must(t, err)
		if again != 0 {
			t.Fatalf("a second PutAssignments of the same experiments wrote %d rows, want each row written once", again)
		}
		if got := read("LEGION-1"); !reflect.DeepEqual(got, want) {
			t.Fatalf("after a second pass the root holds %v, want the first draw's %v", got, want)
		}

		// A key the daemon holds no record of takes no row, and that is no error: the row's
		// foreign key would refuse the write, so the statement guards on the record itself.
		spawned, err := records.PutAssignments(ctx, tx, "LEGION-404", drawn)
		must(t, err)
		if spawned != 0 {
			t.Fatalf("PutAssignments on a key with no record wrote %d rows, want nothing written", spawned)
		}
		if got := read("LEGION-404"); len(got) != 0 {
			t.Fatalf("a key with no record holds %v, want no assignment", got)
		}

		// An experiment drawn for one issue of a tree rather than the whole tree is keyed by that
		// issue, which is what the unit on the row says.
		own, err := records.PutAssignments(ctx, tx, "LEGION-2", []Assignment{
			{Experiment: "plan-length", Unit: "issue", Variant: "short", Mode: "random", DrawnAt: &later},
		})
		must(t, err)
		if own != 1 {
			t.Fatalf("the draw of an issue-unit experiment wrote %d rows, want 1", own)
		}

		// The batched read gives every unit's rows, and nothing for a unit that holds none.
		many, err := records.AssignmentsFor(ctx, tx, []string{"LEGION-1", "LEGION-2", "LEGION-3"})
		must(t, err)
		batched := map[string][]string{}
		for key, assignments := range many {
			batched[key] = assignmentsAs(assignments)
		}
		wantMany := map[string][]string{
			"LEGION-1": want,
			"LEGION-2": {assignedAs(Assignment{Experiment: "plan-length", Unit: "issue", Variant: "short", Mode: "random", DrawnAt: &later})},
		}
		if !reflect.DeepEqual(batched, wantMany) {
			t.Fatalf("the batched read gives %v, want %v: an issue with no assignment has no entry", batched, wantMany)
		}
		empty, err := records.AssignmentsFor(ctx, tx, nil)
		must(t, err)
		if len(empty) != 0 {
			t.Fatalf("the batched read of no issue gives %v, want nothing read", empty)
		}
	})
}

// An issue at its first start takes, for each tree-unit experiment, the row of the nearest issue
// above it that holds one (CarryAssignments over Chains.Above's order), verbatim — arm, mode and
// the original instant — under the carried mark. An issue-unit row above is that issue's own
// answer and is not copied. An experiment none above holds is left to the issue's own draw, and a
// row the issue already holds is never written over.
func TestTheNearestAssignedIssueAboveGivesTheTreeAssignments(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	drawnAt := time.Date(2026, 11, 11, 4, 56, 36, 0, time.UTC)
	later := drawnAt.Add(time.Hour)

	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range []string{"LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4", "LEGION-5"} {
			putRoot(t, ctx, records, tx, key)
		}
		read := func(key string) []string {
			t.Helper()
			assignments, err := records.AssignmentsOf(ctx, tx, key)
			must(t, err)
			return assignmentsAs(assignments)
		}
		// The farther issue above holds memory and review-depth, and an issue experiment of its
		// own; the nearer one holds memory alone, at another arm.
		if _, err := records.PutAssignments(ctx, tx, "LEGION-1", []Assignment{
			{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &drawnAt},
			{Experiment: "review-depth", Unit: "tree", Variant: "", Mode: "off", DrawnAt: &drawnAt},
			{Experiment: "handoff-note", Unit: "issue", Variant: "long", Mode: "random", DrawnAt: &drawnAt},
		}); err != nil {
			t.Fatalf("draw the farther issue's experiments: %v", err)
		}
		if _, err := records.PutAssignments(ctx, tx, "LEGION-2", []Assignment{
			{Experiment: "memory", Unit: "tree", Variant: "none", Mode: "random", DrawnAt: &later},
		}); err != nil {
			t.Fatalf("draw the nearer issue's experiment: %v", err)
		}

		carried, err := records.CarryAssignments(ctx, tx, "LEGION-3", []string{"LEGION-2", "LEGION-1"})
		must(t, err)
		if carried != 2 {
			t.Fatalf("the carry wrote %d rows, want memory from the nearer issue and review-depth from the farther", carried)
		}
		want := []string{
			assignedAs(Assignment{Experiment: "memory", Unit: "tree", Variant: "none", Mode: "random", DrawnAt: &later, Carried: true}),
			assignedAs(Assignment{Experiment: "review-depth", Unit: "tree", Variant: "", Mode: "off", DrawnAt: &drawnAt, Carried: true}),
		}
		if got := read("LEGION-3"); !reflect.DeepEqual(got, want) {
			t.Fatalf("the started issue holds %v, want %v", got, want)
		}

		// Nothing above: nothing carried, the issue's own draw answers for it.
		empty, err := records.CarryAssignments(ctx, tx, "LEGION-4", nil)
		must(t, err)
		if empty != 0 || len(read("LEGION-4")) != 0 {
			t.Fatalf("a carry with nothing above wrote %d rows and left %v, want nothing", empty, read("LEGION-4"))
		}

		// A row the issue already holds is never written over.
		kept := assignedAs(Assignment{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "on", DrawnAt: &later})
		if _, err := records.PutAssignments(ctx, tx, "LEGION-5", []Assignment{
			{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "on", DrawnAt: &later},
		}); err != nil {
			t.Fatalf("draw the issue's own memory arm: %v", err)
		}
		over, err := records.CarryAssignments(ctx, tx, "LEGION-5", []string{"LEGION-2", "LEGION-1"})
		must(t, err)
		if over != 1 {
			t.Fatalf("the carry onto an issue that already holds a row wrote %d rows, want the one it holds none for", over)
		}
		wantKept := []string{kept,
			assignedAs(Assignment{Experiment: "review-depth", Unit: "tree", Variant: "", Mode: "off", DrawnAt: &drawnAt, Carried: true})}
		if got := read("LEGION-5"); !reflect.DeepEqual(got, wantKept) {
			t.Fatalf("after a carry onto an issue that already drew it holds %v, want its own row kept: %v", got, wantKept)
		}
	})
}

// Assignments is the rows of one project's issues and of no other project's. The database is
// shared by every project's daemon and a row is keyed by an issue, so the project a row belongs
// to is the one on the issue it is keyed by, which the read joins the issues to find: two
// projects whose issues each hold a row are read apart, and the read of one project answers the
// other's rows for nobody.
func TestTheAssignmentsOfAProjectLeaveEveryOtherProjectsOut(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	drawnAt := time.Date(2026, 11, 11, 4, 56, 36, 123456000, time.UTC)

	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []Issue{
			{Key: "AAA-1", Project: "AAA"}, {Key: "AAA-2", Project: "AAA"}, {Key: "BBB-1", Project: "BBB"},
		} {
			issue.Repo, issue.GitHubRepo, issue.DefaultBranch = "sjawhar/legion", "sjawhar/legion", "main"
			issue.Tree, issue.Title, issue.Phase = issue.Key, issue.Key, phase.Admitted
			issue.Generation, issue.Status, issue.Rank = 1, "in_progress", "U"
			must(t, records.PutIssue(ctx, tx, issue))
		}
		for key, drawn := range map[string]Assignment{
			"AAA-1": {Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &drawnAt},
			"AAA-2": {Experiment: "plan-length", Unit: "issue", Variant: "short", Mode: "random", DrawnAt: &drawnAt},
			"BBB-1": {Experiment: "memory", Unit: "tree", Variant: "none", Mode: "random", DrawnAt: &drawnAt},
		} {
			wrote, err := records.PutAssignments(ctx, tx, key, []Assignment{drawn})
			must(t, err)
			if wrote != 1 {
				t.Fatalf("the draw of %s wrote %d rows, want its one row", key, wrote)
			}
		}

		read, err := records.Assignments(ctx, tx, "AAA")
		must(t, err)
		got := map[string][]string{}
		for key, assignments := range read {
			got[key] = assignmentsAs(assignments)
		}
		want := map[string][]string{
			"AAA-1": {assignedAs(Assignment{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &drawnAt})},
			"AAA-2": {assignedAs(Assignment{Experiment: "plan-length", Unit: "issue", Variant: "short", Mode: "random", DrawnAt: &drawnAt})},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the assignments of AAA are %v, want %v: another project's rows are no entry of this read", got, want)
		}
	})
}
