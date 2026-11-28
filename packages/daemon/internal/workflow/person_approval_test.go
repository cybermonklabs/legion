package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// personApprovalAt is a person's approval of head - an account with write access to the
// repository, delivered by its webhook, with no Legion footer - submitted at submitted.
func personApprovalAt(head string, submitted time.Time) intake.PullRequestReview {
	return intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1, State: "approved", CommitID: head,
		Author: "a-writer", AuthorCanWrite: true, Body: "Approved by an agent on its owner's account.", SubmittedAt: submitted}
}

// reviewerCompletesAt is the reviewer's completion of its round, naming commit as the head it
// read ("" for none).
func reviewerCompletesAt(commit string) intake.HandoffComplete {
	return intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: reviewClaim,
		Summary: "reviewed", Commit: commit, HandoffRecord: "review-1", HandoffVersion: 1}
}

// A person's approval ends the review round only once the reviewer has completed the round at the
// approved head. An agent can submit an approval on a person's account, and it reads as the
// person's: had it ended the round before the reviewer completed, a head the reviewer never
// approved would go on to accepting, or to the retro and the merge where no acceptance step
// follows. So the approval waits in reviewing, and neither a clean read of the head by GitHub nor
// the pull request's merge ends the round. The reviewer's own review that follows is the newest
// deciding one, so it decides the round: its request for changes sends the work back, and its
// approval ends the round with no completion. The reviewer's completion at the approved head with
// no review of its own ends the round on the person's approval.
func TestAPersonsApprovalEndsTheRoundOnlyOnceTheReviewerCompletes(t *testing.T) {
	t.Parallel()
	submitted := time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// waiting is what comes after the approval and leaves the round waiting.
		waiting []intake.Fact
		then    intake.Fact
		want    phase.Phase
	}{
		{name: "the reviewer completes", then: reviewerCompletesAt(headOne), want: phase.Accepting},
		{name: "the reviewer requests changes", then: intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2, State: "changes_requested",
			CommitID: headOne, Author: testReviewApp, Body: reviewBody("fix the widget", headOne), SubmittedAt: submitted.Add(time.Minute)}, want: phase.Implementing},
		{name: "the reviewer approves", then: intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2, State: "approved",
			CommitID: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne), SubmittedAt: submitted.Add(time.Minute)}, want: phase.Accepting},
		{name: "the pull request merges, then the reviewer completes",
			waiting: []intake.Fact{intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge", HeadSHA: headOne}},
			then:    reviewerCompletesAt(headOne), want: phase.Accepting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, headOne, "green")
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("a person's approval", personApprovalAt(headOne, submitted))
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after a person's approval of the head the issue is in %s, want reviewing: the reviewer has not completed", got)
			}
			if got := reviewerDecision(t, pool); got == nil || got.State != "approved" || got.Head != headOne || !got.Person {
				t.Fatalf("the round's decision = %+v, want the person's approval of %s", got, headOne)
			}
			waiting := append([]intake.Fact{intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: headOne,
				Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean}}, tc.waiting...)
			for i, fact := range waiting {
				apply("a fact the round waits through", fact)
				if got := issuePhase(t, pool); got != phase.Reviewing {
					t.Fatalf("after the fact %d the round waits through (%T) the issue is in %s, want reviewing: the reviewer has not completed", i, fact, got)
				}
			}
			apply(tc.name, tc.then)
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after %s the issue is in %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// A completion counts for a person's approval only where it names the approved head: the reviewer
// read head A, a push moved the pull request to B, and a person approved B. The reviewer's own
// review of A is set aside, since its footer names a head the pull request has left, and a
// completion at A, or at no head, says the reviewer never read B. So the round stays in reviewing,
// stuck on code the reviewer has not read, and the architect is told once, with both heads. The
// reviewer's own approval of B then ends the round.
func TestAPersonsApprovalOfAHeadTheReviewerDidNotReadStaysStuck(t *testing.T) {
	t.Parallel()
	submitted := time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		commit string
		told   string
	}{
		{name: "the reviewer completes at the head before the push", commit: headOne, told: "completed its round at " + headOne},
		{name: "the reviewer completes at no head", commit: "", told: "completed its round at no head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, headOne, "green")
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("the push", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: headOne, After: headTwo, Pusher: "legion-implementer[bot]"})
			apply("the new head", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: headTwo})
			apply("a person's approval of the new head", personApprovalAt(headTwo, submitted))
			apply("the reviewer's approval of the head it read", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2, State: "approved",
				CommitID: headTwo, Author: testReviewApp, Body: reviewBody("ship it", headOne), SubmittedAt: submitted.Add(time.Minute)})
			apply("the reviewer's completion", reviewerCompletesAt(tc.commit))
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the reviewer completed at %q the issue is in %s, want reviewing: the reviewer never read %s", tc.commit, got, headTwo)
			}
			told := reviewStuckNotices(t, pool)
			if len(told) != 1 || !strings.Contains(told[0].Reason, "a person approved head "+headTwo) || !strings.Contains(told[0].Reason, tc.told) {
				t.Fatalf("review-stuck notices %+v, want one naming the person's approval of %s and %q", told, headTwo, tc.told)
			}
			apply("the reviewer's approval of the new head", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 3, State: "approved",
				CommitID: headTwo, Author: testReviewApp, Body: reviewBody("ship it", headTwo), SubmittedAt: submitted.Add(2 * time.Minute)})
			apply("GitHub's clean read of the new head", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: headTwo,
				Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
			if got := issuePhase(t, pool); got != phase.Accepting {
				t.Fatalf("after the reviewer's approval of %s the issue is in %s, want accepting", headTwo, got)
			}
		})
	}
}

// A person's approval that comes after the reviewer completed at the approved head ends the round
// at once. One of a head GitHub answers `behind` for, with the head's own checks not concluded,
// tells nobody before the completion, and the completion leaves it waiting on GitHub as any
// approval of that head waits.
func TestAPersonsApprovalAfterTheCompletionEndsTheRoundAndOneOfABehindHeadWaits(t *testing.T) {
	t.Parallel()
	submitted := time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)
	t.Run("the reviewer completed first", func(t *testing.T) {
		pool := migratedPool(t)
		seedReviewOf(t, pool, headOne, "green")
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		apply("the reviewer's completion", reviewerCompletesAt(headOne))
		apply("a person's approval", personApprovalAt(headOne, submitted))
		if got := issuePhase(t, pool); got != phase.Accepting {
			t.Fatalf("after the completion and then a person's approval the issue is in %s, want accepting", got)
		}
	})
	t.Run("GitHub answers behind while the approval waits", func(t *testing.T) {
		pool := migratedPool(t)
		seedReviewOf(t, pool, headOne, "green")
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		apply("a person's approval", personApprovalAt(headOne, submitted))
		apply("GitHub's read of the head", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: headOne,
			Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateBehind})
		if told := reviewStuckNotices(t, pool); len(told) != 0 {
			t.Fatalf("review-stuck notices %+v before the completion, want none: the approval waits for the reviewer", told)
		}
		apply("the reviewer's completion", reviewerCompletesAt(headOne))
		if got := issuePhase(t, pool); got != phase.Reviewing {
			t.Fatalf("after the completion the issue is in %s, want reviewing: GitHub answers the head behind", got)
		}
		if told := reviewStuckNotices(t, pool); len(told) != 0 {
			t.Fatalf("review-stuck notices %+v after the completion, want none: the head's own checks have not concluded, so the round waits quietly", told)
		}
	})
}
