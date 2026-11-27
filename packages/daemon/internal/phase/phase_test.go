package phase

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// All is the wire contract's phase list, in the transition table's order.
func TestPhaseValuesMatchWorkflowWireContract(t *testing.T) {
	want := []string{
		"admitted", "awaiting_person", "awaiting_children", "planning", "implementing", "testing", "reviewing",
		"step", "accepting", "retro", "merging", "done", "held",
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
// moves back by its own order (workflow's Walk.MovesBack), along 41 of the 57 pairs of MovePairs
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

// A step of a role the deployment defines runs as the phase step, which no role of Legion's own
// works: RoleFor names none for it, it ends with no handoff record, and it is the phase such a
// role has left wherever the issue stands, so its completion is of step. Legion's own roles leave
// the phases they left before, and a name that is no role leaves none.
func TestADeploymentRoleLeavesTheStepPhase(t *testing.T) {
	step := Phase("step")
	if got := RoleFor(step); got != "" {
		t.Errorf("RoleFor(step) = %q, want none: the deployment's role is the plan's", got)
	}
	if word, writer, ok := Handoff(step); ok {
		t.Errorf("Handoff(step) = %q, %q, true; want no handoff record", word, writer)
	}
	for _, at := range []Phase{step, Implementing, Retro, Held} {
		if got := LeftBy("checker", at); got != step {
			t.Errorf("LeftBy(checker, %s) = %q, want step", at, got)
		}
	}
	if got := Of("checker", step); got != step {
		t.Errorf("Of(checker, step) = %q, want step", got)
	}
	for role, want := range map[claim.Role]Phase{
		claim.RoleTester: Testing, claim.RoleImplementer: Implementing, claim.RoleArchitect: "",
		claim.RoleController: "", "": "", "Checker": "",
	} {
		if got := LeftBy(role, step); got != want {
			t.Errorf("LeftBy(%q, step) = %q, want %q", role, got, want)
		}
	}
}

// MovePairs is every move the daemon counts: each pair of two different phases of Order and step,
// and step-step. A task in another order than today's moves back by its own order (workflow's
// Walk.MovesBack), so a move back of one task, testing-reviewing, is a forward pair of Order; a
// task whose steps name two roles of the deployment runs both as step, so one moves back to the
// other by step-step. It holds BackwardPairs first, in their order, then the 21 forward pairs of
// Order, then the pairs of step. No other pair holds one phase two times, and a phase outside
// Order and step is in no pair.
func TestMovePairsTakeTheStepPairs(t *testing.T) {
	pairs := MovePairs()
	if len(pairs) != 57 {
		t.Fatalf("MovePairs has %d pairs, want 57: %q", len(pairs), pairs)
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
	if !slices.Equal(pairs[21:42], forward) {
		t.Fatalf("MovePairs goes on %q, want the forward pairs %q", pairs[21:42], forward)
	}
	for _, pair := range pairs[42:] {
		from, to, ok := ParsePair(pair)
		if !ok || (from != Step && to != Step) || (from != Step && !slices.Contains(Order, from)) || (to != Step && !slices.Contains(Order, to)) {
			t.Fatalf("MovePairs ends with %q, which is no pair of step and a phase of Order, nor step-step", pair)
		}
	}
	seen := map[string]bool{}
	for _, pair := range pairs {
		if seen[pair] {
			t.Fatalf("MovePairs repeats %q", pair)
		}
		seen[pair] = true
	}
	for _, tc := range []struct {
		from, to Phase
		want     bool
	}{
		{Step, Implementing, true}, {Implementing, Step, true}, {Step, Step, true}, {Step, Merging, true}, {Planning, Step, true},
		{Testing, Reviewing, true}, {Reviewing, Testing, true}, {Implementing, Planning, true}, {Retro, Merging, true},
		{Testing, Testing, false}, {Held, Planning, false}, {Merging, Done, false}, {Admitted, Planning, false},
		{Step, Held, false}, {Done, Step, false},
	} {
		if got := IsMovePair(tc.from, tc.to); got != tc.want {
			t.Fatalf("IsMovePair(%s, %s) = %t, want %t", tc.from, tc.to, got, tc.want)
		}
	}
	if from, to, ok := ParsePair("step-step"); !ok || from != Step || to != Step {
		t.Fatalf("ParsePair(step-step) = %q, %q, %t, want step, step, true", from, to, ok)
	}
	pairs[0] = "rubbish"
	if MovePairs()[0] == "rubbish" {
		t.Fatal("MovePairs returned the stored slice, which a caller can overwrite")
	}
}
