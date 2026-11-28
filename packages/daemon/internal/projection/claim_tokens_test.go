package projection

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Two live claims hold the tester's role of an issue at testing: the agent of a key, whose claim
// the tester's phases row names while it runs the plan's step, and the tester's own claim of an
// earlier version of the plan. The state names every claim of the issue by its token: the step's
// worker under `workers.tester`, with its token, and the other claim of the role under
// `otherClaims`, with its role and its token, so a reader of the state can reach either.
func TestTheStateNamesEveryLiveClaimOfAnIssueByItsToken(t *testing.T) {
	const project = "LEGION"
	keyed, err := claim.AgentToken("legion", "LEGION-20", "implementer")
	if err != nil {
		t.Fatal(err)
	}
	own := realToken("LEGION-20", claim.RoleTester)
	store := projectionStore{
		issues: []record.Issue{{Key: "LEGION-20", Project: project, Tree: "LEGION-20", Phase: phase.Testing, Generation: 1,
			Status: "testing", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: "a"}},
		phases: map[string][]record.PhaseRow{
			"LEGION-20": {{Issue: "LEGION-20", Role: claim.RoleTester, Claim: keyed, Entry: 2, StepTask: "task-2"}},
		},
	}
	claims := []supervise.Claim{
		{Token: own, Issue: "LEGION-20", Tree: "LEGION-20", Role: claim.RoleTester, State: supervise.StateIdle, Session: "ses_own"},
		{Token: keyed, Issue: "LEGION-20", Tree: "LEGION-20", Role: claim.RoleTester, State: supervise.StateWorking, Session: "ses_keyed"},
	}
	got, err := Project(context.Background(), Inputs{Store: store, Project: project, Claims: claims, Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	view := got.Issues["LEGION-20"]
	if worker := view.Workers[claim.RoleTester].Claim; worker.Token != keyed || worker.Session != "ses_keyed" {
		t.Fatalf("workers.tester = token %q session %q, want the step's worker %s", worker.Token, worker.Session, keyed)
	}
	if len(view.OtherClaims) != 1 || view.OtherClaims[0].Role != claim.RoleTester || view.OtherClaims[0].Claim.Token != own ||
		view.OtherClaims[0].Claim.Session != "ses_own" {
		t.Fatalf("otherClaims = %+v, want the tester's own claim %s, which works no step", view.OtherClaims, own)
	}
}
