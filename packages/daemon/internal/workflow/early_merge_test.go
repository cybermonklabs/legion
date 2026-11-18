package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A pull request that finishes before the issue reaches merging — merged early, or closed without
// merging — is the architect's to act on, as the shipped daemon routes it: the architect is told
// once, naming the pull request and the phase the issue was in, and the issue's phase is left for
// the architect to decide. A redelivered observation tells it nothing new.
func TestAPullRequestFinishingOutsideMergingTellsTheArchitectOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fact   intake.Fact
		kind   record.NoticeKind
		reason string
	}{
		{name: "merged early", fact: intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, kind: "pr-merged",
			reason: "pull request #42 merged while LEGION-208 was in reviewing"},
		{name: "closed without merging", fact: intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42}, kind: "pr-closed-unmerged",
			reason: "pull request #42 closed without merging while LEGION-208 was in reviewing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
				Failing: []string{}, CheckRuns: []record.AttemptRun{}})
			for _, id := range []string{"finished", "redelivered"} {
				if _, err := intake.ApplyFact(context.Background(), pool, "github", id, tc.fact, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
					t.Fatalf("ApplyFact %s: %v", id, err)
				}
			}
			assertPhase(t, pool, phase.Reviewing)
			got := architectNotices(t, pool)
			if len(got) != 1 || got[0].Kind != tc.kind || got[0].Role != claim.RoleArchitect || !strings.HasPrefix(got[0].Reason, tc.reason) {
				t.Fatalf("notices = %+v, want one %s to the architect beginning %q", got, tc.kind, tc.reason)
			}
		})
	}
}

// A reviewer's approval that stands on the head of a pull request that has already merged ends the
// review round and moves the issue to accepting, in either order of the approval and the merge
// (dispatch://INFRA-3738 R7). A merged pull request leaves the round nothing to wait on: GitHub
// answers `unknown` for it for good, and the daemon never reads its state after the merge. So
// each of those rows holds the merge state the record held before the merge, `blocked`, with no
// check run on the head concluded, on which an open pull request's round would wait: only the
// merge moves the issue. An approval after the merge moves it on the review. A merge after the
// approval moves it on the merge itself, after the one pr-merged notice the merge sends.
//
// Each of those rows also carries GitHub's deletion of the merged branch, which a repository that
// deletes a merged pull request's branch sends as a push from the head to no commit
// (record.NoCommit). The deletion names no head, so it takes no approval back, whether it arrives
// after the merge, before the approval, or while the pull request is still open.
//
// The record can hold a push from the head too, and what that push does to the approval is
// classify.CodeOnItsWay's rule. The deletion a release before the deletion guard stored as a push
// takes no approval back. A push delivered before a merge that names its head takes none back
// either: the merge drops the pushes the record holds. A push to the branch after the merge is not
// recorded. On a merged record that holds the stored deletion, an approval of another head is set
// aside: once its reviewer has completed, the round is stuck on other code, and the architect is
// told once, by the completion. A merged record at a head its merge did not name, holding a push
// from that head, keeps the round open and tells no one: a merge the release before recorded, or a
// merge that names no head, leaves that push on its way.
//
// A merge at a head the record does not hold ends no round: GitHub merged a head whose
// synchronize and push the record has not applied, and the reviewer approved the head before it.
// The record moves to the head the merge names, and the completed round is stuck on other code:
// the architect is told once, by the merge.
//
// An approval that stands on the head of a pull request closed without merging ends no round,
// even on a merge state GitHub would merge from and on a fact that settles the round (the
// reviewer's completion): the architect has the one pr-closed-unmerged notice and decides.
func TestAnApprovalStandingOnAMergedPullRequestsHeadEndsTheReviewRound(t *testing.T) {
	reviewer := record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"}
	approved := reviewer
	approved.Decision = &record.ReviewDecision{State: "approved", Head: headOne}
	approvedAndCompleted := approved
	approvedAndCompleted.HandoffRecord, approvedAndCompleted.HandoffVersion, approvedAndCompleted.Summary = "review-1", 1, "reviewed"
	approval := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne,
		HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)}
	merge := intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge", HeadSHA: headOne}
	mergeOfAnotherHead := intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge", HeadSHA: headTwo}
	mergeNamingNoHead := intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}
	deletion := intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: headOne, After: record.NoCommit, Pusher: "author", Sender: "author"}
	// pushFromHead is a push from headOne to headTwo, to a branch kept in place.
	pushFromHead := intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: headOne, After: headTwo, Pusher: "author", Sender: "author"}
	// heldPush is pushFromHead as the push handler records it (classify.ApplyPushDropping): by its
	// sender, and not the review App's, since the pusher is the author.
	heldPush := []record.ClassifiedPush{{SHA: headTwo, Before: headOne, Sender: "author"}}
	// storedDeletion is the deletion of the branch from head as the push handler stored it before
	// the deletion guard: a push like any other, from the head to no commit.
	storedDeletion := func(head string) []record.ClassifiedPush {
		return []record.ClassifiedPush{{SHA: record.NoCommit, Before: head, Sender: "author"}}
	}
	closeUnmerged := intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: headOne}
	completion := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim",
		Summary: "reviewed", HandoffRecord: "review-1", HandoffVersion: 1}
	for _, tc := range []struct {
		name       string
		state      record.PullRequestState
		mergeState record.MergeState
		// at is the head the record is seeded at, headOne where it is empty, and pushes the pushes
		// it holds.
		at       string
		pushes   []record.ClassifiedPush
		reviewer record.PhaseRow
		facts    []intake.Fact
		want     phase.Phase
		notices  []record.NoticeKind
		// held is the pushes the record ends holding, none where it is empty.
		held []record.ClassifiedPush
		// head is the head the record ends at, and stuckBy the summary of its one review-stuck
		// notice; each is checked only where it is set.
		head, stuckBy string
	}{
		{name: "the approval after the merge", state: record.PullRequestMerged, mergeState: record.MergeStateBlocked,
			reviewer: reviewer, facts: []intake.Fact{deletion, approval},
			want: phase.Accepting, notices: []record.NoticeKind{"phase-finished"}},
		{name: "the merge after the approval", state: record.PullRequestOpen, mergeState: record.MergeStateBlocked,
			reviewer: approvedAndCompleted, facts: []intake.Fact{deletion, merge},
			want: phase.Accepting, notices: []record.NoticeKind{"pr-merged", "phase-finished"}},
		{name: "the approval, the branch's deletion, then the merge", state: record.PullRequestOpen, mergeState: record.MergeStateBlocked,
			reviewer: reviewer, facts: []intake.Fact{approval, deletion, merge},
			want: phase.Accepting, notices: []record.NoticeKind{"pr-merged", "phase-finished"}},
		{name: "a merge at a head the record does not hold", state: record.PullRequestOpen, mergeState: record.MergeStateBlocked,
			reviewer: approvedAndCompleted, facts: []intake.Fact{mergeOfAnotherHead},
			want: phase.Reviewing, notices: []record.NoticeKind{"pr-merged", "review-stuck"}, head: headTwo, stuckBy: byMerge},
		{name: "a merged record holding the deletion push stored before the guard", state: record.PullRequestMerged, mergeState: record.MergeStateBlocked,
			pushes: storedDeletion(headOne), reviewer: reviewer, facts: []intake.Fact{approval},
			want: phase.Accepting, notices: []record.NoticeKind{"phase-finished"}, held: storedDeletion(headOne), head: headOne},
		{name: "a push from the merged head delivered before the merge", state: record.PullRequestOpen, mergeState: record.MergeStateBlocked,
			reviewer: reviewer, facts: []intake.Fact{approval, pushFromHead, merge},
			want: phase.Accepting, notices: []record.NoticeKind{"pr-merged", "phase-finished"}, head: headOne},
		{name: "a push to the branch after the merge, then the approval", state: record.PullRequestMerged, mergeState: record.MergeStateBlocked,
			reviewer: reviewer, facts: []intake.Fact{pushFromHead, approval},
			want: phase.Accepting, notices: []record.NoticeKind{"phase-finished"}, head: headOne},
		{name: "an approval of another head on a merged record that holds a push", state: record.PullRequestMerged, mergeState: record.MergeStateBlocked,
			at: headTwo, pushes: storedDeletion(headTwo), reviewer: approved, facts: []intake.Fact{completion},
			want: phase.Reviewing, notices: []record.NoticeKind{"review-stuck"}, held: storedDeletion(headTwo), head: headTwo, stuckBy: byCompletion},
		{name: "a merged record at a head its merge did not name, holding a push from it", state: record.PullRequestMerged, mergeState: record.MergeStateBlocked,
			pushes: heldPush, reviewer: reviewer, facts: []intake.Fact{approval},
			want: phase.Reviewing, notices: []record.NoticeKind{}, held: heldPush, head: headOne},
		{name: "a push from the head, then a merge that names no head", state: record.PullRequestOpen, mergeState: record.MergeStateBlocked,
			reviewer: reviewer, facts: []intake.Fact{approval, pushFromHead, mergeNamingNoHead},
			want: phase.Reviewing, notices: []record.NoticeKind{"pr-merged"}, held: heldPush, head: headOne},
		{name: "a close without merging after the approval", state: record.PullRequestOpen, mergeState: record.MergeStateClean,
			reviewer: approved, facts: []intake.Fact{closeUnmerged, completion},
			want: phase.Reviewing, notices: []record.NoticeKind{"pr-closed-unmerged"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
			at := tc.at
			if at == "" {
				at = headOne
			}
			pr := record.PullRequest{State: tc.state, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Base: "main",
				HeadSHA: at, Pushes: tc.pushes, Failing: []string{}, Cancelled: []string{}, CheckRuns: []record.AttemptRun{},
				Mergeability: record.MergeabilityMergeable, MergeState: tc.mergeState}
			if tc.state == record.PullRequestMerged {
				pr.MergeSHA = "merge"
			}
			seedPR(t, pool, pr)
			seedPhase(t, pool, tc.reviewer)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedClaimSession(t, pool, reviewClaim, "ses-reviewer")

			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			for i, fact := range tc.facts {
				if result := apply(fmt.Sprintf("fact-%d", i), fact); result.Refusal != nil {
					t.Fatalf("fact %d (%T) was refused: %+v", i, fact, result.Refusal)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); !slices.Equal(got, tc.notices) {
				t.Fatalf("notices %v, want %v", got, tc.notices)
			}
			got := recordedPR(t, pool, "LEGION-208")
			if tc.head != "" && got.HeadSHA != tc.head {
				t.Fatalf("the record ends at head %s, want %s", got.HeadSHA, tc.head)
			}
			if !slices.Equal(got.Pushes, tc.held) {
				t.Fatalf("the record ends holding pushes %+v, want %+v", got.Pushes, tc.held)
			}
			if got := reviewStuckNotices(t, pool); tc.stuckBy != "" && (len(got) != 1 || got[0].Summary != tc.stuckBy) {
				t.Fatalf("review-stuck notices %+v, want one written by %q", got, tc.stuckBy)
			}
		})
	}
}

// A close older than the pull request's newest lifecycle event is a late redelivery
// (classify.LateLifecycle): it neither closes the pull request nor tells the architect. The newer
// close closes it at the head it carries and tells the architect once, and that close redelivered at
// its own clock, which is not late, tells it nothing more, since the pull request is already closed.
func TestALateCloseTellsTheArchitectNothingAndARedeliveredCloseNothingMore(t *testing.T) {
	earlier, later := lateApplied.Add(-time.Hour), lateApplied.Add(time.Minute)
	pool := appliedEvents(t, record.PullRequestOpen)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	for _, step := range []struct {
		id      string
		fact    intake.Fact
		head    string
		state   record.PullRequestState
		notices int
	}{
		{"late-close", lateClosed("head-b", earlier), "head-c", record.PullRequestOpen, 0},
		{"close", lateClosed("head-d", later), "head-d", record.PullRequestClosed, 1},
		{"redelivered-close", lateClosed("head-d", later), "head-d", record.PullRequestClosed, 1},
	} {
		apply(step.id, step.fact)
		if got, notices := pullRequestAt(t, pool), architectNotices(t, pool); got.head != step.head || got.state != step.state || len(notices) != step.notices {
			t.Fatalf("after %s the pull request is %+v, with architect notices %+v; want %s, %s, and %d", step.id, got, notices, step.head, step.state, step.notices)
		}
	}
	assertPhase(t, pool, phase.Reviewing)
}

// A close newer than the closed record is a second close, with GitHub's reopen between the two
// still undelivered, not a redelivery: it tells the architect once, whichever of the reopen and
// the close arrives first, and the pull request ends closed at the close's head.
func TestASecondCloseTellsTheArchitectOnceWhicheverOrderItsReopenArrives(t *testing.T) {
	reopened, closed := lateApplied.Add(time.Minute), lateApplied.Add(2*time.Minute)
	for _, tc := range []struct {
		name  string
		facts []intake.Fact
	}{
		{"the reopen delivered first", []intake.Fact{lateReopened("head-c", reopened), lateClosed("head-d", closed)}},
		{"the close delivered first", []intake.Fact{lateClosed("head-d", closed), lateReopened("head-c", reopened)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := appliedEvents(t, record.PullRequestClosed, tc.facts...)
			if got, notices := pullRequestAt(t, pool), architectNotices(t, pool); got.head != "head-d" || got.state != record.PullRequestClosed || len(notices) != 1 {
				t.Fatalf("the pull request is %+v, with architect notices %+v; want head-d, closed, and one notice", got, notices)
			}
		})
	}
}

// A close with no clock (from a listener that did not carry updated_at) cannot be newer than a
// closed record, so it is a redelivery: the pull request stays closed and the architect is told
// nothing more.
func TestAClocklessCloseOfAClosedPullRequestTellsTheArchitectNothing(t *testing.T) {
	pool := appliedEvents(t, record.PullRequestClosed, lateClosed("head-c", time.Time{}))
	if got, notices := pullRequestAt(t, pool), architectNotices(t, pool); got.state != record.PullRequestClosed || len(notices) != 0 {
		t.Fatalf("the pull request is %+v, with architect notices %+v; want closed, and none", got, notices)
	}
}

// GitHub never closes a merged pull request, so a close observed after the merge is older than
// it, and the merge carries no clock that could fence it: a close of a pull request recorded
// merged changes nothing and tells the architect nothing.
func TestACloseOfAMergedPullRequestChangesNothing(t *testing.T) {
	pool := appliedEvents(t, record.PullRequestMerged, lateClosed("head-d", lateApplied.Add(time.Minute)))
	if got, notices := pullRequestAt(t, pool), architectNotices(t, pool); got.head != "head-c" || got.state != record.PullRequestMerged || len(notices) != 0 {
		t.Fatalf("the pull request is %+v, with architect notices %+v; want head-c, merged, and none", got, notices)
	}
}

// GitHub never reopens a merged pull request either, so a reopen observed after the merge is older
// than it: a reopen of a pull request recorded merged changes nothing, and it stays merged at the
// head it merged at.
func TestAReopenOfAMergedPullRequestChangesNothing(t *testing.T) {
	pool := appliedEvents(t, record.PullRequestMerged, lateReopened("head-d", lateApplied.Add(time.Minute)))
	if got, notices := pullRequestAt(t, pool), architectNotices(t, pool); got.head != "head-c" || got.state != record.PullRequestMerged || len(notices) != 0 {
		t.Fatalf("the pull request is %+v, with architect notices %+v; want head-c, merged, and none", got, notices)
	}
}

// An issue that reaches merging with its pull request already merged has nothing left to merge:
// the merge closes it at once, no merger is started for it, its tree lingers as any root's close
// does, and the one start the close queues is the tester's first look of the production check
// the merge opens (merge_close_test.go).
func TestAnIssueReachingMergingWithItsPullRequestMergedCloses(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Retro, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer-claim"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
		MergeSHA: "merge", Failing: []string{}, CheckRuns: []record.AttemptRun{}})

	result, err := intake.ApplyFact(context.Background(), pool, "api", "retro", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer-claim", Generation: 1, Summary: "retro done", Commit: "head"}, mergingEngine(), admissionStub{})
	if err != nil || result.Refusal != nil {
		t.Fatalf("ApplyFact retro = %#v, %v", result, err)
	}
	assertPhase(t, pool, phase.Done)
	var lingering bool
	if err := pool.QueryRow(context.Background(), "select linger_until is not null from issues where key = 'LEGION-208'").Scan(&lingering); err != nil || !lingering {
		t.Fatalf("the root lingers = %t, %v; want its tree lingering after the close", lingering, err)
	}
	assertOutboxCount(t, pool, "linger_close", 1)
	if started := superviseRequests(t, pool, "start"); len(started) != 1 || started["LEGION-208/tester"] != 1 {
		t.Fatalf("the close started %v, want the tester's check look alone and no merger", started)
	}
	if check := productionCheck(t, pool, "LEGION-208"); check == nil || !check.Open() || check.MergeSHA != "merge" || check.Looks != 1 {
		t.Fatalf("the production check = %+v, want it open on merge commit merge at look 1", check)
	}
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages = %q, want none: the daemon posts no packet", got)
	}
}

// architectNotices is every notice the outbox holds for the architect, in order.
func architectNotices(t *testing.T, pool *pgxpool.Pool) []record.Notice {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'kind', coalesce(payload->>'role', ''), coalesce(payload->>'reason', '') from outbox where kind = 'notice' and payload->>'role' = 'architect' order by id")
	if err != nil {
		t.Fatalf("read notices: %v", err)
	}
	defer rows.Close()
	notices := []record.Notice{}
	for rows.Next() {
		var notice record.Notice
		if err := rows.Scan(&notice.Kind, &notice.Role, &notice.Reason); err != nil {
			t.Fatalf("scan notice: %v", err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate notices: %v", err)
	}
	return notices
}
