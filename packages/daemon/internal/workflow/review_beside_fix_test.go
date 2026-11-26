package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// besideTheFix is LEGION-208 in reviewing on `implementer tester reviewer`, its review step
// started at fixHead and fix 1 running beside it for k-ci failed there (startedFix), with the
// session the reviewer's agent registered recorded on the reviewer's claim: the session a
// review-App review's Legion footer names to be the reviewer's own (byReviewer).
func besideTheFix(t *testing.T) *fixRig {
	t.Helper()
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	seedClaimSession(t, r.pool, reviewClaim, "ses-reviewer")
	return r
}

// reviewOf applies review id of LEGION-208's pull request in state, as the reviewer's own session
// submits it through the review App: its Legion footer names that session, head as the head it
// read, and the entry the reviewer's row stands at (roundReviewBody). It is submitted an hour after
// the engine's clock, so after any completion of the reviewer's: the reviewer's answer to a round
// its completion left undecided (reviewersAnswer).
func (r *fixRig) reviewOf(id int64, state, head string) {
	r.t.Helper()
	r.apply(fmt.Sprintf("review-%d", id), intake.PullRequestReview{Repo: statesRepo, Number: 42, ID: id, State: state, CommitID: head,
		Author: testReviewApp, Body: roundReviewBody(r.t, r.pool, "ses-reviewer", "reviewed", head), SubmittedAt: stepClock.Add(time.Hour)})
}

// fixPushes applies the fix's push of head onto the branch of LEGION-208's task from fixHead, the
// head its check failed at: the push GitHub delivers, then the synchronize that moves the pull
// request's head.
func (r *fixRig) fixPushes(head string) {
	r.t.Helper()
	r.apply("fix-push", intake.Push{Repo: statesRepo, Branch: "legion/LEGION-208", Before: fixHead, After: head, Pusher: "legion-implementer[bot]"})
	r.apply("fix-sync", intake.PullRequestSynchronized{Repo: statesRepo, Number: 42, Branch: "legion/LEGION-208", Before: fixHead, HeadSHA: head})
}

// reviewerCompletes applies the reviewer's completion of its round, naming its handoff record.
func (r *fixRig) reviewerCompletes() {
	r.t.Helper()
	r.apply("review-done", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: reviewClaim,
		Summary: "reviewed", HandoffRecord: reviewRecord, HandoffVersion: 1})
}

// assertApprovalOfXDecided fails unless the reviewer's approval of fixHead decided its round: the
// reviewer's row records it with no refusal, the commit the reviewer read is fixHead, and the run
// of the review's step, the task's third, ended passed.
func (r *fixRig) assertApprovalOfXDecided() {
	r.t.Helper()
	if decision := reviewerDecision(r.t, r.pool); decision == nil || decision.State != "approved" || decision.Head != fixHead || decision.Refused != "" {
		r.t.Fatalf("the reviewer's decision = %+v, want the approval of %s with no refusal", decision, fixHead)
	}
	if got := reviewerRow(r.t, r.pool).ReviewedHead; got != fixHead {
		r.t.Fatalf("the reviewer's row reads the reviewed commit %q, want %s, the commit its review step started at", got, fixHead)
	}
	r.assertRunEnded(3, 1, ResultPassed, 0)
}

// assertLeftTheReview fails unless LEGION-208's task has gone on past its review.
func (r *fixRig) assertLeftTheReview() {
	r.t.Helper()
	if got := issueOf(r.t, r.pool, r.engine, "LEGION-208").Phase; got == phase.Reviewing {
		r.t.Fatalf("LEGION-208 stands in %s, want its task gone on past its review", got)
	}
}

// assertRefusedAt fails unless a review whose footer names named decided nothing while the pull
// request stands at headTwo: the reviewer's row records no decision and keeps the refusal for the
// stuck notice, the issue stays in reviewing, and the review's run has not ended.
func (r *fixRig) assertRefusedAt(named string) {
	r.t.Helper()
	want := "footer names " + named + ", not the head " + headTwo
	if decision := reviewerDecision(r.t, r.pool); decision == nil || decision.State != "" || !strings.HasPrefix(decision.Refused, want) {
		r.t.Fatalf("the reviewer's decision = %+v, want none, refused as %q", decision, want)
	}
	assertPhase(r.t, r.pool, phase.Reviewing)
	if end := r.runEnd(3, 1); end != nil {
		r.t.Fatalf("the review's run ended %+v, want it running", end)
	}
}

// A review step that started at X ends with the review of X, also after the fix beside it pushed
// Y: the reviewer's approval whose Legion footer names X decides its round though the pull request
// stands at Y, the commit the reviewer read stays X for the next review, and the review's run ends
// passed. After the fix ended with new commits, the way after it then runs the steps after the fix
// again at Y, the tester's first. While the fix runs, the task goes on past its review.
func TestAReviewOfXDecidesBesideTheFix(t *testing.T) {
	r := besideTheFix(t)
	r.pushed(headTwo)
	r.apply("fix-done", fixCompletion(headTwo))

	r.reviewOf(1, "approved", fixHead)

	r.assertApprovalOfXDecided()
	assertPhase(t, r.pool, phase.Testing)

	t.Run("while the fix runs", func(t *testing.T) {
		r := besideTheFix(t)
		r.pushed(headTwo)

		r.reviewOf(1, "approved", fixHead)

		r.assertApprovalOfXDecided()
		r.assertLeftTheReview()
	})
}

// A push of the fix moves the pull request's head from X to Y while the review runs, and only the
// fix pushes while it runs, so the push re-asks no round of the reviewer. The reviewer completed
// its round with no deciding review, and the architect was told once that the round is stuck; the
// fix's push of Y tells nothing more, where a head moved by the work under review would tell the
// architect to ask the reviewer for a review of Y. The reviewer's approval of X then decides its
// step. An approval of X that waits on GitHub's answer for X when the fix pushes is decided by
// that push, and nobody is told the round is stuck.
func TestAPushOfTheFixReasksNoReview(t *testing.T) {
	r := besideTheFix(t)
	r.reviewerCompletes()
	if told := reviewStuckNotices(t, r.pool); len(told) != 1 {
		t.Fatalf("after the reviewer completed undecided the architect was told %+v, want one review-stuck notice", told)
	}

	r.fixPushes(headTwo)

	if told := reviewStuckNotices(t, r.pool); len(told) != 1 {
		t.Fatalf("after the fix's push the architect was told %+v, want only the notice of the completion: the fix's push re-asks no review", told)
	}
	r.apply("fix-done", fixCompletion(headTwo))
	r.reviewOf(1, "approved", fixHead)
	r.assertApprovalOfXDecided()
	assertPhase(t, r.pool, phase.Testing)

	// GitHub's answer that keeps an approval beside a fix waiting is one the fix does not cause:
	// the head is behind its base (a blocked head beside a running fix ends the round).
	t.Run("an approval of X that waits on GitHub's answer", func(t *testing.T) {
		r := besideTheFix(t)
		seedPR(t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: statesRepo, Number: 42,
			Branch: "legion/LEGION-208", HeadSHA: fixHead, Failing: []string{}, MergeState: record.MergeStateBehind})
		r.reviewOf(1, "approved", fixHead)
		r.reviewerCompletes()
		assertPhase(t, r.pool, phase.Reviewing)

		r.fixPushes(headTwo)

		if told := reviewStuckNotices(t, r.pool); len(told) != 0 {
			t.Fatalf("after the fix's push the architect was told %+v, want nothing: the push is the fix's", told)
		}
		r.assertApprovalOfXDecided()
		r.assertLeftTheReview()
	})
}

// The control, kept: beside the fix only the commit the review step started at decides beside the
// pull request's head. A review whose footer names Z, neither X nor Y, is refused as today, and the
// issue stays in reviewing with the refusal kept for the stuck notice; and with no fix beside the
// review, a review of X is refused at Y, as today.
func TestAReviewOfAnotherHeadStillWaits(t *testing.T) {
	r := besideTheFix(t)
	r.pushed(headTwo)

	r.reviewOf(1, "approved", headThree)

	r.assertRefusedAt(headThree)

	t.Run("a review of X with no fix beside it is refused at Y", func(t *testing.T) {
		r := reviewing(t)
		seedClaimSession(t, r.pool, reviewClaim, "ses-reviewer")
		r.pushed(headTwo)

		r.reviewOf(1, "approved", fixHead)

		r.assertRefusedAt(fixHead)
	})
}

// mergeStateAtX records GitHub's merge state for LEGION-208's pull request at fixHead, with one
// check run concluded there, so an approval standing on fixHead is judged on that state alone.
func (r *fixRig) mergeStateAtX(state record.MergeState) {
	r.t.Helper()
	seedPR(r.t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: statesRepo, Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: fixHead, Failing: []string{}, Cancelled: []string{},
		Mergeability: record.MergeabilityMergeable, MergeState: state,
		CheckedHead: fixHead, CheckRuns: []record.AttemptRun{{Name: "unit tests", ID: 1, Head: fixHead, Conclusion: record.RunSucceeded}}})
}

// While a fix runs beside the review, the reviewer's approval of X, the head GitHub reports
// `blocked`, ends its round and the task goes on past its review, with no notice of a stuck round:
// a check the fix owns is what blocks the head, and the task's end waits for each check a CI job
// runs. The controls, kept: with no fix the same approval stays in reviewing and the architect is
// told the round is stuck, as the base does; and a head GitHub reports `behind` keeps the wait
// beside a fix, since the fix does not cause it.
func TestAnApprovalOfABlockedHeadBesideARunningFixEndsItsRound(t *testing.T) {
	r := besideTheFix(t)
	r.mergeStateAtX(record.MergeStateBlocked)

	r.reviewOf(1, "approved", fixHead)

	r.assertLeftTheReview()
	if told := reviewStuckNotices(t, r.pool); len(told) != 0 {
		t.Fatalf("the architect was told %+v, want nothing: the blocked head beside a fix ends the round", told)
	}

	t.Run("with no fix the round stays stuck and tells", func(t *testing.T) {
		r := reviewing(t)
		seedClaimSession(t, r.pool, reviewClaim, "ses-reviewer")
		r.mergeStateAtX(record.MergeStateBlocked)
		r.reviewOf(1, "approved", fixHead)
		assertPhase(t, r.pool, phase.Reviewing)
		if told := reviewStuckNotices(t, r.pool); len(told) != 1 {
			t.Fatalf("the architect was told %+v, want one review-stuck notice", told)
		}
	})

	t.Run("a head behind its base keeps the wait beside a fix", func(t *testing.T) {
		r := besideTheFix(t)
		r.mergeStateAtX(record.MergeStateBehind)
		r.reviewOf(1, "approved", fixHead)
		assertPhase(t, r.pool, phase.Reviewing)
	})
}

// reviewBesideFix is the commit LEGION-208's running review step started at beside a fix of its
// task, as the engine reads it (ReviewBesideFix), "" for none.
func (r *fixRig) reviewBesideFix() string {
	r.t.Helper()
	var besideFix string
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		besideFix, err = r.engine.ReviewBesideFix(ctx, tx, "LEGION-208")
		return err
	})
	return besideFix
}

// personPushes applies a person's push of head onto the branch of LEGION-208's task from before,
// then the synchronize that moves the pull request's head: a push that is no fix's.
func (r *fixRig) personPushes(before, head string) {
	r.t.Helper()
	r.apply("person-push", intake.Push{Repo: statesRepo, Branch: "legion/LEGION-208", Before: before, After: head, Pusher: "a-person"})
	r.apply("person-sync", intake.PullRequestSynchronized{Repo: statesRepo, Number: 42, Branch: "legion/LEGION-208", Before: before, HeadSHA: head})
}

// A review step that starts after the fix ended runs beside no fix, whatever the engine's clock
// reads at the fix's end and at the step's start. Fix 1 at the task's end pushes Y and ends, way 2
// runs the tester again at Y, the tester passes, and the review step starts at Y. A person then
// pushes Z, which is no fix's push, and the reviewer approves Y: the approval is refused at Z, and
// the issue stays in reviewing. Its red is the clock deciding it: where the fix's end did not read
// before the step's start, as with a clock that does not advance or one that steps back, the step
// was taken for one beside the fix, and the approval of Y ended the round at Z, a head nobody
// reviewed.
func TestAReviewThatStartsAfterTheFixEndedRunsBesideNoFixWhateverTheClock(t *testing.T) {
	for _, clock := range engineClocks {
		t.Run(clock.name, func(t *testing.T) {
			r := fixAtTheEnd(t, returnEngine())
			seedClaimSession(t, r.pool, reviewClaim, "ses-reviewer")
			clock.set(r.engine)
			r.fixEnds(headTwo)
			r.assertWay(wayAfterFix, phase.Testing, claim.RoleTester, headTwo, 1, retroTesting)
			r.nextStart(TriggerTesterPassed, claim.RoleReviewer, phase.Reviewing)
			if got := r.reviewBesideFix(); got != "" {
				t.Errorf("the review step that started at %s after fix 1 ended stands beside a fix at %q, want none", headTwo, got)
			}

			r.personPushes(headTwo, headThree)
			r.reviewOf(1, "approved", headTwo)

			want := "footer names " + headTwo + ", not the head " + headThree
			if decision := reviewerDecision(t, r.pool); decision == nil || decision.State != "" || !strings.HasPrefix(decision.Refused, want) {
				t.Fatalf("the reviewer's decision = %+v, want none, refused as %q", decision, want)
			}
			assertPhase(t, r.pool, phase.Reviewing)
		})
	}
}

// A review step that a fix ran beside stays beside it after the fix ends, whatever the engine's
// clock reads at the step's start and at the fix's end. The review step starts at X, k-ci fails
// there, fix 1 starts beside the review, pushes Y and ends with its way pending on the review, and
// the reviewer approves X: the approval decides its round though the pull request stands at Y. Its
// red is the clock deciding it: with a clock that steps back, the fix's end read before the step's
// start, the step was taken for one beside no fix, and the approval of X was refused at Y.
func TestAReviewTheFixRanBesideKeepsItsCommitWhateverTheClock(t *testing.T) {
	for _, clock := range engineClocks {
		t.Run(clock.name, func(t *testing.T) {
			engine := returnEngine()
			clock.set(engine)
			r := newFixRig(t, engine, []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
			seedClaimSession(t, r.pool, reviewClaim, "ses-reviewer")
			r.fails(fixHead, "k-ci")
			r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
			r.fixPushes(headTwo)
			if result := r.apply("fix-done", fixCompletion(headTwo)); result.Refusal != nil {
				t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
			}
			r.assertFixEnded("passed", headTwo, false, 0)
			if got := r.reviewBesideFix(); got != fixHead {
				t.Errorf("the review step fix 1 ran beside stands beside a fix at %q, want %s, the commit it started at", got, fixHead)
			}

			r.reviewOf(1, "approved", fixHead)

			r.assertApprovalOfXDecided()
		})
	}
}

// fixEndsWithNoCommit is fix 1's own completion at the head its check failed at, X: the fix made no
// commit of its own, so its EndHead is its Head, and its way waits for the running review step.
func (r *fixRig) fixEndsWithNoCommit() {
	r.t.Helper()
	if result := r.apply("fix-done", fixCompletion(fixHead)); result.Refusal != nil {
		r.t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}
	r.assertFixEnded("passed", fixHead, false, 0)
}

// A person can push to a task's branch at any moment, and after the fix ended nobody else is held
// off it either, so a head the fix did not make is no fix's. Fix 1 runs beside the review step that
// started at X and ends with no commit of its own, so its way waits for that step; a person then
// pushes P. The round stays open: the push asks the reviewer for a review of P, and the approval of
// X is refused at P, since the reviewer read neither P nor a head the fix made. Its red is the rule
// before: any head beside a fix of the step's start counted as the fix's, so the push asked nothing,
// the approval of X ended the round, and the task left its review at P with nobody's review of it.
func TestAPersonsPushBesideAnEndedFixIsNoFixs(t *testing.T) {
	r := besideTheFix(t)
	r.fixEndsWithNoCommit()
	if got := r.reviewBesideFix(); got != fixHead {
		t.Fatalf("the review step beside fix 1 at its end stands beside a fix at %q, want %s", got, fixHead)
	}

	r.personPushes(fixHead, headThree)

	if got := r.reviewBesideFix(); got != "" {
		t.Errorf("after a person pushed %s the review stands beside a fix at %q, want none: the head is no fix's", headThree, got)
	}
	if told := reviewStuckNotices(t, r.pool); len(told) != 1 {
		t.Errorf("after a person's push the architect was told %+v, want one notice asking for a review of %s", told, headThree)
	}

	r.reviewOf(1, "approved", fixHead)

	want := "footer names " + fixHead + ", not the head " + headThree
	if decision := reviewerDecision(t, r.pool); decision == nil || decision.State != "" || !strings.HasPrefix(decision.Refused, want) {
		t.Fatalf("the reviewer's decision = %+v, want none, refused as %q", decision, want)
	}
	assertPhase(t, r.pool, phase.Reviewing)
	if end := r.runEnd(3, 1); end != nil {
		t.Fatalf("the review's run ended %+v, want it open: no review of %s has come", end, headThree)
	}
}

// The way after a fix counts the commits of the task's branch, not the fix's own: where the branch
// moved while the fix ran, the steps after the fix run again at the head it moved to, whoever pushed
// it. Fix 1 ends with no commit of its own beside the running review step, a person pushes P, and
// the reviewer approves P: the way after the fix is way 2, which runs the tester again with its
// working copy moved to P. Its red is the count before: the fix's own end against its own start,
// which read no new commits and took way 3, so the task went on to its retro with P reviewed by its
// reviewer but tested by nobody.
func TestTheWayAfterAFixCountsTheTasksCommits(t *testing.T) {
	r := besideTheFix(t)
	r.fixEndsWithNoCommit()

	r.personPushes(fixHead, headThree)
	r.reviewOf(1, "approved", headThree)

	if decision := reviewerDecision(t, r.pool); decision == nil || decision.State != "approved" || decision.Head != headThree {
		t.Fatalf("the reviewer's decision = %+v, want the approval of %s", decision, headThree)
	}
	if way := r.fixWay(1); way != wayAfterFix {
		t.Fatalf("fix 1 took way %d, want %d: the task's branch moved while the fix ran, so the steps after it run again", way, wayAfterFix)
	}
	assertPhase(t, r.pool, phase.Testing)
	if got := r.moveToOf(claim.RoleTester); got != headThree {
		t.Fatalf("the tester's start moves its working copy to %q, want %s, the head the task's branch stands at", got, headThree)
	}
}
