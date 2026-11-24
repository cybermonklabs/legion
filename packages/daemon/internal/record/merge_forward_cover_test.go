package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Each cover of a merge forward (mergeForwardCovered) joins two steps of a merge forward and goes
// one way, so of two messages of one event whose recipients one claim holds, one is the covering
// message, whichever of them its sender addresses first, and the claim gets that one alone
// (workflow's addressMergeForward). A cover named both ways would leave the message the claim keeps
// to the order the sender addressed the two in. The fault it holds off was planted once, the cover
// map naming the hold as covering the ask too, and the test failed on it.
func TestEachMergeForwardCoverGoesOneWay(t *testing.T) {
	t.Parallel()
	for step, covered := range mergeForwardCovered {
		_, known := mergeForwardRoles[step]
		_, coveredKnown := mergeForwardRoles[covered]
		if !known || !coveredKnown || step == covered {
			t.Fatalf("%q covers %q, want two steps of a merge forward (%v)", step, covered, mergeForwardRoles)
		}
		if !MergeForwardCovers(step, covered) || MergeForwardCovers(covered, step) {
			t.Fatalf("MergeForwardCovers(%s, %s) is not the one way the cover goes", step, covered)
		}
	}
}

// A message that covers another is the one message its claim gets for the event, so it carries
// what the one it covers asked: the review that covers the landed tells the reviewer, which also
// works the step the issue stands at, to go on with that step at the new head after its review, the
// one thing only the landed said; and the ask that covers the hold tells the agent asked, which also
// works that step, that the step waits for the merge. So each covering message's text is another
// than the same message's with no cover, and its cover is the one field that makes it so. The words
// are not compared: they are the agent's to read.
func TestACoveringMergeForwardMessageSaysWhatItCovers(t *testing.T) {
	t.Parallel()
	pr := PullRequest{Repo: "sjawhar/legion", MergeForward: &MergeForward{Head: "c0de", Base: "main", Phase: phase.Accepting, Landed: "d00d"}}
	for _, message := range []MergeForwardMessage{
		{Role: claim.RoleReviewer, Step: MergeForwardReview, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Accepting, PullRequest: 42, Covers: MergeForwardLanded},
		{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Reviewing, PullRequest: 42, Why: ConflictCheckNotGreen, Covers: MergeForwardHold},
	} {
		bare := message
		bare.Covers = ""
		if message.Message("LEGION-208", pr) == bare.Message("LEGION-208", pr) {
			t.Fatalf("the %s that covers a %s sends the text of the %s that covers nothing, want it to say what it covers", message.Step, message.Covers, message.Step)
		}
	}
}
