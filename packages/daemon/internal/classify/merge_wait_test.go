package classify

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// RoundWaitsOnMergeState is the one account of a review round that waits on GitHub's answer about
// merging its head, which the engine reads to decide whether to ask GitHub for that answer
// (workflow's reviewRound) and the state build reads to decide whether the issue's view carries a
// mergeWait object (projection's mergeWaitView). Every way it answers false is here, since a row
// that drifts is a daemon that reads where its state says it does not, or the reverse. At a review
// step the owner does it answers false, since the engine settles no round there (workflow's
// settleRound).
//
// The fixture is an answer that ends a round: MERGEABLE, every check run of the head concluded, no
// check running, no review requested. Each row that waits changes one thing of it, so no row can
// pass on a part the fixture leaves out; each row that is false for a reason of the round, not of
// the answer, is built on a `behind` head, which would wait, so the reason it names is the one
// that holds.
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
	none := 0
	pullRequest := func(state record.MergeState) *record.PullRequest {
		return &record.PullRequest{Issue: issue, Number: 42, HeadSHA: head, State: record.PullRequestOpen, MergeState: state,
			Mergeability: record.MergeabilityMergeable, ReviewsRequested: &none, HeadChecks: record.HeadChecksConcluded,
			CheckRuns: []record.AttemptRun{{Name: "unit", ID: 2, Head: head, Conclusion: record.RunSucceeded}}}
	}
	with := func(state record.MergeState, change func(*record.PullRequest)) *record.PullRequest {
		pr := pullRequest(state)
		change(pr)
		return pr
	}
	one := 1
	for _, tc := range []struct {
		name     string
		issue    record.Issue
		reviewer record.PhaseRow
		pr       *record.PullRequest
		want     bool
	}{
		{"an issue that is not reviewing", implementing, approved(head), pullRequest(record.MergeStateBehind), false},
		{"an issue with no pull request recorded", reviewing, approved(head), nil, false},
		{"a round no review has decided", reviewing, reviewer(nil), pullRequest(record.MergeStateBehind), false},
		{"a round a request for changes decided", reviewing, reviewer(&record.ReviewDecision{State: "changes_requested", Head: head}), pullRequest(record.MergeStateBehind), false},
		{"an approval of a head the branch has left", reviewing, approved("h1"), pullRequest(record.MergeStateBehind), false},
		// The push event reached the daemon before the synchronize that moves the head, so the
		// approval is of code on its way out (CodeOnItsWay).
		{"an approval of a head a push is on its way from", reviewing, approved(head),
			with(record.MergeStateBehind, func(pr *record.PullRequest) { pr.Pushes = []record.ClassifiedPush{{SHA: "h3", Before: head}} }), false},
		{"an approval of a head GitHub reports clean", reviewing, approved(head), pullRequest(record.MergeStateClean), false},
		{"an approval of a head GitHub reports unstable", reviewing, approved(head), pullRequest(record.MergeStateUnstable), false},
		{"an approval of a head GitHub reports has_hooks", reviewing, approved(head), pullRequest(record.MergeStateHasHooks), false},
		{"an approval of the head of a merged pull request", reviewing, approved(head),
			with(record.MergeStateUnknown, func(pr *record.PullRequest) { pr.State = record.PullRequestMerged }), false},
		{"an approval of the head of a closed pull request", reviewing, approved(head),
			with(record.MergeStateBehind, func(pr *record.PullRequest) { pr.State = record.PullRequestClosed }), false},
		// GitHub answered `dirty` twice, a minute apart: the record holds the head conflicting.
		{"an approval of a head held in a confirmed conflict", reviewing, approved(head),
			with(record.MergeStateDirty, func(pr *record.PullRequest) { pr.ConflictingHead = head }), false},
		{"an approval of a dirty head whose first conflicting read waits for its second", reviewing, approved(head),
			with(record.MergeStateDirty, func(pr *record.PullRequest) { pr.ConflictRead = &record.ConflictRead{Head: head, Base: "main"} }), true},
		// What holds the head is a rule the daemon cannot read past; the merger's enqueue answers it.
		{"an approval of a blocked head that is MERGEABLE, its checks concluded and no review asked for", reviewing, approved(head), pullRequest(record.MergeStateBlocked), false},
		{"an approval of a blocked head whose first conflicting read waits", reviewing, approved(head),
			with(record.MergeStateBlocked, func(pr *record.PullRequest) { pr.ConflictRead = &record.ConflictRead{Head: head, Base: "main"} }), true},
		{"an approval of a blocked head whose mergeability GitHub has not computed", reviewing, approved(head),
			with(record.MergeStateBlocked, func(pr *record.PullRequest) { pr.Mergeability = record.MergeabilityUnknown }), true},
		{"an approval of a blocked head on which GitHub holds a review request open", reviewing, approved(head),
			with(record.MergeStateBlocked, func(pr *record.PullRequest) { pr.ReviewsRequested = &one }), true},
		{"an approval of a blocked head no read has counted the review requests of", reviewing, approved(head),
			with(record.MergeStateBlocked, func(pr *record.PullRequest) { pr.ReviewsRequested = nil }), true},
		{"an approval of a blocked head a check of which the read found running", reviewing, approved(head),
			with(record.MergeStateBlocked, func(pr *record.PullRequest) { pr.HeadChecks = record.HeadChecksRunning }), true},
		{"an approval of a blocked head no read has given the rollup of", reviewing, approved(head),
			with(record.MergeStateBlocked, func(pr *record.PullRequest) { pr.HeadChecks = record.HeadChecksUnread }), true},
		{"an approval of a blocked head whose only check run is of the head before it", reviewing, approved(head),
			with(record.MergeStateBlocked, func(pr *record.PullRequest) { pr.CheckRuns[0].Head = "h1" }), true},
		{"an approval of a blocked head GitHub reported no check run on", reviewing, approved(head),
			with(record.MergeStateBlocked, func(pr *record.PullRequest) { pr.CheckRuns = nil }), true},
		{"an approval of a head GitHub reports behind", reviewing, approved(head), pullRequest(record.MergeStateBehind), true},
		{"an approval of a head GitHub reports draft", reviewing, approved(head), pullRequest(record.MergeStateDraft), true},
		{"an approval of a head GitHub has not been read for", reviewing, approved(head), pullRequest(record.MergeStateUnread), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoundWaitsOnMergeState(tc.issue, tc.reviewer, tc.pr); got != tc.want {
				t.Fatalf("RoundWaitsOnMergeState = %v, want %v: %s", got, tc.want, tc.name)
			}
		})
	}
}

// A review step the owner of the task does (`reviewer:owner`, where the claim that works the place
// is the architect: record.Issue.OwnersPlace) has no round for GitHub's merge state to settle: it
// ends by the owner's own report, and workflow's reviewRound answers no round there. A qualifying
// review on GitHub still writes its decision on the reviewer's row, so an approval can stand on a
// head GitHub will not merge from; the round waits on no answer all the same, so the state shows no
// mergeWait for it and the engine owes no read of GitHub's merge state for it. The same approval at
// a review step another agent does, with plan rows or with none, waits as it always has.
func TestNoRoundWaitsOnTheMergeStateAtAReviewStepTheOwnerDoes(t *testing.T) {
	const issue, head = "LEGION-208", "h2"
	approval := record.PhaseRow{Issue: issue, Role: claim.RoleReviewer,
		Decision: &record.ReviewDecision{State: "approved", Head: head}}
	blocked := &record.PullRequest{Issue: issue, Number: 42, HeadSHA: head,
		State: record.PullRequestOpen, MergeState: record.MergeStateBlocked}
	reviewing := func(placeRole claim.Role) record.Issue {
		return record.Issue{Key: issue, Tree: issue, Phase: phase.Reviewing, StepRole: claim.RoleReviewer, PlaceRole: placeRole}
	}
	for _, tc := range []struct {
		name  string
		issue record.Issue
		want  bool
	}{
		{"reviewer:owner", reviewing(claim.RoleArchitect), false},
		{"reviewer with no key", reviewing(claim.RoleReviewer), true},
		{"reviewing with no plan rows", record.Issue{Key: issue, Tree: issue, Phase: phase.Reviewing}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoundWaitsOnMergeState(tc.issue, approval, blocked); got != tc.want {
				t.Fatalf("RoundWaitsOnMergeState at %s with an approval of a blocked head = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// MergeOutcomeOf tells the round that waits quietly from the one whose architect and reviewer are
// told, and names what holds a told head: a head with no answer the round can decide on, or a
// check of it still running, waits quietly, for the read or the settlement that is on its way; a
// head whose own checks are over and that only a push, a person or GitHub's own computation will
// clear is told of. A `blocked` or `unknown` head is judged on the evidence the read that recorded
// its state wrote, so an older read's evidence - a rollback's leftover - is no evidence. GitHub's
// `mergeable` null is told once a null read of the run comes at least NullMergeableFloor after the
// run's first, read to read - so the climb's second read 30 seconds after a push to the base, and
// a read that failed at 90 s, tell nothing - or once the first is NullMergeableTellAfter old by the
// clock, beside `blocked` and `unknown` alike.
func TestAMergeOutcomeTellsOnlyWhatNothingOnItsWayWillClear(t *testing.T) {
	const head = "h2"
	none, one := 0, 1
	read := time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC)
	base := func(change func(*record.PullRequest)) record.PullRequest {
		pr := record.PullRequest{HeadSHA: head, State: record.PullRequestOpen, MergeState: record.MergeStateBlocked,
			Mergeability: record.MergeabilityMergeable, ReviewsRequested: &none, HeadChecks: record.HeadChecksConcluded, EvidenceReadAt: read,
			StateReadAt: read, CheckRuns: []record.AttemptRun{{Name: "unit", ID: 2, Head: head, Conclusion: record.RunSucceeded}}}
		change(&pr)
		return pr
	}
	// null is a run of that many null reads of the head, the first at read and the latest span after it.
	null := func(reads int, span time.Duration) func(*record.PullRequest) {
		return func(pr *record.PullRequest) {
			pr.Mergeability, pr.HeadChecks, pr.NullMergeableReads, pr.NullMergeableSince = record.MergeabilityUnknown, record.HeadChecksUnread, reads, read
			pr.StateReadAt, pr.EvidenceReadAt = read.Add(span), read.Add(span)
		}
	}
	unknownNull := func(reads int, span time.Duration) func(*record.PullRequest) {
		return func(pr *record.PullRequest) { null(reads, span)(pr); pr.MergeState = record.MergeStateUnknown }
	}
	older := func(pr *record.PullRequest) { pr.EvidenceReadAt = pr.StateReadAt.Add(-time.Minute) }
	floor := NullMergeableFloor
	for _, tc := range []struct {
		name string
		pr   record.PullRequest
		at   time.Time
		want MergeOutcome
	}{
		{"blocked, MERGEABLE, nothing running, no review asked for", base(func(*record.PullRequest) {}), read, MergeEndsRound},
		{"clean", base(func(pr *record.PullRequest) { pr.MergeState = record.MergeStateClean }), read, MergeEndsRound},
		{"unknown and MERGEABLE", base(func(pr *record.PullRequest) { pr.MergeState = record.MergeStateUnknown }), read, MergeWaitsQuietly},
		{"blocked with a check of the head running", base(func(pr *record.PullRequest) { pr.HeadChecks = record.HeadChecksRunning }), read, MergeWaitsQuietly},
		{"blocked with no rollup read", base(func(pr *record.PullRequest) { pr.HeadChecks = record.HeadChecksUnread }), read, MergeWaitsQuietly},
		{"blocked with no count read", base(func(pr *record.PullRequest) { pr.ReviewsRequested = nil }), read, MergeWaitsQuietly},
		{"blocked with no settlement of the head", base(func(pr *record.PullRequest) { pr.CheckRuns = nil }), read, MergeWaitsQuietly},
		// Evidence a release before 20261124130001 left beside a read of its own, at a new head or
		// the same one: none open and nothing running, of an older read. It is read as unread.
		{"blocked with an older read's evidence", base(older), read, MergeWaitsQuietly},
		{"blocked with an older read's review request", base(func(pr *record.PullRequest) { older(pr); pr.ReviewsRequested = &one }), read, MergeWaitsQuietly},
		{"blocked with a first null read", base(null(1, 0)), read.Add(time.Minute), MergeWaitsQuietly},
		// A second null read 30 seconds after the first is the climb after a push to the base,
		// meeting GitHub's routine recompute: below the floor it tells nothing.
		{"blocked with a second null read below the floor", base(null(2, 30*time.Second)), read.Add(30 * time.Second), MergeWaitsQuietly},
		{"blocked with a second null read just below the floor", base(null(2, floor-time.Second)), read.Add(floor - time.Second), MergeWaitsQuietly},
		{"blocked with a second null read at the floor", base(null(2, floor)), read.Add(floor), MergeWaitsToldNullMergeable},
		// A read that failed at 90 s answered no null: the floor is measured between the reads.
		{"blocked with null reads at 0 and 30 s and a fact at the floor", base(null(2, 30*time.Second)), read.Add(floor), MergeWaitsQuietly},
		{"blocked with a first null read the bound old", base(null(1, 0)), read.Add(NullMergeableTellAfter), MergeWaitsToldNullMergeable},
		{"blocked with a first null read just inside the bound", base(null(1, 0)), read.Add(NullMergeableTellAfter - time.Second), MergeWaitsQuietly},
		{"blocked with mergeability not read", base(func(pr *record.PullRequest) { pr.Mergeability = "" }), read.Add(time.Hour), MergeWaitsQuietly},
		{"blocked with an older read's null reads", base(func(pr *record.PullRequest) { null(3, 2*floor)(pr); older(pr) }), read.Add(time.Hour), MergeWaitsQuietly},
		{"unknown with a first null read", base(unknownNull(1, 0)), read.Add(time.Minute), MergeWaitsQuietly},
		{"unknown with a second null read below the floor", base(unknownNull(2, 30*time.Second)), read.Add(30 * time.Second), MergeWaitsQuietly},
		{"unknown with a second null read at the floor", base(unknownNull(2, floor)), read.Add(floor), MergeWaitsToldNullMergeable},
		{"unknown with a first null read the bound old", base(unknownNull(1, 0)), read.Add(NullMergeableTellAfter), MergeWaitsToldNullMergeable},
		{"unknown with a check of the head running and null reads", base(func(pr *record.PullRequest) {
			unknownNull(2, floor)(pr)
			pr.HeadChecks = record.HeadChecksRunning
		}), read.Add(floor), MergeWaitsQuietly},
		{"blocked with a review request open", base(func(pr *record.PullRequest) { pr.ReviewsRequested = &one }), read, MergeWaitsToldReviewRequested},
		{"behind", base(func(pr *record.PullRequest) { pr.MergeState = record.MergeStateBehind }), read, MergeWaitsTold},
		{"draft", base(func(pr *record.PullRequest) { pr.MergeState = record.MergeStateDraft }), read, MergeWaitsTold},
	} {
		if got := MergeOutcomeOf(tc.pr, tc.at); got != tc.want {
			t.Errorf("%s: MergeOutcomeOf = %d, want %d", tc.name, got, tc.want)
		}
	}
	for outcome, hold := range map[MergeOutcome]record.StuckHold{MergeWaitsToldNullMergeable: record.StuckHoldMergeableNull,
		MergeWaitsToldReviewRequested: record.StuckHoldReviewRequested, MergeWaitsTold: "", MergeWaitsQuietly: "", MergeEndsRound: ""} {
		if got := outcome.Hold(); got != hold {
			t.Errorf("outcome %d holds %q, want %q", outcome, got, hold)
		}
	}
}

// A reader reads GitHub again for `unknown`, and for a `blocked` answer missing a part of what the
// round decides it on; it stops at every other answer.
func TestAReaderReadsAgainOnlyForAnAnswerTheRoundCannotDecideOn(t *testing.T) {
	none := 0
	complete := func(change func(*record.PullRequest)) record.PullRequest {
		pr := record.PullRequest{MergeState: record.MergeStateBlocked, Mergeability: record.MergeabilityMergeable,
			ReviewsRequested: &none, HeadChecks: record.HeadChecksConcluded}
		change(&pr)
		return pr
	}
	for _, tc := range []struct {
		name   string
		answer record.PullRequest
		want   bool
	}{
		{"blocked and whole", complete(func(*record.PullRequest) {}), false},
		{"blocked with a check running", complete(func(pr *record.PullRequest) { pr.HeadChecks = record.HeadChecksRunning }), false},
		{"unknown", complete(func(pr *record.PullRequest) { pr.MergeState = record.MergeStateUnknown }), true},
		{"blocked with mergeable null", complete(func(pr *record.PullRequest) { pr.Mergeability = record.MergeabilityUnknown }), true},
		{"blocked with no rollup", complete(func(pr *record.PullRequest) { pr.HeadChecks = record.HeadChecksUnread }), true},
		{"blocked with no count", complete(func(pr *record.PullRequest) { pr.ReviewsRequested = nil }), true},
		{"behind with no count", complete(func(pr *record.PullRequest) {
			pr.MergeState, pr.ReviewsRequested = record.MergeStateBehind, nil
		}), false},
	} {
		if got := MergeAnswerIncomplete(tc.answer); got != tc.want {
			t.Errorf("%s: MergeAnswerIncomplete = %t, want %t", tc.name, got, tc.want)
		}
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
// stands, and none is set aside, since the push's new head decides the round, where a review on
// GitHub decides it: not at a review step the owner does (workflow's settleRound).
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
