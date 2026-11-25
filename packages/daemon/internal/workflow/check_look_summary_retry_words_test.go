package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// Main's wokenBy (workflow/check.go at d46ec4e0) told the look that a pending report's wake
// started what woke it with that report's summary, whatever its words: `case wakeNamed: return
// "the wake named at the last look — " + detail`. The branch tells the two wakes a check_wake row
// can carry apart by the first words of its reason (CheckLook: strings.CutPrefix(detail,
// retryOfLook)), so a summary of the checking role that opens with "the retry of a look nobody
// reported: " is taken for a retry the daemon named: the look's task says `woken_by: retry` and
// gives the rest of the role's words as `retry_of`, a field, with no block, no author and no
// time. The checking role's prompt reads those words as the daemon's (go/tester.md: "`named` for
// a look your own `wake_at` started, with the summary you sent in its `summary` block, `retry`
// for the retry of a look nobody reported, with `retry_of` naming why").
//
// INFRA-3686's R1: "A daemon text … that carries words another author wrote (a person, another
// session, a worker's summary) puts them in a block of their own after its facts, which names the
// author and the time where the text's own fields do not already name them". The input carries
// every fact asked for: the report's verdict is pending, it names its own wake, its summary is the
// checking role's, and the daemon records when it took it (record.ProductionCheck.ReportedAt).
func TestAPendingSummaryThatOpensWithTheRetryWordsStaysTheCheckingRolesSummary(t *testing.T) {
	rig := childCheckRig(t)
	const issue = "LEGION-209"
	summary := "the retry of a look nobody reported: look bound came before the deploy finished; production now carries the merge and the flag is still off"
	reported := checkOpened.Add(25 * time.Minute)
	wake := reported.Add(3 * time.Hour)
	rig.clock.now = reported
	rig.apply("pending", intake.CheckReport{Issue: issue, Look: 1, Verdict: "pending", Summary: summary, WakeAt: &wake})
	rows := checkWakeRows(t, rig.pool, issue)
	if len(rows) != 1 {
		t.Fatalf("check_wake rows after look 1's pending = %d, want the one that report named", len(rows))
	}
	due := decodeWake(t, rows[0])
	rig.clock.now = wake
	rig.apply("wake", intake.CheckWakeDue{Issue: issue, Look: due.Look, OpenedAt: due.OpenedAt, Reason: due.Reason})
	starts := superviseRowsOf(t, rig.pool, issue, "start")
	latest := starts[len(starts)-1]
	if latest.CheckLook != 2 || latest.Role != claim.RoleTester {
		t.Fatalf("the newest start row = %+v, want look 2 handed to the tester", latest)
	}
	read := readTaskOf(t, latest.Task)
	if read.fields["woken_by"] != "named" || read.fields["retry_of"] != "" {
		t.Errorf("the look the tester's own wake_at started says woken_by: %q and retry_of: %q, want named and no retry_of: the daemon named no retry, the tester's pending report named the wake",
			read.fields["woken_by"], read.fields["retry_of"])
	}
	at := reported.UTC().Format(time.RFC3339Nano)
	found := false
	for _, block := range read.blocks {
		if block.name != "summary" {
			continue
		}
		found = true
		if block.text != summary || block.attrs["by"] != string(claim.RoleTester) || block.attrs["at"] != at {
			t.Errorf("the summary block = %+v, want the tester's summary %q by the tester at %s", block, summary, at)
		}
	}
	if !found {
		t.Errorf("the look carries the tester's summary in no block (blocks %+v): its words are a field with no author and no time, want them in a summary block by the tester at %s", read.blocks, at)
	}
}
