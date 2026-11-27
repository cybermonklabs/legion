package legion

import (
	"slices"
	"strings"
	"testing"
)

// TestEachPhaseIsInOneColumn: PhaseColumns gives every phase of Columns the one column that lists
// it, and phaseColumns panics on a table that lists one phase in two columns, naming the phase
// and both columns, so such a table stops the package at start (PhaseColumns is read off Columns
// then) rather than put a card in one column and its hours under another.
func TestEachPhaseIsInOneColumn(t *testing.T) {
	listed := 0
	for _, column := range Columns {
		for _, phase := range column.Phases {
			listed++
			if got := PhaseColumns[phase]; got != column.ID {
				t.Errorf("PhaseColumns[%q] = %q, want %q, the column that lists it", phase, got, column.ID)
			}
		}
	}
	if len(PhaseColumns) != listed {
		t.Errorf("PhaseColumns holds %d phases, want the %d Columns lists", len(PhaseColumns), listed)
	}

	recovered := func() (value any) {
		defer func() { value = recover() }()
		phaseColumns([]Column{
			{"first", []string{PhaseMerging}},
			{"second", []string{PhaseAwaitingMerge, PhaseMerging}},
		})
		return nil
	}()
	message, ok := recovered.(string)
	if !ok {
		t.Fatalf("phaseColumns over a phase in two columns recovered %#v, want a panic with a message", recovered)
	}
	for _, named := range []string{PhaseMerging, "first", "second"} {
		if !strings.Contains(message, `"`+named+`"`) {
			t.Errorf("the panic %q does not name %q", message, named)
		}
	}
}

// TestTheWaitForAMergedIssuesChecksHasAColumnOfItsOwn: `verifying`, the phase a merged issue
// waits in while its close waits for the checks of its Verification document
// (cybermonklabs/legion#5558), stands in a column of its own, between Merging and Waiting on
// children. The phase words are written out here rather than read from the constants, since what
// this holds is the table, not the spelling.
//
// A phase no column holds is a phase the board shows nowhere: its issue is on no card and its
// hours are under no column. For `done` that is the point, the issue having left the workflow.
// `verifying` is the one state this change creates, the state where work now waits that never
// waited before, so a board that drew it under Merging would tell the operator the story before
// this change — an issue in the merger's hands — and a board that drew it nowhere would hide the
// change's whole effect from the one view an operator watches.
func TestTheWaitForAMergedIssuesChecksHasAColumnOfItsOwn(t *testing.T) {
	column, named := PhaseColumns["verifying"]
	if !named || column != "verifying" {
		t.Fatalf("PhaseColumns[\"verifying\"] = %q (named %v), want the column %q of its own", column, named, "verifying")
	}
	ids := make([]string, len(Columns))
	for index, held := range Columns {
		ids[index] = held.ID
	}
	at := slices.Index(ids, "verifying")
	if at < 1 || at+1 >= len(ids) || ids[at-1] != ColumnMerging || ids[at+1] != ColumnAwaitingChildren {
		t.Errorf("the columns are %v, want verifying between merging and awaiting_children", ids)
	}
	if held := Columns[at].Phases; !slices.Equal(held, []string{"verifying"}) {
		t.Errorf("the verifying column holds the phases %v, want `verifying` alone", held)
	}
}
