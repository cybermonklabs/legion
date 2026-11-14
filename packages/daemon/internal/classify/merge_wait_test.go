package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// RoundWaitsOnMergeState is the one account of a review round that waits on GitHub's answer about
// merging its head, which the engine reads to decide whether to ask GitHub for that answer
// (workflow's reviewRound) and the state build reads to decide whether the issue's view carries a
// mergeWait object (projection's mergeWaitView). Every way it answers false is here, since a row
// that drifts is a daemon that reads where its state says it does not, or the reverse: an issue
// that is not reviewing, no pull request, no review, a request for changes, an approval that does
// not stand - of a head the branch has left, or of a head a push is already on its way from - and
// an approval that stands on a head GitHub reports one the issue may advance on, in each of the
// three spellings of that (record.MergeState.Mergeable). It is true for the stuck round, whose
// head GitHub has answered `blocked` or `behind` for, and for the round still waiting for a first
// answer, whose state is unread, alike: both wait on GitHub's next answer about the same head.
func TestARoundWaitsOnTheMergeStateOnlyWhereAnApprovalStandsOnAHeadGitHubWillNotMergeFrom(t *testing.T) {
	const issue, head = "LEGION-208", "h2"
	reviewing := record.Issue{Key: issue, Tree: issue, Phase: phase.Reviewing}
	implementing := record.Issue{Key: issue, Tree: issue, Phase: phase.Implementing}
	reviewer := func(decision *record.ReviewDecision) record.PhaseRow {
		return record.PhaseRow{Issue: issue, Role: claim.RoleReviewer, Decision: decision}
	}
	approved := func(reviewed string) record.PhaseRow {
		return reviewer(&record.ReviewDecision{State: "approved", Head: reviewed})
	}
	pullRequest := func(state record.MergeState) *record.PullRequest {
		return &record.PullRequest{Issue: issue, Number: 42, HeadSHA: head,
			State: record.PullRequestOpen, MergeState: state}
	}
	// The head a push is already on its way from: the push event reached the daemon before the
	// synchronize that moves the head, so the approval is of code on its way out (CodeOnItsWay).
	pushing := pullRequest(record.MergeStateBlocked)
	pushing.Pushes = []record.ClassifiedPush{{SHA: "h3", Before: head}}

	for _, tc := range []struct {
		name     string
		issue    record.Issue
		reviewer record.PhaseRow
		pr       *record.PullRequest
		want     bool
	}{
		{"an issue that is not reviewing", implementing, approved(head), pullRequest(record.MergeStateBlocked), false},
		{"an issue with no pull request recorded", reviewing, approved(head), nil, false},
		{"a round no review has decided", reviewing, reviewer(nil), pullRequest(record.MergeStateBlocked), false},
		{"a round a request for changes decided", reviewing, reviewer(&record.ReviewDecision{State: "changes_requested", Head: head}), pullRequest(record.MergeStateBlocked), false},
		{"an approval of a head the branch has left", reviewing, approved("h1"), pullRequest(record.MergeStateBlocked), false},
		{"an approval of a head a push is on its way from", reviewing, approved(head), pushing, false},
		{"an approval of a head GitHub reports clean", reviewing, approved(head), pullRequest(record.MergeStateClean), false},
		{"an approval of a head GitHub reports unstable", reviewing, approved(head), pullRequest(record.MergeStateUnstable), false},
		{"an approval of a head GitHub reports has_hooks", reviewing, approved(head), pullRequest(record.MergeStateHasHooks), false},
		{"an approval of a head GitHub reports blocked", reviewing, approved(head), pullRequest(record.MergeStateBlocked), true},
		{"an approval of a head GitHub reports behind", reviewing, approved(head), pullRequest(record.MergeStateBehind), true},
		{"an approval of a head GitHub has not been read for", reviewing, approved(head), pullRequest(record.MergeStateUnread), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoundWaitsOnMergeState(tc.issue, tc.reviewer, tc.pr); got != tc.want {
				t.Fatalf("RoundWaitsOnMergeState = %v, want %v: %s", got, tc.want, tc.name)
			}
		})
	}
}
