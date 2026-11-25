package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The look a pending report's wake starts carries the report's summary in a block by the checking
// role, and the block names when the daemon recorded that report (record.ProductionCheck
// ReportedAt), so a session the look reaches fresh reads when the words it was left were written
// (dispatch://INFRA-3686/spec R1). Each report is driven as the daemon drives it: the report's fact,
// then the check_wake row it wrote, read back and applied as the outbox applies it. A look nobody
// reported names its retry and no summary, and the record keeps no report's time for it; the next
// pending report's look names that report's own time, never an earlier one.
func TestANamedLooksSummaryBlockNamesWhenThePendingReportWasRecorded(t *testing.T) {
	rig := childCheckRig(t)
	const issue = "LEGION-209"
	// newestWake is the check_wake row the latest end of a look wrote: no outbox runs here, so the
	// rows of the looks before it stand too.
	newestWake := func() record.CheckWakeDue {
		t.Helper()
		rows := checkWakeRows(t, rig.pool, issue)
		return decodeWake(t, rows[len(rows)-1])
	}
	// startNamedLook records a pending report of look at reported, naming its wake three hours on,
	// applies the check_wake row that report wrote at that wake, and gives the next look's task.
	startNamedLook := func(look int, reported time.Time, summary string) string {
		t.Helper()
		wake := reported.Add(3 * time.Hour)
		rig.clock.now = reported
		rig.apply("pending-"+summary, intake.CheckReport{Issue: issue, Look: look, Verdict: "pending", Summary: summary, WakeAt: &wake})
		due := newestWake()
		if due.Look != look || due.Reason != summary {
			t.Fatalf("the check_wake row of look %d's pending = %+v, want that look with the report's summary as its reason", look, due)
		}
		rig.clock.now = wake
		rig.apply("wake-"+summary, intake.CheckWakeDue{Issue: issue, Look: due.Look, OpenedAt: due.OpenedAt, Reason: due.Reason, Retry: due.Retry, Pending: due.Pending})
		starts := superviseRowsOf(t, rig.pool, issue, "start")
		if latest := starts[len(starts)-1]; latest.CheckLook != look+1 || latest.Role != claim.RoleTester {
			t.Fatalf("the newest start row = %+v, want look %d handed to the tester", latest, look+1)
		}
		return starts[len(starts)-1].Task
	}
	wantBlock := func(task, summary string, reported time.Time) {
		t.Helper()
		read := readTaskOf(t, task)
		block := read.block(t, "summary")
		at := reported.UTC().Format(time.RFC3339Nano)
		if read.fields["woken_by"] != "named" || block.text != summary || block.attrs["by"] != string(claim.RoleTester) || block.attrs["at"] != at {
			t.Fatalf("the named look's task is woken_by %q with the summary block %+v, want named, with %q by the tester at %s, when the daemon recorded the report",
				read.fields["woken_by"], block, summary, at)
		}
	}

	// Look 1's pending report, 25 minutes after the merge, apart from the open and the wake.
	first := checkOpened.Add(25 * time.Minute)
	wantBlock(startNamedLook(1, first, "the deploy has not carried the merge"), "the deploy has not carried the merge", first)

	// Look 2 ends with no report at its bound: its retry names no report, so no time is kept.
	ended := rig.clock.now.Add(CheckLookBound)
	rig.clock.now = ended
	rig.apply("look-2-bound", intake.CheckLookEnded{Issue: issue, Look: 2, Cause: "look bound", OpenedAt: checkOpened})
	if check := productionCheck(t, rig.pool, issue); check.ReportedAt != nil {
		t.Fatalf("the check after look 2 nobody reported keeps the report time %s, want none: no report named its retry", check.ReportedAt)
	}
	retry := newestWake()
	if retry.Look != 2 || retry.Reason != retryOfLook+"look bound" {
		t.Fatalf("the check_wake row of look 2 nobody reported = %+v, want look 2's retry", retry)
	}
	rig.clock.now = ended.Add(CheckLookBound)
	rig.apply("look-2-retry", intake.CheckWakeDue{Issue: issue, Look: retry.Look, OpenedAt: retry.OpenedAt, Reason: retry.Reason, Retry: retry.Retry, Pending: retry.Pending})
	starts := superviseRowsOf(t, rig.pool, issue, "start")
	if read := readTaskOf(t, starts[len(starts)-1].Task); read.fields["woken_by"] != "retry" || len(read.blocks) != 0 {
		t.Fatalf("look 3's task is woken_by %q with the blocks %+v, want the retry and no summary block", read.fields["woken_by"], read.blocks)
	}

	// Look 3's pending report names its own time, not look 1's.
	third := rig.clock.now.Add(40 * time.Minute)
	wantBlock(startNamedLook(3, third, "the deploy carried the merge; the flag is still off"), "the deploy carried the merge; the flag is still off", third)
}
