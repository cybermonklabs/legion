package record

import (
	"slices"
	"testing"
)

// HeldBefore reads six words before dispatch_blocker, in the ladder's order, and no other:
// dispatch_blocker itself, the words after it and a word the ladder does not name — the zero
// row's empty word among them — stand before it nowhere. A reorder of HeldReasons moves that
// answer, so it fails here rather than passing silently. What `why` does with those six is
// stated by its sentence table (the projection's heldSentences), whose own test holds the table
// to this answer.
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
