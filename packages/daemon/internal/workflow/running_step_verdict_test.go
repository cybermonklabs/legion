package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Spec R5: review_verdict takes the verdict of the agent of a review step that has passed, and of
// no step that runs now. In accepting the owner's acceptance is the step the issue stands in and
// has not passed: its approve is its sign_off and its changes its move back out of accepting, so
// its review_verdict there, before its sign_off, is refused VERDICT_NOT_PAST_REVIEW, records no
// verdict, asks for no write of legion-verdict and moves no phase, and the refusal names the
// sign_off. The reviewer's step has passed, so the reviewer's review_verdict at the same head is
// still taken and recorded as its step's verdict there.
func TestReviewVerdictRefusesTheStepRunningNow(t *testing.T) {
	pool := seedPastReview(t, phase.Accepting, "needs_review")
	var asked []string
	engine := writingEngine(&asked)
	for _, word := range []string{"approve", "changes"} {
		result := applyFacts(t, pool, engine)("owner-"+word, ownerVerdict(word, headTwo))
		if result.Refusal == nil || result.Refusal.Code != "VERDICT_NOT_PAST_REVIEW" || result.Refusal.Status != 409 {
			t.Fatalf("the owner's review_verdict %s in accepting = %+v, want 409 VERDICT_NOT_PAST_REVIEW", word, result.Refusal)
		}
		for _, want := range []string{"sign_off", "nothing was recorded"} {
			if !strings.Contains(result.Refusal.Message, want) {
				t.Fatalf("the refusal of the owner's %s says %q, want it to say %q", word, result.Refusal.Message, want)
			}
		}
	}
	if given, found := stepVerdictOf(t, pool, headTwo, acceptanceStep); found {
		t.Fatalf("after the owner's refused verdicts the acceptance holds %+v at %s, want none", given, headTwo)
	}
	if len(asked) != 0 {
		t.Fatalf("the owner's refused verdicts asked for writes on %v, want none", asked)
	}
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("after the owner's refused verdicts the issue is in %s, want accepting", got)
	}

	applyRefusingNothing(t, pool, engine, pastReviewVerdict("approve", headTwo))
	if given, found := stepVerdictOf(t, pool, headTwo, reviewerStep); !found || given.Verdict != store.VerdictApprove || given.Session != string(reviewClaim) {
		t.Fatalf("the verdict of the reviewer's step at %s = %+v (found %t), want the reviewer's approve under %s", headTwo, given, found, reviewClaim)
	}
	if !slices.Equal(asked, []string{headTwo}) {
		t.Fatalf("the reviewer's verdict asked for writes on %v, want one on %s", asked, headTwo)
	}
}
