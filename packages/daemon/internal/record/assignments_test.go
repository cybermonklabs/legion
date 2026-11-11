package record

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
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

// The hand-over settles the assignments of a child handed over as a tree of its own. Out of a tree
// the daemon had opened it copies every row of the old tree's root verbatim — arm, mode and the
// original instant — under the carried mark, and settles each experiment declared at the hand-over
// that the old tree held no row for at no variant, no mode and no instant, so that experiment is
// never drawn for the child later. Out of a tree nothing ever opened it writes nothing at all, and
// a row the child already holds is never written over.
func TestTheHandOverCarriesTheAssignmentsOfAnOpenedTree(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	drawnAt := time.Date(2026, 11, 11, 4, 56, 36, 0, time.UTC)
	later := drawnAt.Add(time.Hour)

	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range []string{"LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4", "LEGION-5", "LEGION-6"} {
			putRoot(t, ctx, records, tx, key)
		}
		read := func(key string) []string {
			t.Helper()
			assignments, err := records.AssignmentsOf(ctx, tx, key)
			must(t, err)
			return assignmentsAs(assignments)
		}
		// The trees the carries below read: LEGION-1 was opened, LEGION-6 never was.
		for _, tree := range []string{"LEGION-1"} {
			if _, err := treelifecycle.Open(ctx, tx, treeLifecyclesToken, tree, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("open the tree %s: %v", tree, err)
			}
		}
		// The old tree's own draw: one experiment at an arm, one at none.
		if _, err := records.PutAssignments(ctx, tx, "LEGION-1", []Assignment{
			{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &drawnAt},
			{Experiment: "review-depth", Unit: "tree", Variant: "", Mode: "off", DrawnAt: &drawnAt},
		}); err != nil {
			t.Fatalf("draw the old tree's experiments: %v", err)
		}
		// The experiments declared at the hand-over: the two the old tree drew, and one younger
		// than that tree, which it holds no row for.
		declared := []Assignment{
			{Experiment: "memory", Unit: "tree"},
			{Experiment: "review-depth", Unit: "tree"},
			{Experiment: "plan-length", Unit: "tree"},
		}

		// The copy is verbatim, the original instant included, and the younger experiment is
		// settled at no variant: carried either way, so no draw writes over it.
		carried, err := records.CarryAssignments(ctx, tx, treeLifecyclesToken, "LEGION-2", "LEGION-1", declared)
		must(t, err)
		if carried != 3 {
			t.Fatalf("the hand-over wrote %d rows, want the old tree's two copied and the third settled", carried)
		}
		want := []string{
			assignedAs(Assignment{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &drawnAt, Carried: true}),
			assignedAs(Assignment{Experiment: "plan-length", Unit: "tree", Carried: true}),
			assignedAs(Assignment{Experiment: "review-depth", Unit: "tree", Variant: "", Mode: "off", DrawnAt: &drawnAt, Carried: true}),
		}
		if got := read("LEGION-2"); !reflect.DeepEqual(got, want) {
			t.Fatalf("the handed-over root holds %v, want %v", got, want)
		}
		// What the hand-over settled, the child's own first draw writes nothing over.
		drew, err := records.PutAssignments(ctx, tx, "LEGION-2", []Assignment{
			{Experiment: "plan-length", Unit: "tree", Variant: "short", Mode: "random", DrawnAt: &later},
		})
		must(t, err)
		if drew != 0 {
			t.Fatalf("the first draw of a settled root wrote %d rows, want its hand-over's answer kept", drew)
		}
		if got := read("LEGION-2"); !reflect.DeepEqual(got, want) {
			t.Fatalf("after its own first draw the settled root holds %v, want %v", got, want)
		}

		// A child out of a tree nothing ever opened carries nothing, not even the settled rows:
		// its own first draw answers for it.
		empty, err := records.CarryAssignments(ctx, tx, treeLifecyclesToken, "LEGION-3", "LEGION-6", declared)
		must(t, err)
		if empty != 0 {
			t.Fatalf("a hand-over out of a tree nothing ever opened wrote %d rows, want nothing written", empty)
		}
		if got := read("LEGION-3"); len(got) != 0 {
			t.Fatalf("a child out of a tree nothing ever opened holds %v, want no assignment", got)
		}
		own, err := records.PutAssignments(ctx, tx, "LEGION-3", []Assignment{
			{Experiment: "memory", Unit: "tree", Variant: "none", Mode: "random", DrawnAt: &later},
		})
		must(t, err)
		if own != 1 {
			t.Fatalf("the first draw of a child out of an unopened tree wrote %d rows, want its own draw recorded", own)
		}

		// A lifecycle row of another project's tree of the same key is no tree of this project's.
		if _, err := treelifecycle.Open(ctx, tx, "acmewidgets", "LEGION-6", treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open another project's tree LEGION-6: %v", err)
		}
		elsewhere, err := records.CarryAssignments(ctx, tx, treeLifecyclesToken, "LEGION-4", "LEGION-6", declared)
		must(t, err)
		if elsewhere != 0 {
			t.Fatalf("a hand-over reading another project's lifecycle row wrote %d rows, want nothing written", elsewhere)
		}

		// A row the child already holds is never written over, by the copy or by the settling.
		kept := assignedAs(Assignment{Experiment: "memory", Unit: "tree", Variant: "none", Mode: "off", DrawnAt: &later})
		if _, err := records.PutAssignments(ctx, tx, "LEGION-5", []Assignment{
			{Experiment: "memory", Unit: "tree", Variant: "none", Mode: "off", DrawnAt: &later},
		}); err != nil {
			t.Fatalf("draw the child's own memory arm: %v", err)
		}
		over, err := records.CarryAssignments(ctx, tx, treeLifecyclesToken, "LEGION-5", "LEGION-1", declared)
		must(t, err)
		if over != 2 {
			t.Fatalf("the hand-over onto a root that already holds a row wrote %d rows, want the two it holds none for", over)
		}
		wantKept := []string{
			kept,
			assignedAs(Assignment{Experiment: "plan-length", Unit: "tree", Carried: true}),
			assignedAs(Assignment{Experiment: "review-depth", Unit: "tree", Variant: "", Mode: "off", DrawnAt: &drawnAt, Carried: true}),
		}
		if got := read("LEGION-5"); !reflect.DeepEqual(got, wantKept) {
			t.Fatalf("after a carry onto a root that already drew it holds %v, want its own row kept: %v", got, wantKept)
		}
	})
}
