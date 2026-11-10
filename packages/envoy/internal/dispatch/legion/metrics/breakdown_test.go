package metrics

import (
	"fmt"
	"math"
	"slices"
	"testing"
)

// TestThePhaseBreakdownSharesOutEachIssuesBarHours: each hour is counted once. An issue's parts add
// up to its work time less its queued time and less the hours it waited while a child worked
// (the child's own row counts those), an ask's hours are waiting on a person, and the rest go to
// the phases the status history draws, with no observation at all.
func TestThePhaseBreakdownSharesOutEachIssuesBarHours(t *testing.T) {
	root := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(10)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(6), "needs_review").
		statusAt(hour(10), "done").
		asked(hour(1), hour(2))
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(3)).closedAt(hour(5)).
		statusAt(hour(3), "in_progress").
		statusAt(hour(5), "done")
	report := Compute(fixture(hour(-24), hour(24), root, child), ScopeAll, hour(24))

	rows := map[string]BreakdownIssue{}
	for _, row := range report.PhaseBreakdown.Issues {
		rows[row.Key] = row
	}
	work := map[string]WorkTimeIssue{}
	for _, row := range report.WorkTime.Issues {
		work[row.Key] = row
	}
	for key, row := range rows {
		var sum float64
		for _, spent := range row.Phases {
			sum += spent
		}
		if want := work[key].Hours - work[key].QueuedHours - row.WaitingOnChildrenHours; math.Abs(sum-want) > 0.001 || math.Abs(row.Hours-want) > 0.001 {
			t.Errorf("%s: the breakdown holds %g h (row %g), want its bar hours %g", key, sum, row.Hours, want)
		}
	}
	got := rows["INFRA-1"].Phases
	if got[breakdownPerson] != 1 || rows["INFRA-1"].WaitingOnChildrenHours != 2 || got[breakdownChildren] != 0 {
		t.Errorf("the root waited %g h on a person and %g h on children (%g among its parts), want 1, 2 and none: %v",
			got[breakdownPerson], rows["INFRA-1"].WaitingOnChildrenHours, got[breakdownChildren], got)
	}
	if report.PhaseBreakdown.WaitingOnChildrenHours != 2 || report.PhaseBreakdown.Hours != 8+2 {
		t.Errorf("the breakdown holds %g h with %g h waiting on children apart, want 10 and 2",
			report.PhaseBreakdown.Hours, report.PhaseBreakdown.WaitingOnChildrenHours)
	}
	var shares float64
	for _, phase := range report.PhaseBreakdown.Phases {
		shares += phase.Share
	}
	if math.Abs(shares-1) > 0.001 {
		t.Errorf("the shares add up to %g, want 1", shares)
	}
}

// TestALeftOutTreeIsCountedNowhere: excluding a tree root leaves its issues out of every metric,
// and the largest tree names the tree with the most finished issues, beside the rest's median.
func TestALeftOutTreeIsCountedNowhere(t *testing.T) {
	root := newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(20))
	reviews := []*Issue{root}
	for n := 2; n <= 4; n++ {
		at := float64(n)
		reviews = append(reviews, newIssue(fmt.Sprintf("INFRA-%d", n)).under("INFRA-1").worked("planner", hour(at)).closedAt(hour(at+1)))
	}
	other := newIssue("INFRA-9").worked("implementer", hour(0)).closedAt(hour(10))
	population := fixture(hour(-24), hour(24), append(reviews, other)...)

	report := Compute(population, ScopeAll, hour(24))
	if report.LargestTree == nil || report.LargestTree.Root != "INFRA-1" || report.LargestTree.Finished != 4 {
		t.Fatalf("the largest tree is %+v, want INFRA-1 with 4 finished", report.LargestTree)
	}
	if report.LargestTree.RestMedianHours != 10 {
		t.Errorf("the rest's median is %g h, want 10", report.LargestTree.RestMedianHours)
	}

	population.ExcludeTrees = map[string]bool{"INFRA-1": true}
	left := Compute(population, ScopeAll, hour(24))
	if left.Finished != 1 || len(left.WorkTime.Issues) != 1 || left.WorkTime.Issues[0].Key != "INFRA-9" {
		t.Fatalf("with INFRA-1's tree left out, %d finished (%v), want INFRA-9 alone", left.Finished, left.WorkTime.Issues)
	}
	if len(left.PhaseBreakdown.Issues) != 1 || !slices.Equal(left.ExcludedTrees, []string{"INFRA-1"}) {
		t.Errorf("the breakdown holds %d issues and the answer names %v as left out", len(left.PhaseBreakdown.Issues), left.ExcludedTrees)
	}
}
