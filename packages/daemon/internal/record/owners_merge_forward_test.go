package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The record holds a merge forward asked of the owner that has not ended while the issue is in the
// workflow and held by nothing (Issue.ActiveInWorkflow), its pull request is open and holds an ask
// that has not landed (PullRequest.MergeForwardOpen), and the ask reached the owner's claim
// (MergeForward.Agent), which reads no phases row, so no role name of the implementation step
// decides it. Each end of the ask answers false: the landing, the clear of the ask, the close and
// the merge of the pull request, no pull request, a hold, a stopped task and the issue's leave. The
// controls: an ask of a fresh implementer's claim, and one that reached no claim.
func TestTheRecordHoldsAMergeForwardAskedOfTheOwnerUntilItEnds(t *testing.T) {
	t.Parallel()
	issue := issueFixture("LEGION-330")
	issue.Phase = phase.Reviewing
	owner, err := OwnerClaim(issue)
	if err != nil {
		t.Fatalf("the owner of %s: %v", issue.Key, err)
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		t.Fatalf("the project of %s: %v", issue.Key, err)
	}
	fresh, err := claim.NewToken(project, issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("the implementer of %s: %v", issue.Key, err)
	}
	askedOf := func(agent claim.Token) *PullRequest {
		return &PullRequest{Issue: issue.Key, State: PullRequestOpen, HeadSHA: "c0de", MergeForward: &MergeForward{Head: "c0de", Base: "main",
			Phase: phase.Reviewing, Agent: agent, Why: ConflictCheckNotGreen}}
	}
	landed, cleared, closed, merged := askedOf(owner), askedOf(owner), askedOf(owner), askedOf(owner)
	landed.MergeForward.Landed = "d00d"
	cleared.MergeForward = nil
	closed.State = PullRequestClosed
	merged.State = PullRequestMerged
	heldIssue, stopped, left := issue, issue, issue
	heldIssue.Phase, heldIssue.Hold = phase.Held, &Hold{From: phase.Reviewing, Reason: HoldEscalated}
	stopped.Phase, stopped.Hold = phase.Held, &Hold{From: phase.Reviewing, Reason: HoldTaskStopped}
	left.Phase, left.Status = phase.Done, "done"
	for _, tc := range []struct {
		name  string
		issue Issue
		pr    *PullRequest
		want  bool
	}{
		{name: "asked of the owner, open", issue: issue, pr: askedOf(owner), want: true},
		{name: "landed", issue: issue, pr: landed},
		{name: "cleared", issue: issue, pr: cleared},
		{name: "the pull request closed", issue: issue, pr: closed},
		{name: "the pull request merged", issue: issue, pr: merged},
		{name: "no pull request", issue: issue},
		{name: "held", issue: heldIssue, pr: askedOf(owner)},
		{name: "its task stopped", issue: stopped, pr: askedOf(owner)},
		{name: "out of the workflow", issue: left, pr: askedOf(owner)},
		{name: "control: asked of a fresh implementer", issue: issue, pr: askedOf(fresh)},
		{name: "control: asked of no claim", issue: issue, pr: askedOf("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := OwnersMergeForward(tc.issue, tc.pr); got != tc.want {
				t.Fatalf("OwnersMergeForward(%s) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}
