package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The architect's ruling (as for phase-finished, dispatch://INFRA-3686/comment/f52a57a0-eb03-4d96-87af-d761dff39286):
// a row that does not show that the architect's act can wait is med for the architect.
//
// Main writes a checks-red row with no `hold` and no `head` (workflow's mergeForward at f8a5195a:
// role, phase and a reason "the head <sha> conflicts with <base> (<why>): the <role> is asked to
// merge <base> forward, and the issue stays in <phase>"), and for an issue held from a worker's
// phase the reason ends "stays held from <phase>". So a row main queued for a held issue, still
// undelivered when this release starts (its architect's pod released, the row held for its next
// session), is delivered by this release. The architect is the one who must act on a held issue,
// and this release's own row of the same event names the hold, which makes it med. Read from its
// variant alone, main's row names a worker's phase and no hold, which would read low: no delivery
// wake, so the released architect is not brought back and the held issue waits. Every row this
// release writes names the head (texts.ChecksRedRecord), and main's rows name none, so a row with
// no head is med (texts' checksRedUrgency). The issue here is held from testing, where main's row
// left it.
//
// The first case, the row this release writes for the same conflict (it names `hold` and `head`),
// is the control.
func TestAChecksRedRowAnEarlierReleaseQueuedForAHeldIssueWakesTheArchitect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notice record.Notice
	}{
		{name: "this release's row of a conflict on an issue held from testing",
			notice: urgencyNoticeOf(t, texts.ChecksRedRecord{NoticeHead: texts.NoticeHead{Kind: texts.ChecksRed, Issue: urgencyIssue},
				Phase: phase.Testing, Hold: texts.HoldFailure, ConflictsWith: "main", Head: "c0de", PullRequest: "acme/widgets#86",
				Why: string(record.ConflictCheckNotGreen), Asked: claim.RoleImplementer})},
		{name: "main's row of a conflict on an issue held from testing",
			notice: record.Notice{Kind: texts.ChecksRed, Role: claim.RoleArchitect, Phase: phase.Testing,
				Reason: "the head c0de conflicts with main (" + string(record.ConflictCheckNotGreen) + "): the implementer is asked to merge main forward, " +
					"and the issue stays held from testing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher := &outboxPublisher{}
			runner, sup, pool := runnerIn(t, phase.Held, "in_progress", publisher, quietLogger())
			putOutboxIssue(t, pool, runner.records, record.Issue{Key: urgencyIssue, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Project: "LEGION", Tree: urgencyIssue, Title: "Workflow", Phase: phase.Held,
				Hold: &record.Hold{From: phase.Testing}, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5})
			architect := releasedClaim(t, sup, claim.RoleArchitect)
			urgencyEnqueue(t, pool, runner.records, urgencyIssue, tc.notice)

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run the outbox: %v", err)
			}

			wakes := urgencyWakes(t, pool, claim.RoleArchitect)
			t.Logf("%s (urgency %s to the architect): delivery wakes %d, architect released %t",
				tc.name, noticeUrgency(tc.notice, claim.RoleArchitect), wakes, architect.Claim().Released())
			if wakes != 1 {
				t.Fatalf("delivery wakes = %d, want 1: %s is of %s, held from testing, where no worker works it, so the released architect must be "+
					"brought back to act on it, as for the row this release writes for the same conflict", wakes, tc.name, urgencyIssue)
			}
		})
	}
}
