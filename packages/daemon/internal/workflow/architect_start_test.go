package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The lead of a tree nobody has architected, shared by every pinning of that shape.
const freshLead = "LEGION-208 is yours and its tree is open at generation 2, holding an admission slot while nothing in it runs. " +
	"Take it up in this turn, as your role says: claim it in Dispatch, have its root spec written and every child of your decomposition filed, then"

// The lead of a tree that ran at an earlier generation, shared the same way.
const readmittedLead = "LEGION-208 is yours and its tree ran at an earlier generation and is open again at generation 2, " +
	"holding an admission slot. Re-admission cleared the old generation and its design gate with it. " +
	"Take it up in this turn, as your role says: claim it in Dispatch, then"

// The closing sentence of each shape: what starts the tree's next work, and whether more is coming
// to the architect, as each holds of the root its shape is written for. A carried root's depends on
// the phase it stands in, so it has one for a phase a worker works, one for accepting, which is the
// architect's own, one for held, which no worker works, and one for merging once its pull request
// has merged, which the merge itself ended.
const (
	freshClosing         = " Nothing else starts this tree, and no message is coming."
	carriedClosing       = " The daemon starts that phase's own worker itself."
	carriedOwnClosing    = " That phase is your own, so the daemon starts no worker for it."
	carriedHeldClosing   = " No worker works that phase, so the daemon starts none for it."
	carriedMergedClosing = " Its pull request has merged, so the daemon starts no worker for that phase, and the root stands " +
		"there until the rest of its tree has left the workflow, when the daemon closes it."
	gateStandingClosing  = " Its own first phase starts only once its design gate opens."
	gateOpenClosing      = " The daemon starts its first phase itself once it holds an admission slot and neither an open child under it nor an issue it is blocked by holds that phase back."
	readmittedClosing    = " Its own first phase starts only once its design gate opens again."
	gateOpenInstructions = "Take it up in this turn, as your role says: claim it in Dispatch and take the tree up from where it stands."
)

// closings is every closing sentence above, which no task of the second rule carries.
var closings = []string{freshClosing, carriedClosing, carriedOwnClosing, carriedHeldClosing, carriedMergedClosing,
	gateStandingClosing, gateOpenClosing, readmittedClosing}

// specGated is a deployment that gates the spec step alone.
var specGated = []record.DesignStep{record.DesignStepSpec}

// rootAt is the root every case of this file stands on, standing in p.
func rootAt(p phase.Phase) record.Issue {
	return record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Generation: 2, Phase: p}
}

// admittedRoot is that root at the phase every tree the workflow opens stands in.
func admittedRoot() record.Issue { return rootAt(phase.Admitted) }

// firstGenerationRoot is that root at admitted in its tree's first run, which no earlier
// generation stands behind.
func firstGenerationRoot() record.Issue {
	root := admittedRoot()
	root.Generation = 1
	return root
}

// registeredSpec is the gate rows of a root that registered its spec at version 1, which no
// person has answered.
func registeredSpec() map[record.DesignStep]record.DesignGate {
	return map[record.DesignStep]record.DesignGate{
		record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 1}}
}

// approvedSpec is the gate rows of a root whose spec a person approved at version 1, the one step
// the deployment gates (specGated), so its design gate is open.
func approvedSpec() map[record.DesignStep]record.DesignGate {
	return map[record.DesignStep]record.DesignGate{
		record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 1, ApprovedVersion: new(1)}}
}

// decidedOnTheTree is what a person decided on the tree while nobody in it could hear them.
const decidedOnTheTree = "While nobody in LEGION-208's tree could hear it, a person decided on the tree: " +
	"dispatch://LEGION-208/spec (design-approved by user:alice). Read each before you continue."

// childDecision is a decision kept on LEGION-209, a child of the tree, which a narrowing keeps.
func childDecision() record.KeptDecisions {
	return record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: "dispatch://LEGION-209/ask/ask-1",
		At: time.Unix(0, 0).UTC(), Issue: "LEGION-209", Decision: record.DecisionAskAnswered, Actor: "user:alice"}}}
}

// The whole text of each shape, pinned: this task is the one thing a fresh architect's first turn
// has to go on, so what it says is the whole of what that turn knows. Each shape names the gate
// the deployment arms, where its text speaks of the gate at all, so one gated step, two and none
// are all pinned. Each ends in a closing sentence of its own, and every clause of it holds of the
// root it is sent to: no root but a fresh tree's is told that nothing else starts it and no
// message is coming, since a carried root's phase worker, an open gate's first phase, a person's
// review of a registered step and the re-admitted members of a tree that ran each start something
// or bring a message.
func TestTheArchitectTaskOfEachShape(t *testing.T) {
	t.Parallel()
	two := []record.DesignStep{record.DesignStepDecisions, record.DesignStepCriteria}
	for _, tc := range []struct {
		name  string
		start ArchitectStart
		want  string
	}{
		{
			name:  "a tree nobody has architected, under a deployment that gates the spec",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}},
			want:  freshLead + " request approval of its primary document and register that step of its design gate." + freshClosing,
		},
		{
			name:  "a tree nobody has architected, under a deployment that gates two steps",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: two}},
			want: freshLead + " request approval of each document this deployment gates and register each of their steps of its design gate: " +
				"its decisions document and its Verification document." + freshClosing,
		},
		{
			name:  "a tree nobody has architected, under a deployment that gates no step",
			start: ArchitectStart{Root: admittedRoot()},
			want:  freshLead + " register that spec at its current version, which opens the design gate with no approval." + freshClosing,
		},
		{
			name:  "a tree nobody has architected at generation 1, a member of which runs",
			start: ArchitectStart{Root: firstGenerationRoot(), Gate: record.TreeGate{Steps: specGated}, MembersRun: true},
			want: "LEGION-208 is yours and its tree is open at generation 1, holding an admission slot. " +
				"Take it up in this turn, as your role says: claim it in Dispatch, have its root spec written and every child of your decomposition filed, then" +
				" request approval of its primary document and register that step of its design gate." + gateStandingClosing,
		},
		{
			name: "a root a hand-over carried in",
			start: ArchitectStart{Root: rootAt(phase.Implementing),
				Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want: "LEGION-208 is yours and stands in implementing, in the run a hand-over carried it into, with the design gate that hand-over brought. " +
				"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. " +
				"Decompose nothing and release no child." + carriedClosing,
		},
		{
			name: "a root a hand-over carried in that brought no design gate",
			start: ArchitectStart{Root: rootAt(phase.Implementing),
				Carried: true, Gate: record.TreeGate{Steps: specGated}},
			want: "LEGION-208 is yours and stands in implementing, in the run a hand-over carried it into, and that hand-over brought no design gate. " +
				"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. " +
				"Decompose nothing and release no child." + carriedClosing,
		},
		{
			name: "a root a hand-over carried in that has moved on to a phase of that run",
			start: ArchitectStart{Root: rootAt(phase.Testing),
				Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want: "LEGION-208 is yours and stands in testing, in the run a hand-over carried it into, with the design gate that hand-over brought. " +
				"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. " +
				"Decompose nothing and release no child." + carriedClosing,
		},
		{
			name: "a root a hand-over carried into accepting, the architect's own phase",
			start: ArchitectStart{Root: rootAt(phase.Accepting),
				Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want: "LEGION-208 is yours and stands in accepting, in the run a hand-over carried it into, with the design gate that hand-over brought. " +
				"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. " +
				"Decompose nothing and release no child." + carriedOwnClosing,
		},
		{
			name: "a root a hand-over carried in held",
			start: ArchitectStart{Root: rootAt(phase.Held),
				Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want: "LEGION-208 is yours and stands in held, in the run a hand-over carried it into, with the design gate that hand-over brought. " +
				"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. " +
				"Decompose nothing and release no child." + carriedHeldClosing,
		},
		{
			name: "a root a hand-over carried into merging whose pull request has merged",
			start: ArchitectStart{Root: rootAt(phase.Merging),
				Carried: true, GateCarried: true, PhaseFinished: true, Gate: record.TreeGate{Steps: specGated}},
			want: "LEGION-208 is yours and stands in merging, in the run a hand-over carried it into, with the design gate that hand-over brought. " +
				"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. " +
				"Decompose nothing and release no child." + carriedMergedClosing,
		},
		{
			name: "a root a hand-over carried into merging whose pull request has not merged",
			start: ArchitectStart{Root: rootAt(phase.Merging),
				Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want: "LEGION-208 is yours and stands in merging, in the run a hand-over carried it into, with the design gate that hand-over brought. " +
				"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. " +
				"Decompose nothing and release no child." + carriedClosing,
		},
		{
			name: "a root whose one gated step a person has approved, so its gate is open",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 2, ApprovedVersion: new(2)}}}},
			want: "LEGION-208 is yours and its design gate is open: " +
				"its primary document is registered at version 2 and a person has approved that version. " +
				gateOpenInstructions + gateOpenClosing,
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
				"and take the tree up from there." + gateStandingClosing,
		},
		{
			name: "a root whose registered step no person has answered",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 4, ApprovedVersion: new(2)}}}},
			want: "LEGION-208 is yours and its design gate already stands part way: " +
				"its primary document is registered at version 4 and no person has answered it. " +
				"Take it up in this turn, as your role says: claim it in Dispatch, carry that gate on from where it stands " +
				"and take the tree up from there." + gateStandingClosing,
		},
		{
			name: "a root holding a row under a deployment that gates no step, so its gate is open",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 1, ApprovedVersion: new(1)}}}},
			want: "LEGION-208 is yours and its design gate is open: " +
				"its primary document is registered at version 1, which opened the gate with no approval. " +
				gateOpenInstructions + gateOpenClosing,
		},
		{
			name:  "a tree that ran at an earlier generation, under a deployment that gates the spec",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Served: 1},
			want:  readmittedLead + " request approval of its primary document and register that step of its design gate again. Decompose nothing anew." + readmittedClosing,
		},
		{
			name:  "a tree that ran at an earlier generation, under a deployment that gates two steps",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: two}, Served: 1},
			want: readmittedLead + " request approval of each document this deployment gates and register each of their steps of its design gate again: " +
				"its decisions document and its Verification document. Decompose nothing anew." + readmittedClosing,
		},
		{
			name:  "a tree that ran at an earlier generation, under a deployment that gates no step",
			start: ArchitectStart{Root: admittedRoot(), Served: 1},
			want:  readmittedLead + " register that spec again at its current version, which opens the design gate with no approval. Decompose nothing anew." + readmittedClosing,
		},
		{
			name:  "a tree re-admitted while a member of it runs, whose architect claim served nothing",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, MembersRun: true},
			want:  readmittedLead + " request approval of its primary document and register that step of its design gate again. Decompose nothing anew." + readmittedClosing,
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
// shape's, and ends with its own shape's closing sentence. The decomposition is asked for by the
// one shape that opens a tree.
func TestTheArchitectTaskLabelsOfEachShape(t *testing.T) {
	t.Parallel()
	labels := []string{ArchitectFreshTreeLabel, ArchitectCarriedPhaseLabel, ArchitectGateRowsLabel, ArchitectGateOpenLabel,
		ArchitectReadmittedLabel}
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
		closing    string
		decomposes bool
	}{
		{
			name:       "a tree nobody has architected",
			start:      ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}},
			label:      ArchitectFreshTreeLabel,
			closing:    freshClosing,
			decomposes: true,
		},
		{
			name: "a root a hand-over carried in",
			start: ArchitectStart{Root: rootAt(phase.Accepting),
				Carried: true, Gate: record.TreeGate{Steps: specGated}},
			label:   ArchitectCarriedPhaseLabel,
			closing: carriedOwnClosing,
		},
		{
			name:    "a root that holds design gate rows",
			start:   ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: registeredSpec()}},
			label:   ArchitectGateRowsLabel,
			closing: gateStandingClosing,
		},
		{
			name:    "a root whose design gate is open",
			start:   ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: approvedSpec()}},
			label:   ArchitectGateOpenLabel,
			closing: gateOpenClosing,
		},
		{
			name:    "a tree that ran at an earlier generation",
			start:   ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Served: 1},
			label:   ArchitectReadmittedLabel,
			closing: readmittedClosing,
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
			if !strings.HasSuffix(task, tc.closing) {
				t.Fatalf("the task %q does not end with its shape's closing sentence %q", task, tc.closing)
			}
		})
	}
}

// The rule that chooses the shape, case by case. Six readings of it are easy to get wrong: a
// root at admitted whose architect claim served the root's own generation and no phase entry, and
// whose run never stood in a phase of its own, is a tree nobody has architected, since a task of
// this run and of no entry, a wake's, says nothing of an earlier run; a claim that served a phase
// entry ran the tree before whatever run it served, which is the claim the upgrade leaves a
// re-admitted tree of the release before (served entry 1, no run) and a claim that served a wake's
// task of the root's own generation after an entry of an earlier one; a root re-admitted while a
// member of its tree runs ran at an earlier generation though its claim served nothing, and the
// same member at generation 1 leaves the tree one nobody has architected, told nothing that
// member makes false; a root a hand-over carried in past admitted takes the carried shape
// whatever its gate rows say; a carried root that never left admitted has no phase of its own to
// take up, so it reads as any other root standing there; and a root whose gate rows open its gate
// reads apart from one whose rows stand part way, since the daemon starts its first phase itself.
func TestTheArchitectTaskShapeRule(t *testing.T) {
	t.Parallel()
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
		// never is what the task must not say, where the case names it.
		never []string
	}{
		{
			name:  "a tree with no architect claim and no gate row",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}},
			label: ArchitectFreshTreeLabel,
		},
		{
			name:  "an architect claim that served the root's own generation and no entry, for a tree that never ran",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Served: 2},
			label: ArchitectFreshTreeLabel,
		},
		{
			name:  "an architect claim the upgrade settled: served entry 1 and no run",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, ServedEntry: 1},
			label: ArchitectReadmittedLabel,
		},
		{
			name:  "an architect claim that served an earlier entry and a task of the root's own generation",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Served: 2, ServedEntry: 1},
			label: ArchitectReadmittedLabel,
		},
		{
			name:  "a root re-admitted while a member of its tree runs, whose architect claim served nothing",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, MembersRun: true},
			label: ArchitectReadmittedLabel,
			never: []string{ArchitectFreshTreeLabel, ArchitectDecompositionLabel, "while nothing in it runs", freshClosing},
		},
		{
			name:  "a root at generation 1 while a member of its tree runs, whose architect claim served nothing",
			start: ArchitectStart{Root: firstGenerationRoot(), Gate: record.TreeGate{Steps: specGated}, MembersRun: true},
			label: ArchitectFreshTreeLabel,
			never: []string{"while nothing in it runs", freshClosing},
		},
		{
			name:  "an architect claim that served the generation before",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Served: 1},
			label: ArchitectReadmittedLabel,
		},
		{
			name:  "a run that stood in a phase of its own, with no architect claim at all",
			start: ArchitectStart{Root: takenBack(), Gate: record.TreeGate{Steps: specGated}},
			label: ArchitectReadmittedLabel,
		},
		{
			name:  "a run that stood in a phase of its own, whose claim served this generation",
			start: ArchitectStart{Root: takenBack(), Gate: record.TreeGate{Steps: specGated}, Served: 2},
			label: ArchitectReadmittedLabel,
		},
		{
			name:  "a gate row beats a tree that ran at an earlier generation",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: registered}, Served: 1},
			label: ArchitectGateRowsLabel,
		},
		{
			name:  "an open gate beats a tree that ran at an earlier generation",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: approvedSpec()}, Served: 1},
			label: ArchitectGateOpenLabel,
		},
		{
			name:  "a carried root whose open gate the carry brought",
			start: carried(record.TreeGate{Steps: specGated, Rows: approvedSpec()}, false),
			label: ArchitectCarriedPhaseLabel,
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
		{
			name:  "a carried root standing at admitted, whose gate the carry brought open",
			start: ArchitectStart{Root: admittedRoot(), Carried: true, Gate: record.TreeGate{Steps: specGated, Rows: approvedSpec()}},
			label: ArchitectGateOpenLabel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := ArchitectStartTask(tc.start)
			if !strings.Contains(task, tc.label) {
				t.Fatalf("the architect's start task is %q, want the shape labelled %q", task, tc.label)
			}
			for _, untrue := range tc.never {
				if strings.Contains(task, untrue) {
					t.Fatalf("the architect's start task is %q; it says %q, which is false of a tree a member of which runs", task, untrue)
				}
			}
		})
	}
}

// The fourth rule reads what the tree's architect claim served, never the claim's launch count.
// The claim is kept from one run to the next, and its generation (claims.generation) counts its
// launches, so it can stand above the root's generation after relaunches in an earlier run, or
// below it where the claim was never relaunched, whatever the tree has run. Each case is a root
// re-admitted at generation 3, at admitted with no design gate row and no run of its own phase,
// read from the record through ArchitectStartOf, which reads the claim's serving_generation and
// served_entry (record.Store.ArchitectServed):
//   - a claim that launched five times and last served generation 2 ran the tree at an earlier
//     generation, so the task is of the fourth rule's shape;
//   - a claim that served nothing took no task of any run, so the tree is one nobody has
//     architected;
//   - a claim that served generation 3 and no entry took a task of the run the root stands at and
//     of no entry, a wake's, which says nothing of an earlier run, so the task is not of the
//     fourth rule's shape;
//   - a claim the upgrade settled, served entry 1 and no run, ran the tree on the release before,
//     so the task is of the fourth rule's shape;
//   - a claim that served entry 1 and then generation 3, a wake's task of this run, ran the tree
//     at an earlier generation all the same, so the task is of the fourth rule's shape.
func TestTheArchitectTaskReadsTheRunItsArchitectServedAndNotItsLaunches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		launches, served uint64
		servedEntry      int64
		label            string
	}{
		{"a claim that launched five times and served generation 2", 5, 2, 0, ArchitectReadmittedLabel},
		{"a claim that launched twice and served nothing", 2, 0, 0, ArchitectFreshTreeLabel},
		{"a claim that launched twice and served generation 3, the run the root stands at, and no entry", 2, 3, 0, ArchitectFreshTreeLabel},
		{"a claim the upgrade settled, served entry 1 and no run", 2, 0, 1, ArchitectReadmittedLabel},
		{"a claim that served entry 1 and then generation 3, the run the root stands at", 2, 3, 1, ArchitectReadmittedLabel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			pool := migratedPool(t)
			root := architectEntryRoot()
			root.Generation = 3
			seedIssue(t, pool, root)
			project, err := claim.ProjectToken(root.Project)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
				state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation, given_entry, served_entry)
				values ($1, $2, $3, $4, 'architect', $5, 'session-architect', '', 'ready', 0, 0, 0, 0, $6, $7, $7)`,
				string(wakeToken(t, claim.RoleArchitect)), project, root.Tree, root.Key, int64(tc.launches), int64(tc.served),
				tc.servedEntry); err != nil {
				t.Fatalf("seed the architect's claim: %v", err)
			}
			var task string
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				start, err := ArchitectStartOf(ctx, tx, record.NewStore(), specGated, root)
				task = ArchitectStartTask(start)
				return err
			}); err != nil {
				t.Fatalf("read what the architect's first task is derived from: %v", err)
			}
			if !strings.Contains(task, tc.label) {
				t.Fatalf("the architect's start task is %q, want the shape labelled %q", task, tc.label)
			}
			if tc.label != ArchitectReadmittedLabel && strings.Contains(task, ArchitectReadmittedLabel) {
				t.Fatalf("the architect's start task is %q, want no task of the fourth rule's shape: its claim took no task of an earlier run", task)
			}
		})
	}
}

// Whether a member of the tree runs is read from the record (ArchitectStartOf, through
// record.Store.TreeMembersRun): an issue of the root's tree other than the root, standing past
// phase.Admitted in the workflow. Each case is the root of architectEntryRoot at admitted, at
// generation 2, whose architect claim served nothing, beside one other issue:
//   - a child in planning, in the workflow, runs, so the tree ran at an earlier generation;
//   - a child at admitted waits for its first phase and runs nothing;
//   - a child in planning that Dispatch has taken out of the workflow runs nothing;
//   - a child whose phase is done runs nothing, whatever its status says;
//   - an issue in planning of another tree is no member of this one.
func TestTheArchitectStartReadsWhetherAMemberOfTheTreeRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		tree   string
		stands phase.Phase
		status string
		runs   bool
	}{
		{name: "a child in planning", stands: phase.Planning, status: "in_progress", runs: true},
		{name: "a child at admitted", stands: phase.Admitted, status: "todo"},
		{name: "a child in planning taken out of the workflow", stands: phase.Planning, status: "backlog"},
		{name: "a child whose phase is done", stands: phase.Done, status: "in_progress"},
		{name: "an issue in planning of another tree", tree: "LEGION-300", stands: phase.Planning, status: "in_progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			pool := migratedPool(t)
			root := architectEntryRoot()
			root.Generation = 2
			seedIssue(t, pool, root)
			member := architectEntryRoot()
			member.Key, member.Phase, member.Status, member.Generation = "LEGION-209", tc.stands, tc.status, 1
			if tc.tree != "" {
				member.Tree = tc.tree
			}
			parent := member.Tree
			member.Parent = &parent
			seedIssue(t, pool, member)
			var start ArchitectStart
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				var err error
				start, err = ArchitectStartOf(ctx, tx, record.NewStore(), specGated, root)
				return err
			}); err != nil {
				t.Fatalf("read what the architect's first task is derived from: %v", err)
			}
			if start.MembersRun != tc.runs {
				t.Fatalf("the reader says a member of %s runs = %t beside %s at %s (%s), want %t",
					root.Key, start.MembersRun, member.Key, member.Phase, member.Status, tc.runs)
			}
			want := ArchitectFreshTreeLabel
			if tc.runs {
				want = ArchitectReadmittedLabel
			}
			if task := ArchitectStartTask(start); !strings.Contains(task, want) {
				t.Fatalf("the architect's start task is %q, want the shape labelled %q", task, want)
			}
		})
	}
}

// A root a hand-over carried into merging whose pull request has already merged stands in a phase
// the merge itself ended (PhaseFinished), so the promotion that opens its tree starts no merger
// (admit's startMidPhaseMembers), and its architect's task says so rather than that the daemon
// starts that phase's worker. The reader reads it from the record (ArchitectStartOf), over the
// same pull request the promotion reads. The control is the same root whose pull request is still
// open, whose merger the promotion does start.
func TestTheArchitectTaskOfACarriedRootInMergingReadsWhetherItsPullRequestMerged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		state    record.PullRequestState
		finished bool
		closing  string
	}{
		{"a pull request that has merged", record.PullRequestMerged, true, carriedMergedClosing},
		{"a pull request still open", record.PullRequestOpen, false, carriedClosing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			pool := migratedPool(t)
			root := architectEntryRoot()
			root.Phase, root.Generation, root.Status = phase.Merging, 2, "retro"
			seedIssue(t, pool, root)
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().PutHandedOverRoot(ctx, tx, record.HandedOverRoot{Issue: root.Key, Generation: root.Generation})
			})
			seedPR(t, pool, record.PullRequest{State: tc.state, Issue: root.Key, Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/" + root.Key, HeadSHA: "head", Failing: []string{}})
			var start ArchitectStart
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				var err error
				start, err = ArchitectStartOf(ctx, tx, record.NewStore(), specGated, root)
				return err
			}); err != nil {
				t.Fatalf("read what the architect's first task is derived from: %v", err)
			}
			if !start.Carried || start.PhaseFinished != tc.finished {
				t.Fatalf("the carried root's start = carried %t, phase finished %t; want carried, phase finished %t",
					start.Carried, start.PhaseFinished, tc.finished)
			}
			if task := ArchitectStartTask(start); !strings.HasSuffix(task, tc.closing) {
				t.Fatalf("the architect's start task = %q, want it to end %q", task, tc.closing)
			}
		})
	}
}

// What a carried root's task says of the phase it stands in and of its design gate, read from the
// record (ArchitectStartOf). The hand-over row names the issue, its generation and whether a gate
// came with it, and no phase, so it does not say whether the root still stands in the phase that
// came with it: the task names the phase it stands in now and the run the hand-over carried it
// into, which holds of a root still in its carried phase and of one that has moved on in that
// same run (dispatch://INFRA-2614 R3, R5). The gate clause is the row's own
// (record.HandedOverRoot.GateCarried): a child handed over from a tree whose root was reopened in
// its linger comes with none, and a task that said a gate came with it would be false of it.
func TestTheArchitectTaskOfACarriedRootNamesThePhaseItStandsInAndItsGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		stands      phase.Phase
		gateCarried bool
		says        string
	}{
		{
			name: "the phase that came with it", stands: phase.Implementing, gateCarried: true,
			says: "LEGION-208 is yours and stands in implementing, in the run a hand-over carried it into, " +
				"with the design gate that hand-over brought.",
		},
		{
			name: "a phase of that run it has moved on to", stands: phase.Testing, gateCarried: true,
			says: "LEGION-208 is yours and stands in testing, in the run a hand-over carried it into, " +
				"with the design gate that hand-over brought.",
		},
		{
			name: "a hand-over that brought no design gate", stands: phase.Testing,
			says: "LEGION-208 is yours and stands in testing, in the run a hand-over carried it into, " +
				"and that hand-over brought no design gate.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			pool := migratedPool(t)
			root := architectEntryRoot()
			root.Phase, root.Generation = tc.stands, 2
			seedIssue(t, pool, root)
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().PutHandedOverRoot(ctx, tx, record.HandedOverRoot{Issue: root.Key,
					Generation: root.Generation, GateCarried: tc.gateCarried})
			})
			var start ArchitectStart
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				var err error
				start, err = ArchitectStartOf(ctx, tx, record.NewStore(), specGated, root)
				return err
			}); err != nil {
				t.Fatalf("read what the architect's first task is derived from: %v", err)
			}
			if !start.Carried || start.GateCarried != tc.gateCarried {
				t.Fatalf("the carried root's start = carried %t, gate carried %t; want carried, gate carried %t",
					start.Carried, start.GateCarried, tc.gateCarried)
			}
			task := ArchitectStartTask(start)
			if !strings.HasPrefix(task, tc.says) {
				t.Fatalf("the architect's start task = %q, want it to begin %q", task, tc.says)
			}
			if strings.Contains(task, "the phase a hand-over carried it in at") {
				t.Fatalf("the architect's start task = %q; it names the phase it stands in as the phase the hand-over carried it "+
					"in at, which the record does not say", task)
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
	t.Parallel()
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
				Gate: record.TreeGate{Steps: specGated, Rows: registered}, Served: 1}
			if got := ArchitectStartTask(start); got != "" {
				t.Fatalf("the architect's start task = %q, want none: this root is no root the derivation serves", got)
			}
			start.Decided = decidedOnTheTree
			got := ArchitectStartTask(start)
			if got != decidedOnTheTree {
				t.Fatalf("the architect's start task = %q, want the decided text alone %q", got, decidedOnTheTree)
			}
			for _, closing := range closings {
				if strings.HasSuffix(got, closing) {
					t.Fatalf("the task %q carries the closing sentence %q of a shape, want none", got, closing)
				}
			}
		})
	}
}

// A person's decision on the tree is read before the work, so it leads whichever shape the root
// gets: the decided text, a blank line, then the shape's own text.
func TestTheArchitectTaskNamesAKeptDecisionFirstInEveryShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		start ArchitectStart
	}{
		{"a tree nobody has architected", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}}},
		{"a root a hand-over carried in", ArchitectStart{Root: rootAt(phase.Planning), Carried: true, Gate: record.TreeGate{Steps: specGated}}},
		{"a root that holds design gate rows", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: registeredSpec()}}},
		{"a root whose design gate is open", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: approvedSpec()}}},
		{"a tree that ran at an earlier generation", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Served: 1}},
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
	t.Parallel()
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
			name:  "a root whose design gate is open",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: approvedSpec()}},
			label: ArchitectGateOpenLabel,
		},
		{
			name:  "a tree that ran at an earlier generation",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Served: 1},
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
	t.Parallel()
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
	t.Parallel()
	start := ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}}
	if got := NarrowRootTask(start, 0, record.KeptDecisions{}); got != "" {
		t.Fatalf("the narrowed task = %q, want the empty task its caller retires", got)
	}
}
