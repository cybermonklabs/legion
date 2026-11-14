package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The lead of a tree nobody has architected, shared by every pinning of that shape.
const freshLead = "LEGION-208 is yours and its tree is open at generation 2, holding an admission slot while nothing in it runs. " +
	"Take it up in this turn, as your role says: claim it in Dispatch, have its root spec written and every child of your decomposition filed, then"

// The lead of a tree that ran at an earlier generation, shared the same way.
const readmittedLead = "LEGION-208 is yours and its tree ran at an earlier generation and is open again at generation 2, " +
	"holding an admission slot while nothing in it runs. Re-admission cleared the old generation and its design gate with it. " +
	"Take it up in this turn, as your role says: claim it in Dispatch, then"

const startTail = " Nothing else starts this tree, and no message is coming."

// specGated is a deployment that gates the spec step alone.
var specGated = []record.DesignStep{record.DesignStepSpec}

// rootAt is the root every case of this file stands on, standing in p.
func rootAt(p phase.Phase) record.Issue {
	return record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Generation: 2, Phase: p}
}

// admittedRoot is that root at the phase every tree the workflow opens stands in.
func admittedRoot() record.Issue { return rootAt(phase.Admitted) }

// registeredSpec is the gate rows of a root that registered its spec at version 1, which no
// person has answered.
func registeredSpec() map[record.DesignStep]record.DesignGate {
	return map[record.DesignStep]record.DesignGate{
		record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 1}}
}

// decidedOnTheTree is what a person decided on the tree while nobody in it could hear them.
const decidedOnTheTree = "While nobody in LEGION-208's tree could hear it, a person decided on the tree: " +
	"dispatch://LEGION-208/spec (design-approved by user:alice). Read each before you continue."

// childDecision is a decision kept on LEGION-209, a child of the tree, which a narrowing keeps.
func childDecision() record.KeptDecisions {
	return record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: "dispatch://LEGION-209/ask/ask-1",
		At: time.Unix(0, 0).UTC(), Issue: "LEGION-209", Decision: record.DecisionAskAnswered, Actor: "user:alice"}}}
}

// The whole text of each of the four shapes, pinned: this task is the one thing a fresh
// architect's first turn has to go on, so what it says is the whole of what that turn knows. Each
// shape names the gate the deployment arms, where its text speaks of the gate at all, so one
// gated step, two and none are all pinned.
func TestTheArchitectTaskOfEachShape(t *testing.T) {
	two := []record.DesignStep{record.DesignStepDecisions, record.DesignStepCriteria}
	for _, tc := range []struct {
		name  string
		start ArchitectStart
		want  string
	}{
		{
			name:  "a tree nobody has architected, under a deployment that gates the spec",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}},
			want:  freshLead + " request approval of its primary document and register that step of its design gate." + startTail,
		},
		{
			name:  "a tree nobody has architected, under a deployment that gates two steps",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: two}},
			want: freshLead + " request approval of each document this deployment gates and register each of their steps of its design gate: " +
				"its decisions document and its Verification document." + startTail,
		},
		{
			name:  "a tree nobody has architected, under a deployment that gates no step",
			start: ArchitectStart{Root: admittedRoot()},
			want:  freshLead + " register that spec at its current version, which opens the design gate with no approval." + startTail,
		},
		{
			name: "a root a hand-over carried in",
			start: ArchitectStart{Root: rootAt(phase.Implementing),
				Carried: true, Gate: record.TreeGate{Steps: specGated}},
			want: "LEGION-208 is yours and stands in implementing, the phase a hand-over carried it in at, with the design gate it brought. " +
				"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. " +
				"Decompose nothing and release no child." + startTail,
		},
		{
			name: "a root whose one gated step a person has approved",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 2, ApprovedVersion: new(2)}}}},
			want: "LEGION-208 is yours and its design gate already stands part way: " +
				"its primary document is registered at version 2 and a person has approved that version. " +
				"Take it up in this turn, as your role says: claim it in Dispatch, carry that gate on from where it stands " +
				"and take the tree up from there." + startTail,
		},
		{
			name: "a root whose two gated steps stand at a changes request and at nothing",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: two, Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepDecisions: {Issue: "LEGION-208", Step: record.DesignStepDecisions, LatestVersion: 3,
					ToldChangesVersion: new(3), ToldChangesReason: "name the alternative"}}}},
			want: "LEGION-208 is yours and its design gate already stands part way: " +
				"its decisions document is registered at version 3 and a person has asked for changes to it; " +
				"its Verification document is not registered. " +
				"Take it up in this turn, as your role says: claim it in Dispatch, carry that gate on from where it stands " +
				"and take the tree up from there." + startTail,
		},
		{
			name: "a root whose registered step no person has answered",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 4, ApprovedVersion: new(2)}}}},
			want: "LEGION-208 is yours and its design gate already stands part way: " +
				"its primary document is registered at version 4 and no person has answered it. " +
				"Take it up in this turn, as your role says: claim it in Dispatch, carry that gate on from where it stands " +
				"and take the tree up from there." + startTail,
		},
		{
			name: "a root holding a row under a deployment that gates no step",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 1, ApprovedVersion: new(1)}}}},
			want: "LEGION-208 is yours and its design gate already stands part way: " +
				"its primary document is registered at version 1, which opened the gate with no approval. " +
				"Take it up in this turn, as your role says: claim it in Dispatch, carry that gate on from where it stands " +
				"and take the tree up from there." + startTail,
		},
		{
			name:  "a tree that ran at an earlier generation, under a deployment that gates the spec",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Launch: 1},
			want:  readmittedLead + " request approval of its primary document and register that step of its design gate again. Decompose nothing anew." + startTail,
		},
		{
			name:  "a tree that ran at an earlier generation, under a deployment that gates two steps",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: two}, Launch: 1},
			want: readmittedLead + " request approval of each document this deployment gates and register each of their steps of its design gate again: " +
				"its decisions document and its Verification document. Decompose nothing anew." + startTail,
		},
		{
			name:  "a tree that ran at an earlier generation, under a deployment that gates no step",
			start: ArchitectStart{Root: admittedRoot(), Launch: 1},
			want:  readmittedLead + " register that spec again at its current version, which opens the design gate with no approval. Decompose nothing anew." + startTail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArchitectStartTask(tc.start); got != tc.want {
				t.Fatalf("the architect's start task =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// Every label is a label of one shape: a task carries its own shape's label, carries no other
// shape's, and ends with the tail that tells the architect nothing else starts its tree. The
// decomposition is asked for by the one shape that opens a tree.
func TestTheArchitectTaskLabelsOfEachShape(t *testing.T) {
	labels := []string{ArchitectFreshTreeLabel, ArchitectCarriedPhaseLabel, ArchitectGateRowsLabel, ArchitectReadmittedLabel}
	for i, label := range labels {
		for j, other := range labels {
			if i != j && strings.Contains(label, other) {
				t.Fatalf("the label %q holds the label %q, so a test cannot tell the two shapes apart", label, other)
			}
		}
		if strings.Contains(label, ArchitectDecompositionLabel) || strings.Contains(ArchitectDecompositionLabel, label) {
			t.Fatalf("the label %q and the decomposition label %q hold one another", label, ArchitectDecompositionLabel)
		}
	}
	for _, tc := range []struct {
		name       string
		start      ArchitectStart
		label      string
		decomposes bool
	}{
		{
			name:       "a tree nobody has architected",
			start:      ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}},
			label:      ArchitectFreshTreeLabel,
			decomposes: true,
		},
		{
			name: "a root a hand-over carried in",
			start: ArchitectStart{Root: rootAt(phase.Accepting),
				Carried: true, Gate: record.TreeGate{Steps: specGated}},
			label: ArchitectCarriedPhaseLabel,
		},
		{
			name:  "a root that holds design gate rows",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: registeredSpec()}},
			label: ArchitectGateRowsLabel,
		},
		{
			name:  "a tree that ran at an earlier generation",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Launch: 1},
			label: ArchitectReadmittedLabel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := ArchitectStartTask(tc.start)
			if !strings.Contains(task, tc.label) {
				t.Fatalf("the task %q does not carry its own label %q", task, tc.label)
			}
			for _, other := range labels {
				if other != tc.label && strings.Contains(task, other) {
					t.Fatalf("the task %q carries another shape's label %q", task, other)
				}
			}
			if asks := strings.Contains(task, ArchitectDecompositionLabel); asks != tc.decomposes {
				t.Fatalf("the task %q asks for a decomposition = %v, want %v", task, asks, tc.decomposes)
			}
			if !strings.HasSuffix(task, startTail) {
				t.Fatalf("the task %q does not end with the first-task tail", task)
			}
		})
	}
}

// The rule that chooses the shape, case by case. Three readings of it are easy to get wrong: a
// root at admitted whose architect claim already stands at the root's own generation and whose
// tree has never run is a tree nobody has architected, since the first act is the act it still
// owes; a root a hand-over carried in stands in the phase that came with it whatever its gate
// rows say; and a carried root that never left admitted has no phase of its own to take up, so it
// reads as any other root standing there.
func TestTheArchitectTaskShapeRule(t *testing.T) {
	registered := registeredSpec()
	carried := func(gate record.TreeGate, taken bool) ArchitectStart {
		root := rootAt(phase.Reviewing)
		root.TakenBack = taken
		return ArchitectStart{Root: root, Carried: true, Gate: gate}
	}
	takenBack := func() record.Issue {
		root := admittedRoot()
		root.TakenBack = true
		return root
	}
	for _, tc := range []struct {
		name  string
		start ArchitectStart
		label string
	}{
		{
			name:  "a tree with no architect claim and no gate row",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}},
			label: ArchitectFreshTreeLabel,
		},
		{
			name:  "an architect claim already at the root's own generation, for a tree that never ran",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Launch: 2},
			label: ArchitectFreshTreeLabel,
		},
		{
			name:  "an architect claim one generation behind",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Launch: 1},
			label: ArchitectReadmittedLabel,
		},
		{
			name:  "a run that stood in a phase of its own, with no architect claim at all",
			start: ArchitectStart{Root: takenBack(), Gate: record.TreeGate{Steps: specGated}},
			label: ArchitectReadmittedLabel,
		},
		{
			name:  "a run that stood in a phase of its own, whose claim stands at this generation",
			start: ArchitectStart{Root: takenBack(), Gate: record.TreeGate{Steps: specGated}, Launch: 2},
			label: ArchitectReadmittedLabel,
		},
		{
			name:  "a gate row beats a tree that ran at an earlier generation",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: registered}, Launch: 1},
			label: ArchitectGateRowsLabel,
		},
		{
			name:  "a carried root with no gate row",
			start: carried(record.TreeGate{Steps: specGated}, false),
			label: ArchitectCarriedPhaseLabel,
		},
		{
			name:  "a carried root whose gate rows the carry brought",
			start: carried(record.TreeGate{Steps: specGated, Rows: registered}, false),
			label: ArchitectCarriedPhaseLabel,
		},
		{
			name:  "a carried root whose run stood in a phase of its own",
			start: carried(record.TreeGate{Steps: specGated, Rows: registered}, true),
			label: ArchitectCarriedPhaseLabel,
		},
		{
			name:  "a carried root standing at admitted, with no gate row",
			start: ArchitectStart{Root: admittedRoot(), Carried: true, Gate: record.TreeGate{Steps: specGated}},
			label: ArchitectFreshTreeLabel,
		},
		{
			name:  "a carried root standing at admitted, holding the gate rows the carry brought",
			start: ArchitectStart{Root: admittedRoot(), Carried: true, Gate: record.TreeGate{Steps: specGated, Rows: registered}},
			label: ArchitectGateRowsLabel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if task := ArchitectStartTask(tc.start); !strings.Contains(task, tc.label) {
				t.Fatalf("the architect's start task is %q, want the shape labelled %q", task, tc.label)
			}
		})
	}
}

// The derivation gives no task for a root past admitted under its own power: one in a working
// phase, one waiting on a person and one in accepting. Each of those is told its work by its
// phase's own role, by the resume a person's answer composes, or by the notice that moved it, so
// a first act stated here would be false. The answer is what a person decided on the tree and
// nothing else, the empty string where nobody decided anything, and it carries no first-task tail.
func TestTheArchitectTaskIsNoneForARootPastAdmitted(t *testing.T) {
	registered := registeredSpec()
	for _, tc := range []struct {
		name  string
		stand phase.Phase
	}{
		{"a root past admitted under its own power", phase.Planning},
		{"a root waiting on a person", phase.AwaitingPerson},
		{"a root in accepting", phase.Accepting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := ArchitectStart{Root: rootAt(tc.stand),
				Gate: record.TreeGate{Steps: specGated, Rows: registered}, Launch: 1}
			if got := ArchitectStartTask(start); got != "" {
				t.Fatalf("the architect's start task = %q, want none: this root is no root the derivation serves", got)
			}
			start.Decided = decidedOnTheTree
			got := ArchitectStartTask(start)
			if got != decidedOnTheTree {
				t.Fatalf("the architect's start task = %q, want the decided text alone %q", got, decidedOnTheTree)
			}
			if strings.HasSuffix(got, startTail) {
				t.Fatalf("the task %q carries the first-task tail, want none", got)
			}
		})
	}
}

// A person's decision on the tree is read before the work, so it leads whichever shape the root
// gets: the decided text, a blank line, then the shape's own text.
func TestTheArchitectTaskNamesAKeptDecisionFirstInEveryShape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start ArchitectStart
	}{
		{"a tree nobody has architected", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}}},
		{"a root a hand-over carried in", ArchitectStart{Root: rootAt(phase.Planning), Carried: true, Gate: record.TreeGate{Steps: specGated}}},
		{"a root that holds design gate rows", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: registeredSpec()}}},
		{"a tree that ran at an earlier generation", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Launch: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bare := ArchitectStartTask(tc.start)
			tc.start.Decided = decidedOnTheTree
			if got, want := ArchitectStartTask(tc.start), decidedOnTheTree+"\n\n"+bare; got != want {
				t.Fatalf("the architect's start task =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// Narrowing a root architect's held task writes it again from the decisions it still names. A
// task of an entry carried the first-task instruction and keeps it, in the shape the record gives
// the root now: a decision leaving the tree says nothing about whether the tree has been taken
// up, and a rewrite that dropped the instruction would hand a fresh architect the empty task the
// instruction exists to replace. A task of no entry carried no instruction, and is the decisions'
// own text alone.
//
// What decides it is the entry and not the held task's text, which the narrowing is not given:
// whatever wording a release wrote a derived task in, the instruction is written again, so a
// closing sentence reworded by a later release costs no first act.
func TestNarrowingARootsArchitectTaskKeepsItsShape(t *testing.T) {
	stays := childDecision()
	decided := KeptDecisionsTask("LEGION-208", stays)
	if decided == "" {
		t.Fatal("the kept decisions name nothing, so the narrowing has nothing to pin")
	}
	for _, tc := range []struct {
		name  string
		start ArchitectStart
		label string
	}{
		{
			name:  "a tree nobody has architected",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}},
			label: ArchitectFreshTreeLabel,
		},
		{
			name: "a root a hand-over carried in",
			start: ArchitectStart{Root: rootAt(phase.Testing),
				Carried: true, Gate: record.TreeGate{Steps: specGated}},
			label: ArchitectCarriedPhaseLabel,
		},
		{
			name:  "a root that holds design gate rows",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: registeredSpec()}},
			label: ArchitectGateRowsLabel,
		},
		{
			name:  "a tree that ran at an earlier generation",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Launch: 1},
			label: ArchitectReadmittedLabel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole := tc.start
			whole.Decided = decided
			narrowed := NarrowRootTask(tc.start, heldEntry, stays)
			if want := ArchitectStartTask(whole); narrowed != want {
				t.Fatalf("the narrowed task =\n%q\nwant\n%q", narrowed, want)
			}
			if !strings.Contains(narrowed, tc.label) {
				t.Fatalf("the narrowed task lost the shape labelled %q", tc.label)
			}
			if got := NarrowRootTask(tc.start, 0, stays); got != decided {
				t.Fatalf("the narrowed task of a held task of no entry = %q, want the decisions' own text %q", got, decided)
			}
		})
	}
}

// heldEntry is the phase entry a held task of these cases is of: the entry the start that queued
// it carried, which a start the derivation wrote carries wherever it hands the architect work.
const heldEntry int64 = 3

// A root that moved on while its task waited is served no first task, so the narrowing of a held
// task of an entry writes the kept decisions' own text alone: the tree has been taken up, and its
// phase's own role says what the work is.
func TestNarrowingARootsArchitectTaskThatMovedOnDropsTheInstruction(t *testing.T) {
	stays := childDecision()
	decided := KeptDecisionsTask("LEGION-208", stays)
	moved := ArchitectStart{Root: rootAt(phase.Planning), Gate: record.TreeGate{Steps: specGated}}
	if got := NarrowRootTask(moved, heldEntry, stays); got != decided {
		t.Fatalf("the narrowed task of a root now in planning = %q, want the kept decisions' own text %q", got, decided)
	}
}

// A narrowing that leaves no decision at all writes the empty task its caller retires rather than
// sends, where the held task is of no entry and so carried no first-task instruction.
func TestNarrowingARootsKeptDecisionsToNothingWritesNoTask(t *testing.T) {
	start := ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}}
	if got := NarrowRootTask(start, 0, record.KeptDecisions{}); got != "" {
		t.Fatalf("the narrowed task = %q, want the empty task its caller retires", got)
	}
}
