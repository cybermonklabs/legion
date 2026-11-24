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
// not stand - of a head the branch has left, or of a head a push is already on its way from - an
// approval that stands on a head GitHub reports one the issue may advance on, in each of the
// three spellings of that (record.MergeState.Mergeable), and an approval of the head of a pull
// request that is not open: a merged one, whose state GitHub answers `unknown` for good, and a
// closed one, whose recorded `blocked` waits on nothing until a reopen. Nor does it hold for an
// approval of a head the record holds in a confirmed conflict with its base: that approval ends
// its round whatever GitHub's state (dispatch://INFRA-4374 R0). It is true for the stuck round,
// whose head GitHub has answered `blocked` or `behind` for, for the round still waiting for a
// first answer, whose state is unread, and for a `dirty` head whose first conflicting read waits
// for its second, alike: each waits on GitHub's next answer about the same head.
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
	merged := pullRequest(record.MergeStateUnknown)
	merged.State = record.PullRequestMerged
	closed := pullRequest(record.MergeStateBlocked)
	closed.State = record.PullRequestClosed
	// The head GitHub answered `dirty` for twice, a minute apart: the record holds it conflicting.
	conflicting := pullRequest(record.MergeStateDirty)
	conflicting.ConflictingHead = head
	// The head GitHub answered `dirty` for once: the first read waits for its second.
	firstRead := pullRequest(record.MergeStateDirty)
	firstRead.ConflictRead = &record.ConflictRead{Head: head, Base: "main"}

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
		{"an approval of the head of a merged pull request", reviewing, approved(head), merged, false},
		{"an approval of the head of a closed pull request", reviewing, approved(head), closed, false},
		{"an approval of a head held in a confirmed conflict", reviewing, approved(head), conflicting, false},
		{"an approval of a dirty head whose first conflicting read waits for its second", reviewing, approved(head), firstRead, true},
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

// A push to no commit (record.NoCommit) is GitHub's deletion of the branch. It names no code, so it
// is never on its way (CodeOnItsWay), whether the record holds the pull request open or merged, and
// the path does not run through it to a push that makes the branch again. A release before the
// deletion guard stored the deletion as it stored any push. With nothing else on its way, an
// approval of the head stands (ApprovalStands), and an approval of another head is set aside
// (ApprovalSetAside), since nothing more is coming. A real push from the head is on its way in
// either state, beside a deletion or not. A merged record holds one only where its merge did not
// name the head, and that push can be the code that merged. While a push is on its way no approval
// stands, and none is set aside, since the push's new head decides the round.
func TestAPushToNoCommitIsNeverCodeOnItsWay(t *testing.T) {
	const head, other = "h2", "h1"
	deletion := record.ClassifiedPush{SHA: record.NoCommit, Before: head, Sender: "author"}
	madeAgain := record.ClassifiedPush{SHA: "h4", Before: record.NoCommit, Sender: "author"}
	push := record.ClassifiedPush{SHA: "h3", Before: head, Sender: "author"}
	for _, tc := range []struct {
		name     string
		state    record.PullRequestState
		pushes   []record.ClassifiedPush
		onItsWay bool
	}{
		{"an open pull request holding the deletion of its branch", record.PullRequestOpen, []record.ClassifiedPush{deletion}, false},
		{"a merged pull request holding the deletion of its branch", record.PullRequestMerged, []record.ClassifiedPush{deletion}, false},
		{"a merged pull request holding its branch's deletion and the branch made again", record.PullRequestMerged,
			[]record.ClassifiedPush{deletion, madeAgain}, false},
		{"an open pull request holding a push from its head", record.PullRequestOpen, []record.ClassifiedPush{push}, true},
		{"a merged pull request at a head its merge did not name, holding a push from it", record.PullRequestMerged,
			[]record.ClassifiedPush{push}, true},
		{"a merged pull request holding the deletion of its branch and a push from its head", record.PullRequestMerged,
			[]record.ClassifiedPush{deletion, push}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := record.PullRequest{Issue: "LEGION-208", Number: 42, HeadSHA: head, State: tc.state, Pushes: tc.pushes}
			if got := CodeOnItsWay(pr); got != tc.onItsWay {
				t.Fatalf("CodeOnItsWay = %v, want %v", got, tc.onItsWay)
			}
			if got := ApprovalStands(pr, head); got != !tc.onItsWay {
				t.Fatalf("ApprovalStands of the head = %v, want %v", got, !tc.onItsWay)
			}
			if got := ApprovalSetAside(pr, head); got {
				t.Fatal("ApprovalSetAside of the head = true, want false: the approval stands or waits on the push")
			}
			if got := ApprovalSetAside(pr, other); got != !tc.onItsWay {
				t.Fatalf("ApprovalSetAside of another head = %v, want %v", got, !tc.onItsWay)
			}
		})
	}
}
