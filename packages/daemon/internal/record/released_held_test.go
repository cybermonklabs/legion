package record

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// A started issue that gave its slot back to wait on another issue stands in the waiting line
// (LineWaitingOnIssue) and takes no held row, neither from the pass that built the line
// (Waiting's second answer) nor from the assembly (HeldList). Two such issues stand here, one at
// a working phase and one at admitted, because the line's last loop reaches both and must pass
// over each before any of its skips reports it. A member at a working phase that holds no slot,
// carries no mark and owes no start is the control: the daemon has not started it by any record,
// so it is no member of the line and the held list names it.
func TestAnIssueReleasedForAWaitTakesNoHeldRow(t *testing.T) {
	released := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	approved := 1
	working := linePartIssue("LEGION-12", "LEGION-10", "in_progress", phase.Implementing, "O")
	working.ReleasedForWaitAt = &released
	admitted := linePartIssue("LEGION-13", "LEGION-10", "in_progress", phase.Admitted, "P")
	admitted.ReleasedForWaitAt = &released
	control := linePartIssue("LEGION-14", "LEGION-10", "in_progress", phase.Implementing, "Q")
	issues := []Issue{
		linePartIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"),
		working, admitted, control,
	}
	facts := LineFacts{
		Lifecycles: map[string]treelifecycle.Lifecycle{"LEGION-10": {Tree: "LEGION-10", Authority: treelifecycle.AuthorityWorkflow}},
		Gates: map[string]TreeGate{"LEGION-10": {Rows: map[DesignStep]DesignGate{DesignStepSpec: {
			Issue: "LEGION-10", Step: DesignStepSpec, LatestVersion: 1, ApprovedVersion: &approved,
			ApprovedMark: EventMark{Seq: 7, At: released.Add(-time.Hour)}, EverOpened: true}}}},
		Waits: map[string][]string{"LEGION-12": {"LEGION-300"}, "LEGION-13": {"LEGION-301"}},
	}
	slots := []Slot{{Issue: "LEGION-10", Index: 0, AdmittedAt: released.Add(-2 * time.Hour)}}

	line, dropped := Waiting(issues, slots, facts)
	for _, key := range []string{"LEGION-12", "LEGION-13"} {
		var entry *LineEntry
		for i := range line {
			if line[i].Issue.Key == key {
				entry = &line[i]
			}
		}
		if entry == nil || entry.Kind != LineWaitingOnIssue {
			t.Fatalf("the line holds %s as %+v, want a waiting_on_issue entry", key, entry)
		}
		for _, row := range dropped {
			if row.Issue == key {
				t.Errorf("the pass reported %s as %+v, want no row: it placed that issue in the line", key, row)
			}
		}
	}

	held := HeldList(issues, slots, line, dropped, facts)
	if len(held) != 1 || held[0].Issue != "LEGION-14" {
		t.Fatalf("the held list = %+v, want the control LEGION-14 alone: an issue the line carries takes no row", held)
	}
}
