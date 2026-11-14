package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The engine starts no architect, so no phase start carries news into accepting: the move of an
// owed member into accepting is what queues the reconcile of its tree's news (record.OwedNews over
// the record the move leaves), which sends that member's news to the architect that owns it. The
// control is the same move of a member whose holder took the approved version already, which is
// owed nothing and queues none.
func TestAMoveIntoAcceptingOfAnOwedMemberQueuesTheReconcile(t *testing.T) {
	for _, tc := range []struct {
		name string
		took int
		want int
	}{
		{name: "a reviewer that took the version before the approved one", took: 3, want: 1},
		{name: "a reviewer that took the approved version", took: 4, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
			seedPhase(t, pool, record.PhaseRow{Issue: gatedRoot, Role: claim.RoleReviewer, Claim: "review-claim", HandoffRecord: "review-1",
				HandoffVersion: 1, Summary: "looks right"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: gatedRoot, Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/" + gatedRoot, HeadSHA: headOne, CheckedHead: headOne, Failing: []string{}})
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(4), EverOpened: true})
			seedTold(t, pool, gatedRoot, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: tc.took})
			seedTold(t, pool, gatedRoot, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 4})

			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
				intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne, HeadSHA: headOne,
					Author: testReviewApp, Body: reviewBody("ship it", headOne)})

			assertPhase(t, pool, phase.Accepting)
			assertOutboxCount(t, pool, "design_changes", tc.want)
		})
	}
}

// The write of a gate row that raises a step's approved version queues the reconcile of the tree's
// news, once: a raise while that reconcile is still to run queues no second beside it, since the
// reconcile reads what each holder is owed when it runs. A new version that closes the gate queues
// none, since a closed gate owes nobody anything.
func TestAGateWriteThatRaisesTheApprovedVersionQueuesOneReconcile(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(4),
		ApprovedMark: record.EventMark{Seq: 9}, EverOpened: true})
	seedTold(t, pool, gatedRoot, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 4})
	seedTold(t, pool, gatedRoot, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 4})
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "version-5", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc,
		Kind: intake.DispatchArtifactVersion, Version: 5})
	assertOutboxCount(t, pool, "design_changes", 0)

	applyMirrored(t, pool, engine, "approved-5", stepApproval(gatedRoot, gateDoc, 5, record.EventMark{Seq: 11}))
	assertOutboxCount(t, pool, "design_changes", 1)

	applyMirrored(t, pool, engine, "version-6", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc,
		Kind: intake.DispatchArtifactVersion, Version: 6})
	applyMirrored(t, pool, engine, "approved-6", stepApproval(gatedRoot, gateDoc, 6, record.EventMark{Seq: 13}))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.ApprovedVersion == nil || *gate.ApprovedVersion != 6 {
		t.Fatalf("the spec gate = %+v, want it approved at 6: the second raise this case reads never happened", gate)
	}
	assertOutboxCount(t, pool, "design_changes", 1)
}
