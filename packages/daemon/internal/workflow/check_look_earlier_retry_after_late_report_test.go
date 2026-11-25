package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// Main's endLook (workflow/check.go at d46ec4e0) queues the retry of a look nobody reported as a
// check_wake row whose reason is "the retry of a look nobody reported: "+cause, and main's tester
// prompt (go/tester.md at d46ec4e0) told that reason apart from a pending report's: "for a look
// your own `wake_at` started, the reason being the summary you sent, or for the retry of a look
// nobody reported, the reason being that retry's sentence". Main's checkReported counts a report
// of the check's current look "however that look ended — its bound came due, an operator's hold met
// its start, or the checking role's claim failed in it (dispatch://INFRA-714/spec R12)".
//
// The branch reads an unmarked row as a retry only "where its reason is word for word the sentence
// that release's endLook wrote ... and the check holds no time of a pending report" (queuedRetry),
// and tells any other unmarked row as woken_by: named, its reason in a summary block by the
// checking role at the check's ReportedAt. go/tester.md says `named` is "a look your own `wake_at`
// started, with the summary you sent in its `summary` block". But a late pending report of the look
// an earlier release ended with no report sets ReportedAt (checkReported) while that release's
// unmarked retry row is still queued, and the retry comes due first where the report names a later
// wake. The input carries the fact: the row was queued by the look's end, before the report the
// check records, and the report queued a row of its own at its own wake_at.
//
// INFRA-3686's R1: a text "that carries words another author wrote ... puts them in a block of
// their own after its facts, which names the author and the time where the text's own fields do
// not already name them". The daemon's words of a retry are no summary of the checking role's,
// written at no report's time, and the look is no wake it named.
func TestARetryRowAnEarlierReleaseQueuedStaysARetryAfterALateReportOfItsLook(t *testing.T) {
	rig := childCheckRig(t)
	const issue = "LEGION-209"
	ended := checkOpened.Add(CheckLookBound)
	rig.clock.now = ended
	rig.apply("look-1-bound", intake.CheckLookEnded{Issue: issue, Look: 1, Cause: "look bound", OpenedAt: checkOpened})
	rows := checkWakeRows(t, rig.pool, issue)
	if len(rows) != 1 {
		t.Fatalf("check_wake rows after look 1 nobody reported = %d, want its retry alone", len(rows))
	}
	// The row as an earlier release queued it: the same look, check, reason and due time, and no
	// retry mark, since that release wrote none.
	if _, err := rig.pool.Exec(t.Context(), `update outbox set payload = payload - 'retry' where id = $1`, rows[0].id); err != nil {
		t.Fatalf("drop the retry mark of row %d: %v", rows[0].id, err)
	}
	earlier := decodeWake(t, checkWakeRows(t, rig.pool, issue)[0])
	if earlier.Look != 1 || earlier.Reason != "the retry of a look nobody reported: look bound" || earlier.Retry {
		t.Fatalf("the earlier release's row = %+v, want look 1's retry in main's words with no mark", earlier)
	}

	// The tester's report of look 1 lands after its bound ended it, as the checking agent's report
	// of the current look counts however that look ended, and names a wake past the retry's.
	reported := ended.Add(5 * time.Minute)
	wake := ended.Add(CheckLookBound + time.Hour)
	rig.clock.now = reported
	if result := rig.apply("late-pending", intake.CheckReport{Issue: issue, Look: 1, Verdict: "pending",
		Summary: "the deploy has not carried the merge", WakeAt: &wake}); result.Refusal != nil {
		t.Fatalf("the late pending report = %+v, want it recorded", result.Refusal)
	}
	if rows := checkWakeRows(t, rig.pool, issue); len(rows) != 2 || !rows[0].nextAt.Before(rows[1].nextAt) {
		t.Fatalf("check_wake rows after the late report = %+v, want the earlier release's retry due before the report's own wake", rows)
	}

	rig.clock.now = ended.Add(CheckLookBound)
	rig.apply("look-1-retry", intake.CheckWakeDue{Issue: issue, Look: earlier.Look, OpenedAt: earlier.OpenedAt, Reason: earlier.Reason, Retry: earlier.Retry})
	starts := superviseRowsOf(t, rig.pool, issue, "start")
	latest := starts[len(starts)-1]
	if latest.CheckLook != 2 || latest.Role != claim.RoleTester {
		t.Fatalf("the newest start row = %+v, want look 2 handed to the tester", latest)
	}
	read := readTaskOf(t, latest.Task)
	for _, block := range read.blocks {
		if block.name == "summary" {
			t.Errorf("the look the earlier release's retry started carries the daemon's retry sentence as a summary block by %q at %q (%+v): "+
				"the tester wrote no such words at that report and named a wake at %s, not this one",
				block.attrs["by"], block.attrs["at"], block, wake.Format(time.RFC3339))
		}
	}
	if read.fields["woken_by"] != "retry" || read.fields["retry_of"] != "look bound" {
		t.Errorf("the look an earlier release's retry row started is woken_by %q with retry_of %q, want retry, naming look bound as why",
			read.fields["woken_by"], read.fields["retry_of"])
	}
}
