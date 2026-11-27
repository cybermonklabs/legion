package record

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The line and the held list read the pass's chain only over the very records it was built over
// and the same records above them (passOver): a copy of the records, a part of them, facts with
// other records above or none, and facts no pass read each get a chain of their own, built as the
// pass builds one, so no walk reads a chain of records it was not handed.
func TestTheLineReadsThePassChainOnlyOverItsOwnRecords(t *testing.T) {
	root := chainIssue("LEGION-900", "LEGION-900", "", phase.Admitted, "in_progress")
	child := chainIssue("LEGION-903", "LEGION-900", "LEGION-902", phase.Admitted, "todo")
	doneParent := chainIssue("LEGION-902", "LEGION-900", "LEGION-900", phase.Done, "done")
	issues := []Issue{root, child}
	above := map[string]Issue{doneParent.Key: doneParent}
	built := &passChain{issues: issues, above: above}
	facts := LineFacts{Parents: above, pass: built}
	if passOver(issues, facts) != built {
		t.Fatalf("the line over the records the pass read and the same records above them walks a chain of its own, want the pass's")
	}
	none, other, unread := facts, facts, LineFacts{Parents: above}
	none.Parents = nil
	other.Parents = map[string]Issue{"LEGION-999": chainIssue("LEGION-999", "LEGION-999", "", phase.Done, "done")}
	for _, tc := range []struct {
		name   string
		issues []Issue
		facts  LineFacts
	}{
		{"a copy of the records", slices.Clone(issues), facts},
		{"a part of the records", issues[:1], facts},
		{"no records above", issues, none},
		{"as many other records above", issues, other},
		{"facts no pass read", issues, unread},
	} {
		got := passOver(tc.issues, tc.facts)
		if got == built {
			t.Fatalf("%s: the line walks the pass's chain, want one of its own", tc.name)
		}
		if want := chainOf(byKey(tc.issues), tc.facts.Parents); !reflect.DeepEqual(got.chain(), want) {
			t.Fatalf("%s: the line's own chain holds %v, want %v", tc.name, slices.Sorted(maps.Keys(got.chain())), slices.Sorted(maps.Keys(want)))
		}
	}
}
