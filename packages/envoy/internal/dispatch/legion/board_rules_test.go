package legion

import (
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
