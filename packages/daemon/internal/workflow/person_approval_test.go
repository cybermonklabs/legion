package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A person's approval - from an account with write access to the repository, delivered by its
// webhook, with no Legion footer - ends the review round only once the reviewer has completed
// the round. An agent can submit an approval on a person's account, and it reads as the person's:
// had it ended the round before the reviewer completed, a head the reviewer never approved would
// go on to accepting, or to the retro and the merge where no acceptance step follows. So the
// approval waits in reviewing, and a clean read of the head by GitHub does not end the round.
// The reviewer's own review that follows is the newest deciding one, so it decides the round: its
// request for changes sends the work back, and its approval ends the round with no completion.
// The reviewer's completion with no review of its own ends the round on the person's approval.
func TestAPersonsApprovalEndsTheRoundOnlyOnceTheReviewerCompletes(t *testing.T) {
	t.Parallel()
	submitted := time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		then intake.Fact
		want phase.Phase
	}{
		{name: "the reviewer completes", then: intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: reviewClaim,
			Summary: "reviewed", HandoffRecord: "review-1", HandoffVersion: 1}, want: phase.Accepting},
		{name: "the reviewer requests changes", then: intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2, State: "changes_requested",
			CommitID: headOne, Author: testReviewApp, Body: reviewBody("fix the widget", headOne), SubmittedAt: submitted.Add(time.Minute)}, want: phase.Implementing},
		{name: "the reviewer approves", then: intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2, State: "approved",
			CommitID: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne), SubmittedAt: submitted.Add(time.Minute)}, want: phase.Accepting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, headOne, "green")
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("a person's approval", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1, State: "approved", CommitID: headOne,
				Author: "a-writer", AuthorCanWrite: true, Body: "Approved by an agent on its owner's account.", SubmittedAt: submitted})
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after a person's approval of the head the issue is in %s, want reviewing: the reviewer has not completed", got)
			}
			if got := reviewerDecision(t, pool); got == nil || got.State != "approved" || got.Head != headOne {
				t.Fatalf("the round's decision = %+v, want the person's approval of %s", got, headOne)
			}
			apply("GitHub's clean read of the head", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: headOne,
				Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after GitHub's clean read of the approved head the issue is in %s, want reviewing: the reviewer has not completed", got)
			}
			apply(tc.name, tc.then)
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after %s the issue is in %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}
