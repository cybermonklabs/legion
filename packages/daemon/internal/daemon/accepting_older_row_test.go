package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The architect's ruling on test handoff v17 (dispatch://INFRA-3686/comment/e6303092-ce07-478f-b717-ba1b45cc47aa):
// a phase-finished notice that moves an issue into accepting is med for the architect, whoever
// finished, since no worker runs there and the architect's sign-off is the next act.
//
// Main writes a phase-finished row with no `to` (workflow's noticeFor at f8a5195a: role, phase,
// summary and verdict, and for a worker's own move back a reason "moved back to <phase> by the
// <role>: ..."), and main's #5161 already lets an owner's plan put acceptance:owner right after the
// implementer or the tester. So a row main queued for such a move, still undelivered when this
// release starts (its architect's pod released, the row held for its next session), is delivered by
// this release. The head reads the urgency from the row's variant alone (noticeUrgency), and a
// tester's, an implementer's or a merger's row that names no `to` reads low: no delivery wake, so
// the released architect is not brought back and the issue waits in accepting. A reviewer's row
// with no `to` is med for this very reason (texts' phaseFinishedUrgency: "a reviewer's phase that
// names no phase it moved to, as a row of an older daemon does"). The issue here stands in
// accepting, where those rows left it.
//
// The first case, the row this release writes for the same move (it names `to: accepting`), is the
// control: it wakes the architect at the head, where the rows main wrote do not.
func TestAPhaseFinishedRowAnEarlierReleaseQueuedForAMoveIntoAcceptingWakesTheArchitect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notice record.Notice
	}{
		{name: "this release's row of a tester's pass into accepting",
			notice: record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing, Issue: urgencyIssue, To: phase.Accepting,
				Summary: "Every check passed.", Verdict: "pass"}},
		{name: "main's row of a tester's pass into accepting",
			notice: record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing, Summary: "Every check passed.", Verdict: "pass"}},
		{name: "main's row of an implementer's completion into accepting",
			notice: record.Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Phase: phase.Implementing, Summary: "Built it."}},
		{name: "main's row of a merger's move back into accepting",
			notice: record.Notice{Kind: "phase-finished", Role: claim.RoleMerger, Phase: phase.Merging,
				Reason: "moved back to accepting by the merger: the approval is for another head"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher := &outboxPublisher{}
			runner, sup, pool := runnerIn(t, phase.Accepting, "needs_review", publisher, quietLogger())
			architect := releasedClaim(t, sup, claim.RoleArchitect)
			urgencyEnqueue(t, pool, runner.records, urgencyIssue, tc.notice)

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run the outbox: %v", err)
			}

			wakes := urgencyWakes(t, pool, claim.RoleArchitect)
			t.Logf("%s (urgency %s to the architect): delivery wakes %d, architect released %t",
				tc.name, noticeUrgency(tc.notice, claim.RoleArchitect), wakes, architect.Claim().Released())
			if wakes != 1 {
				t.Fatalf("delivery wakes = %d, want 1: %s left %s in accepting, where no worker runs, so the released architect must be "+
					"brought back to sign off, as for the row this release writes for the same move", wakes, tc.name, urgencyIssue)
			}
		})
	}
}
