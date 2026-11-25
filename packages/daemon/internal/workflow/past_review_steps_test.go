package workflow

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/verdict"
)

// A task whose reviewer step comes before its tester (implementer reviewer tester) reaches
// legion-verdict success when a merge forward lands in testing after the review passed: the landing
// asks the reviewer for its verdict at the new head (past review is a review step that has passed,
// by the walk, reviewStep.passed), the reviewer's approve there is taken in testing, since its own
// step has passed, the tester's checks read yes at that head, and the writer writes legion-verdict
// success on it, so the merger merges it. Under a past-review rule of phases alone (accepting,
// retro or merging) nothing would ask the reviewer in testing and its verdict would be refused
// there (VERDICT_NOT_PAST_REVIEW), so the new head would never get its review step's verdict and
// the merger would wait for good.
func TestAMergeForwardLandingInTestingAfterTheReviewReachesLegionVerdictSuccess(t *testing.T) {
	pool := plannedConflictIssue(t, phase.Testing, settledGreen("lint"), "implementer", "reviewer", "tester")
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	landsMergeForward(t, pool)
	if got := toldWho(mergeForwardMessages(t, pool)); !slices.Contains(got, "reviewer review") {
		t.Fatalf("the landing in testing queued %v, want the reviewer asked for its verdict at %s", got, forwardHead)
	}
	github := newGitHubFake(t, pool, readsAfter(4*time.Minute), record.MergeStateBlocked)
	github.apply("reviewer-verdict", pastReviewVerdict("approve", forwardHead))
	state, id := testedFact(t, pool, "LEGION-208", forwardHead, true)
	github.apply(id, state)
	github.apply("tester", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim",
		Summary: "every check passes", Commit: forwardHead, HandoffRecord: "test-1", HandoffVersion: 1})
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("after the tester's completion LEGION-208 is in %s, want retro", got)
	}
	if got := github.verdicts[forwardHead]; got != verdict.Success {
		t.Fatalf("legion-verdict on %s = %q (all written: %v), want success", forwardHead, got, github.verdicts)
	}
	github.apply("retro", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim",
		Summary: "retro done", Commit: forwardHead})
	if got := issuePhase(t, pool); got != phase.Merging {
		t.Fatalf("after the retro's completion LEGION-208 is in %s, want merging", got)
	}
	github.merge(forwardHead)
	if got := issuePhase(t, pool); got != phase.Done {
		t.Fatalf("after the merger's merge LEGION-208 is in %s, want done", got)
	}
}
