package workflow

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Main's endLook (workflow/check.go at d46ec4e0) queues the retry of a look nobody reported as a
// check_wake row whose reason is "the retry of a look nobody reported: "+cause, and its wokenBy
// tells the look that row starts as "the wake named at the last look — " + detail. Main's tester
// prompt (go/tester.md at d46ec4e0) says what that reason is: "`the wake named at the last look —
// <reason>` for a look your own `wake_at` started, the reason being the summary you sent, or for
// the retry of a look nobody reported, the reason being that retry's sentence". Main never said
// that the checking role wrote a retry's words.
//
// The branch tells such a row's look as the checking role's own wake: a row with no retry mark
// (record.CheckWakeDue.Retry) is woken_by: named, its reason in a summary block by the checking
// role, and go/tester.md and go/implementer.md now say `named` is "a look your own `wake_at`
// started, with the summary you sent in its `summary` block". Every check_wake row an earlier
// release queued has no mark, its retries among them, and such a row is still queued at the
// upgrade for any check whose look went unreported in the two hours before it (CheckLookBound),
// most of all for the checks this release leaves with the implementer.
//
// The input carries the fact: the row's reason is main's retry sentence, and the check holds no
// time of a report (record.ProductionCheck.ReportedAt, which an earlier release never wrote and
// which this release's endLook clears), while every unmarked row this release writes comes from a
// pending report, whose record keeps that report's time (checkReported).
//
// INFRA-3686's R1: a text "that carries words another author wrote ... puts them in a block of
// their own after its facts, which names the author and the time where the text's own fields do
// not already name them". The daemon's words of a retry are no summary of the checking role's,
// and the look is no wake it named.
func TestARetryWakeAnEarlierReleaseQueuedIsNotToldAsTheCheckingRolesSummary(t *testing.T) {
	rig := childCheckRig(t)
	const issue = "LEGION-209"
	ended := checkOpened.Add(CheckLookBound)
	rig.clock.now = ended
	rig.apply("look-1-bound", intake.CheckLookEnded{Issue: issue, Look: 1, Cause: "look bound", OpenedAt: checkOpened})
	if check := productionCheck(t, rig.pool, issue); check == nil || check.ReportedAt != nil {
		t.Fatalf("the check after look 1 nobody reported = %+v, want it to hold no report's time", check)
	}
	rows := checkWakeRows(t, rig.pool, issue)
	if len(rows) != 1 {
		t.Fatalf("check_wake rows after look 1 nobody reported = %d, want its retry alone", len(rows))
	}
	// The row as an earlier release queued it: the same look, check and reason, and no mark, since
	// that release wrote none.
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rows[0].payload, &payload); err != nil {
		t.Fatalf("decode check_wake row %d: %v", rows[0].id, err)
	}
	delete(payload, "retry")
	earlier, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var due record.CheckWakeDue
	if err := json.Unmarshal(earlier, &due); err != nil {
		t.Fatalf("decode the earlier release's row %s: %v", earlier, err)
	}
	if due.Look != 1 || due.Reason != "the retry of a look nobody reported: look bound" || due.Retry {
		t.Fatalf("the earlier release's row = %+v, want look 1's retry in main's words with no mark", due)
	}
	rig.clock.now = ended.Add(CheckLookBound)
	rig.apply("look-1-retry", intake.CheckWakeDue{Issue: issue, Look: due.Look, OpenedAt: due.OpenedAt, Reason: due.Reason, Retry: due.Retry})
	starts := superviseRowsOf(t, rig.pool, issue, "start")
	latest := starts[len(starts)-1]
	if latest.CheckLook != 2 || latest.Role != claim.RoleTester {
		t.Fatalf("the newest start row = %+v, want look 2 handed to the tester", latest)
	}
	read := readTaskOf(t, latest.Task)
	for _, block := range read.blocks {
		if block.name == "summary" {
			t.Errorf("the look carries the daemon's retry sentence as a summary block by %q (%+v): the checking role wrote no such words and named no such wake",
				block.attrs["by"], block)
		}
	}
	if read.fields["woken_by"] != "retry" || read.fields["retry_of"] != "look bound" {
		t.Errorf("the look an earlier release's retry row started is woken_by %q with retry_of %q, want retry, naming look bound as why",
			read.fields["woken_by"], read.fields["retry_of"])
	}
}
