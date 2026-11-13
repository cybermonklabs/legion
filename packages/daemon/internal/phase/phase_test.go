package phase

import (
	"slices"
	"testing"
)

// All is the wire contract's phase list, in the transition table's order.
func TestPhaseValuesMatchWorkflowWireContract(t *testing.T) {
	want := []string{
		"admitted", "awaiting_person", "planning", "implementing", "testing", "reviewing", "accepting", "retro",
		"merging", "done", "held",
	}
	got := make([]string, len(All))
	for index, value := range All {
		got[index] = string(value)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("All = %q, want %q", got, want)
	}
}

// BackwardPairs is the whole vocabulary of moves back: the triangle over the seven working
// phases of Order, 21 pairs. There is no pair beyond them, since no phase of this workflow waits
// for a merge somebody else makes (dispatch://INFRA-714).
func TestBackwardPairsHoldsEveryMoveBack(t *testing.T) {
	pairs := BackwardPairs()
	if len(pairs) != 21 {
		t.Fatalf("BackwardPairs has %d pairs, want 21: %q", len(pairs), pairs)
	}
	seen := map[string]bool{}
	for _, pair := range pairs {
		if seen[pair] {
			t.Fatalf("BackwardPairs repeats %q", pair)
		}
		seen[pair] = true
		from, to, ok := ParsePair(pair)
		if !ok {
			t.Fatalf("ParsePair(%q) refused a pair of its own list", pair)
		}
		if !MovesBack(from, to) {
			t.Fatalf("%q is no move back", pair)
		}
	}
	for _, want := range []string{"implementing-planning", "merging-retro", "merging-implementing"} {
		if !seen[want] {
			t.Fatalf("BackwardPairs misses %q: %q", want, pairs)
		}
	}
	// The answer is a copy, so a caller that trims it leaves the next caller's answer whole.
	pairs[0] = "rubbish"
	if BackwardPairs()[0] == "rubbish" {
		t.Fatal("BackwardPairs returned the stored slice, which a caller can overwrite")
	}
}

func TestParsePairReadsOnlyKnownMovesBack(t *testing.T) {
	for _, testCase := range []struct {
		name string
		text string
		from Phase
		to   Phase
		ok   bool
	}{
		{name: "a move back", text: "reviewing-implementing", from: Reviewing, to: Implementing, ok: true},
		{name: "the phase word holding an underscore", text: "awaiting_person-planning"},
		// A pair of two phases of Order that moves forward is no move back, and the list is what
		// ParsePair answers against.
		{name: "a pair outside the list", text: "retro-merging"},
		{name: "a forward move", text: "planning-implementing"},
		{name: "a phase that is no part of the line", text: "held-planning"},
		{name: "rubbish", text: "not a pair at all"},
		{name: "nothing", text: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			from, to, ok := ParsePair(testCase.text)
			if ok != testCase.ok || from != testCase.from || to != testCase.to {
				t.Fatalf("ParsePair(%q) = %q, %q, %t, want %q, %q, %t",
					testCase.text, from, to, ok, testCase.from, testCase.to, testCase.ok)
			}
		})
	}
}

func TestMovesBackIgnoresPhasesOutsideOrder(t *testing.T) {
	if !MovesBack(Retro, Testing) {
		t.Fatal("retro -> testing is a move back")
	}
	if MovesBack(Testing, Retro) {
		t.Fatal("testing -> retro moves forward")
	}
	for _, outside := range []Phase{Admitted, AwaitingPerson, Done, Held} {
		if MovesBack(Merging, outside) || MovesBack(outside, Planning) {
			t.Fatalf("%s stands outside Order, so no move into or out of it moves back", outside)
		}
	}
}
