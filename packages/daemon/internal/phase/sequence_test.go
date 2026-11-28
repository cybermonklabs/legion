package phase

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The library holds the three sequences dispatch://INFRA-3570 names, by name and in this order,
// and each phase list is the one that issue decided: today's line for `default`, the three phases
// no lighter workflow may drop for `light`, and today's line without its planner for
// `no-planner`.
func TestTheLibraryHoldsTheThreeSequences(t *testing.T) {
	want := map[string][]Phase{
		DefaultSequenceName: {Planning, Implementing, Testing, Reviewing, Accepting, Retro, Merging},
		"light":             {Implementing, Reviewing, Merging},
		"no-planner":        {Implementing, Testing, Reviewing, Accepting, Retro, Merging},
	}
	if got := SequenceNames(); !slices.Equal(got, []string{DefaultSequenceName, "light", "no-planner"}) {
		t.Fatalf("SequenceNames() = %q, want default, light and no-planner", got)
	}
	for name, phases := range want {
		sequence, ok := SequenceFor(name)
		if !ok || sequence.Name != name || !slices.Equal(sequence.Phases, phases) {
			t.Errorf("SequenceFor(%q) = %+v, %t, want %v", name, sequence, ok, phases)
		}
	}
	// The default is today's whole line, so an issue that names no sequence walks exactly what
	// every issue walked before the library existed.
	if !slices.Equal(DefaultSequence().Phases, Order) {
		t.Errorf("the default sequence walks %v, want Order %v", DefaultSequence().Phases, Order)
	}
	for _, unknown := range []string{"", "Light", "heavy", "default "} {
		if sequence, ok := SequenceFor(unknown); ok {
			t.Errorf("SequenceFor(%q) = %+v, want no sequence: the library answers only the names it holds", unknown, sequence)
		}
	}
	// The answers are copies, so a caller that edits one leaves the library whole.
	Sequences()[0].Phases[0] = Done
	if sequence, _ := SequenceFor(DefaultSequenceName); sequence.Phases[0] != Planning {
		t.Fatal("Sequences returned the stored phase list, which a caller can overwrite")
	}
}

// Every sequence of the library keeps the two rules dispatch://INFRA-3570 sets for any sequence:
// it is a sub-list of Order in Order's own order, and it holds implementing, reviewing and
// merging, the review before the landing that no lighter workflow may drop
// (dispatch://INFRA-3577/spec R34).
func TestEverySequenceOfTheLibraryKeepsTheRules(t *testing.T) {
	for _, sequence := range Sequences() {
		if err := sequence.validate(); err != nil {
			t.Errorf("the library's %s: %v", sequence.Name, err)
		}
	}
}

// validate can fail, and fails for each rule on its own: a sequence that drops reviewing, one that
// drops implementing or merging, one that walks two phases in the reverse of Order's order, one
// that walks a phase twice, one that walks a phase outside Order, and one with no name. A check
// that passed the library above and could refuse none of these would prove nothing about it.
func TestValidateRefusesASequenceThatBreaksARule(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		sequence Sequence
		says     string
	}{
		{name: "no reviewing", sequence: Sequence{Name: "no-review", Phases: []Phase{Implementing, Merging}}, says: "reviewing"},
		{name: "no implementing", sequence: Sequence{Name: "no-implementer", Phases: []Phase{Planning, Reviewing, Merging}}, says: "implementing"},
		{name: "no merging", sequence: Sequence{Name: "no-merge", Phases: []Phase{Implementing, Reviewing}}, says: "merging"},
		{name: "reordered", sequence: Sequence{Name: "reordered", Phases: []Phase{Implementing, Testing, Reviewing, Retro, Accepting, Merging}}, says: "order"},
		{name: "repeated", sequence: Sequence{Name: "repeated", Phases: []Phase{Implementing, Reviewing, Reviewing, Merging}}, says: "order"},
		{name: "outside Order", sequence: Sequence{Name: "held", Phases: []Phase{Implementing, Held, Reviewing, Merging}}, says: "held"},
		{name: "no name", sequence: Sequence{Phases: []Phase{Implementing, Reviewing, Merging}}, says: "name"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.sequence.validate()
			if err == nil {
				t.Fatalf("validate(%+v) accepted it, want it refused", testCase.sequence)
			}
			if !strings.Contains(err.Error(), testCase.says) {
				t.Fatalf("validate(%+v) = %v, want the refusal to name %q", testCase.sequence, err, testCase.says)
			}
		})
	}
}

// The implementer works two phases of the default sequence, implementing and retro, so a
// completion it reports after its issue moved on is of retro once the issue stands past retro, and
// of implementing before. A sequence without retro has the implementer work implementing alone: a
// light issue in merging stands past where retro would be in Order, and the implementer's late
// completion there is of implementing, the one phase of that sequence it worked, never of a retro
// the issue never ran. The default's answer is the control: it is today's.
func TestTheImplementersLeftPhaseIsOfItsSequence(t *testing.T) {
	light, _ := SequenceFor("light")
	noPlanner, _ := SequenceFor("no-planner")
	for _, testCase := range []struct {
		sequence Sequence
		at       Phase
		want     Phase
	}{
		{sequence: DefaultSequence(), at: Merging, want: Retro},
		{sequence: DefaultSequence(), at: Testing, want: Implementing},
		{sequence: DefaultSequence(), at: Accepting, want: Implementing},
		{sequence: noPlanner, at: Merging, want: Retro},
		{sequence: light, at: Merging, want: Implementing},
		{sequence: light, at: Reviewing, want: Implementing},
	} {
		if got := LeftBy(claim.RoleImplementer, testCase.at, testCase.sequence); got != testCase.want {
			t.Errorf("LeftBy(implementer, %s, %s) = %q, want %q", testCase.at, testCase.sequence.Name, got, testCase.want)
		}
		if got := Of(claim.RoleImplementer, testCase.at, testCase.sequence); got != testCase.want {
			t.Errorf("Of(implementer, %s, %s) = %q, want %q", testCase.at, testCase.sequence.Name, got, testCase.want)
		}
	}
	// A role answers its own phase while the issue stands in it, whatever the sequence.
	if got := Of(claim.RoleImplementer, Implementing, light); got != Implementing {
		t.Errorf("Of(implementer, implementing, light) = %q, want implementing", got)
	}
	if got := Of(claim.RoleMerger, Merging, light); got != Merging {
		t.Errorf("Of(merger, merging, light) = %q, want merging", got)
	}
}
