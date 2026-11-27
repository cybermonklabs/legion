package phase

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The implementer works two phases, implementing and the retro, and a completion of it that
// arrives after the issue moved on is of the one it left: implementing before the retro, and the
// retro once the issue stands past it, in merging and, after the merge, in verifying, where its
// close waits. Verifying stands outside Order, so the line alone cannot place it past the retro,
// and held stands outside it too: a held issue's work reached no phase past where it was held.
func TestAnImplementersCompletionAfterItsRetroIsOfTheRetro(t *testing.T) {
	for _, tc := range []struct{ at, want Phase }{
		{Testing, Implementing},
		{Reviewing, Implementing},
		{Accepting, Implementing},
		{Retro, Retro},
		{Merging, Retro},
		{Verifying, Retro},
		{Held, Implementing},
	} {
		if got := Of(claim.RoleImplementer, tc.at); got != tc.want {
			t.Errorf("the implementer's completion while its issue stands in %s is of %q, want %q", tc.at, got, tc.want)
		}
	}
}

// All is the wire contract's phase list, in the transition table's order.
func TestPhaseValuesMatchWorkflowWireContract(t *testing.T) {
	want := []string{
		"admitted", "awaiting_person", "awaiting_children", "planning", "implementing", "testing", "reviewing",
		"accepting", "retro", "merging", "verifying", "done", "held",
	}
	got := make([]string, len(All))
	for index, value := range All {
		got[index] = string(value)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("All = %q, want %q", got, want)
	}
}

// BackwardPairs is every move back of today's line, Order: the triangle over the seven working
// phases of Order, 21 pairs. There is no move back of Order beyond them, since no phase of this
// workflow waits for a merge somebody else makes (dispatch://INFRA-714). A task in another order
// moves back by its own order (workflow's Walk.MovesBack), along 28 of the 42 pairs of MovePairs
// (plan.CanMoveBack), so these 21 are the moves back of a task in the default steps, not of
// every task.
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
		// MovePairs holds each pair of two different phases of Order, a forward one as well as a
		// move back, since a task in another order moves back by its own order; ParsePair answers
		// against that list.
		{name: "a forward pair of Order, which MovePairs holds", text: "retro-merging", from: Retro, to: Merging, ok: true},
		{name: "another forward pair of Order", text: "planning-implementing", from: Planning, to: Implementing, ok: true},
		{name: "a phase paired with itself", text: "testing-testing"},
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
	for _, outside := range []Phase{Admitted, AwaitingPerson, AwaitingChildren, Done, Held} {
		if MovesBack(Merging, outside) || MovesBack(outside, Planning) {
			t.Fatalf("%s stands outside Order, so no move into or out of it moves back", outside)
		}
	}
}

// MovePairs is every move between two different phases of Order the daemon counts: a task in
// another order than today's moves back by its own order (workflow's Walk.MovesBack), so a move
// back of one task, testing-reviewing, is a forward pair of Order. It holds BackwardPairs first,
// in their order, then the 21 forward pairs, each phase of Order paired with every phase after it.
// No pair holds one phase two times, and a phase outside Order is in no pair.
func TestMovePairsHoldsEveryPairOfTwoDifferentPhases(t *testing.T) {
	pairs := MovePairs()
	if len(pairs) != 42 {
		t.Fatalf("MovePairs has %d pairs, want 42: %q", len(pairs), pairs)
	}
	if !slices.Equal(pairs[:21], BackwardPairs()) {
		t.Fatalf("MovePairs begins %q, want BackwardPairs %q in their order", pairs[:21], BackwardPairs())
	}
	// Written out here, not built by the loop under test.
	forward := []string{
		"planning-implementing", "planning-testing", "planning-reviewing", "planning-accepting", "planning-retro", "planning-merging",
		"implementing-testing", "implementing-reviewing", "implementing-accepting", "implementing-retro", "implementing-merging",
		"testing-reviewing", "testing-accepting", "testing-retro", "testing-merging",
		"reviewing-accepting", "reviewing-retro", "reviewing-merging",
		"accepting-retro", "accepting-merging",
		"retro-merging",
	}
	if !slices.Equal(pairs[21:], forward) {
		t.Fatalf("MovePairs ends %q, want the forward pairs %q", pairs[21:], forward)
	}
	for _, tc := range []struct {
		from, to Phase
		want     bool
	}{
		{Testing, Reviewing, true}, {Reviewing, Testing, true}, {Implementing, Planning, true}, {Retro, Merging, true},
		{Testing, Testing, false}, {Held, Planning, false}, {Merging, Done, false}, {Admitted, Planning, false},
	} {
		if got := IsMovePair(tc.from, tc.to); got != tc.want {
			t.Fatalf("IsMovePair(%s, %s) = %t, want %t", tc.from, tc.to, got, tc.want)
		}
	}
	pairs[0] = "rubbish"
	if MovePairs()[0] == "rubbish" {
		t.Fatal("MovePairs returned the stored slice, which a caller can overwrite")
	}
}
