package projection

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The state's own word for the member of a tree whose root stands out of the workflow at
// triage, at phase done and under a gate a person approved: no word above `unclassified` is
// true of it, so C2's last word is what it reads (dispatch://INFRA-3107).
//
// It is the state's half of a pair, the other being internal/admit's
// TestTheTickNoticeCarriesTheHeldIssues, which records the same member and requires the same
// word of the tick. Such a root is read differently by the two: the state reads the project's
// whole record and holds the root among the pass's own issues, while the tick reads the live
// issues (record.Store.LiveIssues), which leave out a root at phase done that holds no slot,
// and meets it in the records that read leaves behind (record.ReadTreeRoots). The gates of
// those roots are read where the facts are built (record.ReadLineFacts), so the two readers
// classify over the same gates and one issue reads one word wherever it is read (C1); read over
// the roots among the pass's own issues alone, the tick had no gate for this root, and
// record.TreeGate.Open() over no rows gave its member `design_gate` where the state gives this.
func TestTheMemberOfARootOutOfTheWorkflowUnderAnOpenGateIsUnclassified(t *testing.T) {
	root := treeRoot("LEGION-200", "triage", phase.Done)
	member := treeMember("LEGION-201", "LEGION-200", "LEGION-200", "todo", phase.Admitted)
	store := projectionStore{
		issues: []record.Issue{root, member},
		gates:  map[string]map[record.DesignStep]record.DesignGate{"LEGION-200": openGate("LEGION-200", projectedAt.Add(-time.Hour))},
	}

	want := api.HeldIssue{Issue: "LEGION-201", Reason: record.HeldUnclassified}
	if got := wordFor(t, store, "LEGION-201"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the held row of %s = %#v, want %#v", "LEGION-201", got, want)
	}
}
