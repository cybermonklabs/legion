package phase

import (
	"fmt"
	"slices"
)

// Sequence is one line of phases an issue may walk: an ordered sub-list of Order, named, which the
// daemon's transition table is generated from (internal/workflow's TableOf). Every issue walked
// Order whole before dispatch://INFRA-3570; an issue's architect may now choose a lighter line for
// a small task, from the library below and nowhere else.
//
// The library lives here, beside Order, and not in internal/workflow, for the reason Order does,
// and three readers rather than two now. internal/config refuses a project setting that names a
// sequence the library does not hold; internal/api reads an issue's sequence to say which phase a
// late completion is of (Of), as the engine does; and the engine generates its table from it.
// Neither of the first two can import internal/workflow. Measured with `go list`:
// `go list -f '{{.TestImports}}' ./internal/workflow/` names both internal/api and internal/config,
// since every test file of package workflow is `package workflow`, so an edge from either of them
// to internal/workflow would close a cycle in the workflow test binary.
type Sequence struct {
	Name   string
	Phases []Phase
}

// DefaultSequenceName is the sequence an issue walks when nothing chose another: the whole of
// Order, which every issue walked before the library existed. An issue record holding no name,
// and a project setting none, mean this one.
const DefaultSequenceName = "default"

// sequences is the library, in the order its names are listed to a reader. Each holds the two
// rules validate states, which TestEverySequenceOfTheLibraryKeepsTheRules holds every entry to.
var sequences = []Sequence{
	{Name: DefaultSequenceName, Phases: []Phase{Planning, Implementing, Testing, Reviewing, Accepting, Retro, Merging}},
	// A small change: the implementer's pull request goes straight to the reviewer, and the
	// reviewer's approval straight to the merger, with no plan, no test phase, no acceptance and
	// no retro (dispatch://INFRA-3570).
	{Name: "light", Phases: []Phase{Implementing, Reviewing, Merging}},
	// Today's line without its planner, for a task whose plan is already written down.
	{Name: "no-planner", Phases: []Phase{Implementing, Testing, Reviewing, Accepting, Retro, Merging}},
}

// Sequences is the library, each sequence a copy, so a caller that edits one leaves the next
// caller's answer whole.
func Sequences() []Sequence {
	out := make([]Sequence, len(sequences))
	for index, sequence := range sequences {
		out[index] = Sequence{Name: sequence.Name, Phases: slices.Clone(sequence.Phases)}
	}
	return out
}

// SequenceNames is the library's names, in its order: what a refusal of a name lists.
func SequenceNames() []string {
	names := make([]string, len(sequences))
	for index, sequence := range sequences {
		names[index] = sequence.Name
	}
	return names
}

// SequenceFor is the library's sequence of that name, and false for every other name, the empty
// one among them: what an empty name on a record or in a setting means is its reader's to say.
func SequenceFor(name string) (Sequence, bool) {
	for _, sequence := range sequences {
		if sequence.Name == name {
			return Sequence{Name: sequence.Name, Phases: slices.Clone(sequence.Phases)}, true
		}
	}
	return Sequence{}, false
}

// DefaultSequence is the library's default (DefaultSequenceName).
func DefaultSequence() Sequence {
	sequence, _ := SequenceFor(DefaultSequenceName)
	return sequence
}

// Holds says whether the sequence walks p.
func (s Sequence) Holds(p Phase) bool {
	return slices.Contains(s.Phases, p)
}

// validate states the two rules every sequence keeps (dispatch://INFRA-3570). It is a sub-list of
// Order in Order's own order, with no phase twice, since a move back and its counts are read
// against Order (MovesBack, BackwardPairs) and a line that reordered them would count a forward
// step as a move back. It holds implementing, reviewing and merging, because the review before the
// landing is the one rule no lighter workflow may drop (dispatch://INFRA-3577/spec R34): an issue
// whose line skipped the reviewer would merge work nobody had reviewed.
func (s Sequence) validate() error {
	if s.Name == "" {
		return fmt.Errorf("a sequence has a name, and %v has none", s.Phases)
	}
	last := -1
	for _, p := range s.Phases {
		at := slices.Index(Order, p)
		if at < 0 {
			return fmt.Errorf("sequence %s walks %s, which is no phase of Order %v", s.Name, p, Order)
		}
		if at <= last {
			return fmt.Errorf("sequence %s walks %v, which is not in Order's order %v", s.Name, s.Phases, Order)
		}
		last = at
	}
	for _, required := range []Phase{Implementing, Reviewing, Merging} {
		if !s.Holds(required) {
			return fmt.Errorf("sequence %s walks %v without %s: every sequence holds implementing, reviewing and merging, the review before the landing (dispatch://INFRA-3577/spec R34)",
				s.Name, s.Phases, required)
		}
	}
	return nil
}
