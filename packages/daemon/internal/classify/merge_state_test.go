package classify

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// GitHub's verdict on merging a head - its mergeability and its merge state - is its answer about
// the tree that head carries, so a move to another head leaves nothing to read: any push may have
// changed the code GitHub answered about, and a verdict of another head must never move an
// approval (dispatch://INFRA-714 R2). The time that verdict was read goes with it
// (record.PullRequest.StateReadAt), and so do the count of open review requests and the head's
// rollup the same read gave (ReviewsRequested, HeadChecks): a count or a rollup left behind would
// say the head the record now stands at had been read when it was the head before it that was. A
// synchronize that moves no head keeps all of them, since GitHub answered about that very tree.
// The push that links a move judges it again:
// the arrival replays with that push (replayRun), which gives the verdict back from the run's
// anchor, so the order the webhooks arrive in decides nothing once both have arrived.
func TestEveryMoveTakesTheMergeVerdictAndNoMoveKeepsIt(t *testing.T) {
	read := time.Date(2026, 11, 12, 7, 5, 3, 0, time.UTC)
	code := record.ClassifiedPush{SHA: "h3", Before: "h2"}
	tests := record.ClassifiedPush{SHA: "h3", Before: "h2", ByReviewApp: true}
	for _, tc := range []struct {
		name  string
		facts []runFact
		state record.MergeState
	}{
		{"a code push, then its head", []runFact{pushed(code), arrivesFrom("h2", "h3")}, record.MergeStateUnread},
		{"a head, then its code push", []runFact{arrivesFrom("h2", "h3"), pushed(code)}, record.MergeStateUnread},
		{"the review App's push, then its head", []runFact{pushed(tests), arrivesFrom("h2", "h3")}, record.MergeStateUnread},
		{"a head whose push has not arrived", []runFact{arrivesFrom("h2", "h3")}, record.MergeStateUnread},
		{"a synchronize back to the recorded head", []runFact{arrivesFrom("h1", "h2")}, record.MergeStateClean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			two := 2
			pr := record.PullRequest{HeadSHA: "h2", Failing: []string{},
				Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean, StateReadAt: read,
				ReviewsRequested: &two, HeadChecks: record.HeadChecksConcluded, EvidenceReadAt: read,
				NullMergeableReads: 1, NullMergeableSince: read}
			for _, fact := range tc.facts {
				pr = stored(t, fact.apply(pr))
			}
			wantMergeable, wantRead, wantCount, wantChecks, wantNulls := record.MergeabilityMergeable, read, &two, record.HeadChecksConcluded, 1
			if tc.state == record.MergeStateUnread {
				wantMergeable, wantRead, wantCount, wantChecks, wantNulls = "", time.Time{}, nil, record.HeadChecksUnread, 0
			}
			if pr.MergeState != tc.state || pr.Mergeability != wantMergeable || !pr.StateReadAt.Equal(wantRead) ||
				!SameCount(pr.ReviewsRequested, wantCount) || pr.HeadChecks != wantChecks || !pr.EvidenceReadAt.Equal(wantRead) ||
				pr.NullMergeableReads != wantNulls || pr.NullMergeableSince.Equal(read) != (wantNulls == 1) {
				t.Fatalf("after %v the verdict is %q/%q read at %v with %v review requests, checks %q, %d null reads since %v, written at %v; want %q/%q read at %v with %v, %q, %d, written then",
					tc.facts, pr.Mergeability, pr.MergeState, pr.StateReadAt, pr.ReviewsRequested, pr.HeadChecks, pr.NullMergeableReads,
					pr.NullMergeableSince, pr.EvidenceReadAt, wantMergeable, tc.state, wantRead, wantCount, wantChecks, wantNulls)
			}
		})
	}
}

// A verdict read while a head run is open is GitHub's answer for the head the run has reached -
// the engine drops a read that answers for any other head - so the push that closes the run must
// not put the anchor's older answer back over it. The anchor gives its verdict back only to a
// record holding none (withJudgement), which is the record a move no push linked yet left. The
// read time travels with the verdict on both paths: the anchor keeps the one its own head was read
// at, and the replay that keeps the record's own verdict keeps the time that one was read at.
func TestAVerdictReadWhileAHeadRunIsOpenStandsOverTheAnchors(t *testing.T) {
	anchored := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
	reread := time.Date(2026, 11, 12, 7, 5, 3, 0, time.UTC)
	one := 1
	pr := record.PullRequest{HeadSHA: "h2", Failing: []string{},
		Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean, StateReadAt: anchored}
	pr = stored(t, arrivesFrom("h2", "h3").apply(pr))
	if pr.Run == nil || pr.MergeState != record.MergeStateUnread || !pr.StateReadAt.IsZero() {
		t.Fatalf("after the head arrived before its push the run is open %v with verdict %q read at %v, want it open with nothing read",
			pr.Run != nil, pr.MergeState, pr.StateReadAt)
	}
	if pr.Run.Anchor.MergeState != record.MergeStateClean || !pr.Run.Anchor.StateReadAt.Equal(anchored) {
		t.Fatalf("the run is anchored at %q read at %v, want h2's own clean read at %v: the anchor keeps the whole verdict, the time it was read with it",
			pr.Run.Anchor.MergeState, pr.Run.Anchor.StateReadAt, anchored)
	}
	// The read the arrival asks for, answering for h3 (workflow's askMergeState, mergeability),
	// with a review request open and the head's checks concluded.
	pr.Mergeability, pr.MergeState, pr.StateReadAt = record.MergeabilityMergeable, record.MergeStateBlocked, reread
	pr.ReviewsRequested, pr.HeadChecks = &one, record.HeadChecksConcluded

	pr = stored(t, pushed(record.ClassifiedPush{SHA: "h3", Before: "h2"}).apply(pr))
	if pr.Run != nil || pr.MergeState != record.MergeStateBlocked || !pr.StateReadAt.Equal(reread) ||
		!SameCount(pr.ReviewsRequested, &one) || pr.HeadChecks != record.HeadChecksConcluded {
		t.Fatalf("after the push closed the run the verdict is %q read at %v with %v review requests and checks %q, with the run open %v; want h3's own blocked read at %v with 1 and concluded, and the run closed",
			pr.MergeState, pr.StateReadAt, pr.ReviewsRequested, pr.HeadChecks, pr.Run != nil, reread)
	}
}

// A head run's anchor keeps the whole verdict of the head it was opened at, the count of open
// review requests and the head's rollup with it, through the JSON the record stores it in; and the
// replay gives all of it back to a record a move left with none (withJudgement), so the record
// then holds the answer GitHub gave, whole, and never a merge state beside no count.
func TestAnAnchoredVerdictComesBackWithItsCountAndRollup(t *testing.T) {
	anchored := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
	three := 3
	pr := record.PullRequest{HeadSHA: "h2", Failing: []string{},
		Mergeability: record.MergeabilityUnknown, MergeState: record.MergeStateBlocked, StateReadAt: anchored,
		ReviewsRequested: &three, HeadChecks: record.HeadChecksRunning, EvidenceReadAt: anchored, NullMergeableReads: 2, NullMergeableSince: anchored}
	pr = stored(t, arrivesFrom("h2", "h3").apply(pr))
	if pr.Run == nil || pr.ReviewsRequested != nil || pr.HeadChecks != record.HeadChecksUnread || !pr.EvidenceReadAt.IsZero() || pr.NullMergeableReads != 0 {
		t.Fatalf("after the head arrived before its push the record holds %v review requests, checks %q, %d null reads written at %v with the run open %v, want none read and the run open",
			pr.ReviewsRequested, pr.HeadChecks, pr.NullMergeableReads, pr.EvidenceReadAt, pr.Run != nil)
	}
	anchor := pr.Run.Anchor
	if !SameCount(anchor.ReviewsRequested, &three) || anchor.HeadChecks != record.HeadChecksRunning || !anchor.EvidenceReadAt.Equal(anchored) ||
		anchor.NullMergeableReads != 2 || !anchor.NullMergeableSince.Equal(anchored) {
		t.Fatalf("the run's anchor holds %v review requests, checks %q, %d null reads since %v written at %v, want h2's 3, running, 2 since %v",
			anchor.ReviewsRequested, anchor.HeadChecks, anchor.NullMergeableReads, anchor.NullMergeableSince, anchor.EvidenceReadAt, anchored)
	}
	back := withJudgement(pr, anchor)
	if back.MergeState != record.MergeStateBlocked || !SameCount(back.ReviewsRequested, &three) || back.HeadChecks != record.HeadChecksRunning ||
		!EvidenceStands(back) || back.NullMergeableReads != 2 || !back.NullMergeableSince.Equal(anchored) {
		t.Fatalf("the replay gave back %q with %v review requests, checks %q, %d null reads written at %v beside a read at %v, want the anchor's whole answer, standing",
			back.MergeState, back.ReviewsRequested, back.HeadChecks, back.NullMergeableReads, back.EvidenceReadAt, back.StateReadAt)
	}
	// A record holding a read of its own keeps it, the count and rollup with it.
	none := 0
	read := pr
	read.MergeState, read.ReviewsRequested, read.HeadChecks = record.MergeStateClean, &none, record.HeadChecksConcluded
	kept := withJudgement(read, pr.Run.Anchor)
	if kept.MergeState != record.MergeStateClean || !SameCount(kept.ReviewsRequested, &none) || kept.HeadChecks != record.HeadChecksConcluded {
		t.Fatalf("the replay over a record with its own read left %q with %v review requests and checks %q, want its clean with 0 and concluded",
			kept.MergeState, kept.ReviewsRequested, kept.HeadChecks)
	}
}
