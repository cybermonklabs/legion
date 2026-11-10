package metrics

import (
	"math"
	"testing"
)

// TestThePhaseBreakdownSharesOutEachIssuesBarHours: an issue's breakdown adds up to its work time
// less its queued time, an ask's hours are waiting on a person, a child's working hours are its
// root's waiting on children, and the rest go to the phases the status history draws, with no
// observation at all.
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
		if want := work[key].Hours - work[key].QueuedHours; math.Abs(sum-want) > 0.001 || math.Abs(row.Hours-want) > 0.001 {
			t.Errorf("%s: the breakdown holds %g h (row %g), want its bar hours %g", key, sum, row.Hours, want)
		}
	}
	got := rows["INFRA-1"].Phases
	if got[breakdownPerson] != 1 || got[breakdownChildren] != 2 {
		t.Errorf("the root waited %g h on a person and %g h on children, want 1 and 2: %v", got[breakdownPerson], got[breakdownChildren], got)
	}
	var shares float64
	for _, phase := range report.PhaseBreakdown.Phases {
		shares += phase.Share
	}
	if math.Abs(shares-1) > 0.001 {
		t.Errorf("the shares add up to %g, want 1", shares)
	}
}
