package record

import (
	"slices"
	"testing"
)

// The held words that take `why`'s blocked arm are the six the ladder reads before
// dispatch_blocker, in the ladder's order, and no other: a blocked issue whose word is one of
// them reads that word's sentence rather than its links' (the projection's whyStates, through
// HeldBefore). A reorder of HeldReasons changes which sentence such an issue reads, so it fails
// here rather than passing silently. dispatch_blocker itself, the words after it and a word the
// ladder does not name — the zero row's empty word among them — take the arm of the links.
func TestTheHeldWordsThatTakeTheBlockedArmAreTheSixTheLadderReadsBeforeTheLinks(t *testing.T) {
	var before []HeldReason
	for _, word := range HeldReasons {
		if HeldBefore(word, HeldDispatchBlocker) {
			before = append(before, word)
		}
	}
	want := []HeldReason{HeldTreeClosed, HeldTreeParked, HeldLabelOff, HeldAncestry, HeldRootAwaitingPerson, HeldDesignGate}
	if !slices.Equal(before, want) {
		t.Errorf("the words HeldBefore reads before dispatch_blocker = %v, want %v", before, want)
	}
	for _, word := range []HeldReason{HeldDispatchBlocker, HeldChildrenFirst, HeldAcceptedAncestor, HeldAtWork,
		HeldUnclassified, "", "not_a_word"} {
		if HeldBefore(word, HeldDispatchBlocker) {
			t.Errorf("HeldBefore(%q, dispatch_blocker) = true, want false", word)
		}
	}
	if HeldBefore(HeldTreeClosed, "not_a_word") {
		t.Errorf("HeldBefore(tree_closed, an unknown word) = true, want false: an unknown word ranks nowhere")
	}
}
