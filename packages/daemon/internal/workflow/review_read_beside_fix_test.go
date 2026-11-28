package workflow

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// listedReviewID is the event id the listed approval of X is applied under: one id for the one
// review, as the daemon's read of the list mints it from the review's id and submission time, so a
// second application of it is the same fact.
const listedReviewID = "review-read-1"

// listedReviewOfX is the reviewer's own approval of fixHead as the daemon's read of GitHub's list of
// LEGION-208's pull request's reviews builds it (intake.PullRequestReview.ReadFromList): its Legion
// footer names the reviewer's session, fixHead and the entry of the round that stands, and its
// commit_id is headTwo, the pull request's current head, where GitHub's list moves it.
func (r *fixRig) listedReviewOfX() intake.PullRequestReview {
	r.t.Helper()
	return intake.PullRequestReview{Repo: statesRepo, Number: 42, ID: 1, State: "approved", CommitID: headTwo, Author: testReviewApp,
		Body: roundReviewBody(r.t, r.pool, "ses-reviewer", "reviewed", fixHead), SubmittedAt: stepClock.Add(time.Hour), ReadFromList: true}
}

// listRead is the judgement of review that the daemon's read of the list makes before it applies
// one (daemon's reviewToApply): the session of the reviewer's claim (ReviewerSession) and the
// commit the review step started at beside a fix (ReviewBesideFix), read in one read-only
// transaction, then RefusesRound of review against the pull request's record. It answers that
// commit and the refusal.
func (r *fixRig) listRead(review intake.PullRequestReview) (string, RoundRefusal) {
	r.t.Helper()
	var session, besideFix string
	var pr *record.PullRequest
	if err := store.TransactWith(r.t.Context(), r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if pr, err = r.engine.store.PullRequest(ctx, tx, "LEGION-208"); err != nil {
			return err
		}
		if session, err = r.engine.ReviewerSession(ctx, tx, "LEGION-208"); err != nil {
			return err
		}
		besideFix, err = r.engine.ReviewBesideFix(ctx, tx, "LEGION-208")
		return err
	}); err != nil {
		r.t.Fatalf("read the reviewer's session and the commit beside the fix: %v", err)
	}
	if pr == nil {
		r.t.Fatal("LEGION-208 has no pull request")
	}
	return besideFix, r.engine.RefusesRound(review, *pr, session, besideFix)
}

// fixWay is the way fix n of LEGION-208's task a has taken (record.TaskFix.Way), 0 for none.
func (r *fixRig) fixWay(n int) int {
	r.t.Helper()
	way := 0
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error {
		fix, err := r.engine.store.FixAt(ctx, tx, "LEGION-208", 1, "a", n)
		if err == nil && fix != nil {
			way = fix.Way
		}
		return err
	})
	return way
}

// The daemon's read of GitHub's list of a pull request's reviews judges a review before it applies
// it, in a transaction of its own, so a fact can apply between the two. Here the review step started
// at X beside fix 1, which pushed Y and ended while the review ran, so the way after fix 1 waits for
// the review's end. The read judges the reviewer's own approval of X, listed while the pull request
// stands at Y, and takes it: the commit its review step started at beside the fix is X. Before the
// read's fact applies, the check fails again at Y. No fix opens while the way after fix 1 waits
// (openFix), so the latest fix is still fix 1, which ran beside the review, and the commit beside
// the fix is still X. The approval, applied from the list, then decides its round: the reviewer's
// row holds the approval of X with no refusal, the review's run ends passed at X, the way after fix
// 1 runs the steps after it again at Y (way 2, the tester first), and the fix rule at the tail of
// that fact opens fix 2 for the failure at Y.
//
// Its red is openFix's wait taken out: the failure at Y then opens fix 2 while the way after fix 1
// waits, fix 2 reads its job and has not started, so the latest fix ran beside no review, the
// commit beside the fix is "", and the approval of X is refused at Y.
func TestAListedReviewOfXDecidesAfterAFailureAtTheFixsHead(t *testing.T) {
	// beside is LEGION-208 with its review step open at X beside fix 1, which pushed Y and ended
	// while the review ran, its way pending, and the listed approval of X, which the read's
	// judgement takes then.
	beside := func(t *testing.T) (*fixRig, intake.PullRequestReview) {
		t.Helper()
		r := besideTheFix(t)
		r.fixPushes(headTwo)
		if result := r.apply("fix-done", fixCompletion(headTwo)); result.Refusal != nil {
			t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
		}
		r.assertFixEnded("passed", headTwo, false, 0)
		review := r.listedReviewOfX()
		if besideFix, refusal := r.listRead(review); besideFix != fixHead || refusal.Reason != "" {
			t.Fatalf("the read's judgement beside fix 1 names the commit %q and refuses %q, want %s and no refusal", besideFix, refusal.Reason, fixHead)
		}
		return r, review
	}
	fix2 := "2 reading k-ci@" + headTwo + " place 1"

	t.Run("no fix opens while the way after fix 1 waits, and the approval decides", func(t *testing.T) {
		r, review := beside(t)

		r.fails(headTwo, "k-ci")

		if got, want := r.fixes(), []string{fixOf(1, "ended", "k-ci")}; !slices.Equal(got, want) {
			t.Errorf("the fixes after the failure at %s = %q, want %q: no fix opens while the way after fix 1 waits", headTwo, got, want)
		}
		if besideFix, refusal := r.listRead(review); besideFix != fixHead || refusal.Reason != "" {
			t.Errorf("after the failure at %s the commit beside the fix is %q and the judgement refuses %q, want %s and no refusal", headTwo, besideFix, refusal.Reason, fixHead)
		}

		if result := r.apply(listedReviewID, review); result.Refusal != nil || result.Unspent || result.Duplicate {
			t.Fatalf("the listed approval of %s applied as %+v, want it applied", fixHead, result)
		}

		r.assertApprovalOfXDecided()
		if got := r.reviewEndCommit(); got != fixHead {
			t.Fatalf("the review step ends at %q, want %s, the commit its review read", got, fixHead)
		}
		if got := r.fixWay(1); got != wayAfterFix {
			t.Fatalf("fix 1 took way %d after the review's end, want %d: it made commits, so the steps after it run again", got, wayAfterFix)
		}
		assertPhase(t, r.pool, phase.Testing)
		assertStrings(t, "the fixes after the review's end", r.fixes(), []string{fixOf(1, "ended", "k-ci"), fix2})
	})
}
