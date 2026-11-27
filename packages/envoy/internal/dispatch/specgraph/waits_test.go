package specgraph

import (
	"slices"
	"testing"
)

// none is the list of a unit that serves, needs or waits on none.
var none = []string{}

// v-work 10: "Two units each wait on a statement that the other unit serves. Dispatch refuses the
// second wait." R29: "a wait on a statement counts as a wait on each unit that serves that
// statement".
func TestTwoUnitsThatWaitOnEachOthersStatementsCloseACycle(t *testing.T) {
	t.Parallel()
	expectCycle(t, []Unit{
		{ID: "u-a", Serves: []string{"s-a"}, Needs: none, WaitsOn: []string{"s-b"}},
		{ID: "u-b", Serves: []string{"s-b"}, Needs: none, WaitsOn: []string{"s-a"}},
	}, []string{"u-a", "s-b", "u-b", "s-a", "u-a"})
}

func TestAUnitThatWaitsOnItsOwnStatementIsACycle(t *testing.T) {
	t.Parallel()
	expectCycle(t, []Unit{{ID: "u-a", Serves: []string{"s-a"}, Needs: none, WaitsOn: []string{"s-a"}}}, []string{"u-a", "s-a", "u-a"})
}

// A unit that needs another unit waits on it directly, so its step names no statement.
func TestNeedsAndWaitsJoin(t *testing.T) {
	t.Parallel()
	expectCycle(t, []Unit{
		{ID: "u-a", Serves: []string{"s-a"}, Needs: []string{"u-b"}, WaitsOn: none},
		{ID: "u-b", Serves: none, Needs: none, WaitsOn: []string{"s-a"}},
	}, []string{"u-a", "u-b", "s-a", "u-a"})
}

// A chain of waits ends at a unit that waits on a statement that no unit serves.
func TestNoCycle(t *testing.T) {
	t.Parallel()
	expectCycle(t, []Unit{
		{ID: "u-a", Serves: []string{"s-a"}, Needs: none, WaitsOn: []string{"s-b"}},
		{ID: "u-b", Serves: []string{"s-b"}, Needs: []string{"u-c"}, WaitsOn: []string{"s-c"}},
		{ID: "u-c", Serves: []string{"s-d"}, Needs: none, WaitsOn: none},
	}, nil)
}

// Of several cycles, WaitCycle gives the shortest one from the smallest unit that is on a cycle,
// whatever the order of the units: u-b's two units are on no cycle, and u-c is on a cycle of three
// units and on one of two.
func TestTheShortestCycleFromTheSmallestUnit(t *testing.T) {
	t.Parallel()
	units := []Unit{
		{ID: "u-z", Serves: []string{"s-z"}, Needs: none, WaitsOn: []string{"s-y"}},
		{ID: "u-y", Serves: []string{"s-y"}, Needs: none, WaitsOn: []string{"s-z"}},
		{ID: "u-e", Serves: []string{"s-e"}, Needs: none, WaitsOn: []string{"s-c"}},
		{ID: "u-d", Serves: []string{"s-d"}, Needs: none, WaitsOn: []string{"s-e"}},
		{ID: "u-c", Serves: []string{"s-c"}, Needs: none, WaitsOn: []string{"s-d", "s-e"}},
		{ID: "u-b", Serves: none, Needs: none, WaitsOn: []string{"s-c"}},
		{ID: "u-a", Serves: none, Needs: []string{"u-b"}, WaitsOn: none},
	}
	want := []string{"u-c", "s-e", "u-e", "s-c", "u-c"}
	expectCycle(t, units, want)
	slices.Reverse(units)
	expectCycle(t, units, want)
}

// expectCycle fails the test unless WaitCycle(units) is want, with no error.
func expectCycle(t *testing.T, units []Unit, want []string) {
	t.Helper()
	if got, err := WaitCycle(units); err != nil || !slices.Equal(got, want) || (got == nil) != (want == nil) {
		t.Errorf("WaitCycle = %q, %v, want %q", got, err, want)
	}
}

// A unit that sets no id would read as a unit named "", one of no letter and no digit as a unit
// named nothing, one that does not set serves, needs or waits_on as one with none, and a name of
// no letter and no digit in them as a statement or a unit that nothing else names, each of which
// hides a cycle of waits (R29); and a unit with the id of a unit before it would read as one unit
// with the relations of both, which can close a cycle that the units do not have, as u-a here,
// which would wait on its own statement. WaitCycle refuses each, in the words of the reader of
// units.json, and gives no cycle. The control row closes a cycle.
func TestWaitCycleRefusesAUnitThatIsNotWhole(t *testing.T) {
	t.Parallel()
	other := Unit{ID: "u-a", Serves: []string{"s-a"}, Needs: none, WaitsOn: []string{"s-b"}}
	for _, row := range []struct {
		name string
		unit Unit
		want string
	}{
		{"control: u-b serves s-b and waits on s-a", Unit{ID: "u-b", Serves: []string{"s-b"}, Needs: none, WaitsOn: []string{"s-a"}}, ""},
		{"no id", Unit{Serves: []string{"s-b"}, Needs: none, WaitsOn: []string{"s-a"}}, "specgraph: unit 2 has no id"},
		{"an id of no letter and no digit", Unit{ID: " ", Serves: []string{"s-b"}, Needs: none, WaitsOn: []string{"s-a"}},
			"specgraph: unit 2: its id has no letter and no digit, so it names no unit"},
		{"no serves", Unit{ID: "u-b", Needs: none, WaitsOn: []string{"s-a"}}, `specgraph: unit "u-b": it sets no serves; a unit with none sets an empty list`},
		{"no needs", Unit{ID: "u-b", Serves: []string{"s-b"}, WaitsOn: []string{"s-a"}}, `specgraph: unit "u-b": it sets no needs; a unit with none sets an empty list`},
		{"no waits_on", Unit{ID: "u-b", Serves: []string{"s-b"}, Needs: none}, `specgraph: unit "u-b": it sets no waits_on; a unit with none sets an empty list`},
		{"a statement of no letter and no digit in waits_on", Unit{ID: "u-b", Serves: []string{"s-b"}, Needs: none, WaitsOn: []string{" "}},
			`specgraph: unit "u-b": waits_on holds " ", which names no statement`},
		{"a unit of no letter and no digit in needs", Unit{ID: "u-b", Serves: []string{"s-b"}, Needs: []string{""}, WaitsOn: []string{"s-a"}},
			`specgraph: unit "u-b": needs holds "", which names no unit`},
		{"the id of the unit before it", Unit{ID: "u-a", Serves: none, Needs: none, WaitsOn: []string{"s-a"}},
			`specgraph: unit "u-a" is in the list two times`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := WaitCycle([]Unit{other, row.unit})
			switch {
			case row.want == "" && (err != nil || got == nil):
				t.Errorf("WaitCycle = %q, %v, want a cycle and no error", got, err)
			case row.want != "" && (err == nil || err.Error() != row.want || got != nil):
				t.Errorf("WaitCycle = %q, %v, want no cycle and the error %q", got, err, row.want)
			}
		})
	}
}
