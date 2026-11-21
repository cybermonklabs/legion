package workflow

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

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

// decidedOnTheTree is what a person decided on the tree while nobody in it could hear them, as the
// kept-decisions task names it.
var decidedOnTheTree = KeptDecisionsTask("LEGION-208", record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: "dispatch://LEGION-208/spec",
	At: time.Unix(0, 0).UTC(), Issue: "LEGION-208", Decision: record.DecisionDesignApproved, Actor: "user:alice"}}})

// childDecision is a decision kept on LEGION-209, a child of the tree, which a narrowing keeps.
func childDecision() record.KeptDecisions {
	return record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: "dispatch://LEGION-209/ask/ask-1",
		At: time.Unix(0, 0).UTC(), Issue: "LEGION-209", Decision: record.DecisionAskAnswered, Actor: "user:alice"}}}
}

// startHead is the head of every start task of LEGION-208.
var startHead = texts.TaskHead{Kind: texts.ArchitectStart, Issue: "LEGION-208"}

// The facts of each shape, whole: this task is the one thing a fresh architect's first turn has to
// go on, so what it states is the whole of what that turn knows. Each shape names the steps the
// deployment gates and the first act, so one gated step, two and none are all held. Each names what
// starts the tree's next work, and each holds of the root it is sent to: no root but a fresh tree's
// is told that nothing else starts it, since a carried root's phase worker, an open gate's first
// phase, a person's review of a registered step and the re-admitted members of a tree that ran each
// start something or bring a message.
func TestTheArchitectTaskOfEachShape(t *testing.T) {
	two := []record.DesignStep{record.DesignStepDecisions, record.DesignStepCriteria}
	yes, no := true, false
	freshActs := []string{FirstActClaim, FirstActWriteSpec, FirstActDecompose, FirstActRequestApproval, FirstActRegisterGate}
	carriedActs := []string{FirstActClaim, FirstActResumePhase}
	carried := func(p phase.Phase, gate bool, merged bool, starts *bool) texts.ArchitectStartRecord {
		gateCarried := gate
		return texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeCarriedPhase, Generation: 2, Phase: p, GateCarried: &gateCarried,
			Merged: merged, DaemonStartsWorker: starts, FirstAct: carriedActs}
	}
	for _, tc := range []struct {
		name  string
		start ArchitectStart
		want  texts.ArchitectStartRecord
	}{
		{
			name:  "a tree nobody has architected, under a deployment that gates the spec",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeFreshTree, Generation: 2, HoldsSlot: true, Phase: phase.Admitted,
				Gated: []string{"spec"}, FirstAct: freshActs, Decompose: true, NothingElseStarts: true},
		},
		{
			name:  "a tree nobody has architected, under a deployment that gates two steps",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: two}},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeFreshTree, Generation: 2, HoldsSlot: true, Phase: phase.Admitted,
				Gated: []string{"decisions", "criteria"}, FirstAct: freshActs, Decompose: true, NothingElseStarts: true},
		},
		{
			name:  "a tree nobody has architected, under a deployment that gates no step",
			start: ArchitectStart{Root: admittedRoot()},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeFreshTree, Generation: 2, HoldsSlot: true, Phase: phase.Admitted,
				FirstAct: []string{FirstActClaim, FirstActWriteSpec, FirstActDecompose, FirstActRegisterGate}, Decompose: true, NothingElseStarts: true},
		},
		{
			name:  "a tree nobody has architected at generation 1, a member of which runs",
			start: ArchitectStart{Root: firstGenerationRoot(), Gate: record.TreeGate{Steps: specGated}, MembersRun: true},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeFreshTree, Generation: 1, HoldsSlot: true, Phase: phase.Admitted,
				MembersRun: true, Gated: []string{"spec"}, FirstAct: freshActs, Decompose: true, FirstPhaseWaitsFor: []string{WaitsForGate}},
		},
		{
			name:  "a root a hand-over carried in",
			start: ArchitectStart{Root: rootAt(phase.Implementing), Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want:  carried(phase.Implementing, true, false, &yes),
		},
		{
			name:  "a root a hand-over carried in that brought no design gate",
			start: ArchitectStart{Root: rootAt(phase.Implementing), Carried: true, Gate: record.TreeGate{Steps: specGated}},
			want:  carried(phase.Implementing, false, false, &yes),
		},
		{
			name:  "a root a hand-over carried in that has moved on to a phase of that run",
			start: ArchitectStart{Root: rootAt(phase.Testing), Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want:  carried(phase.Testing, true, false, &yes),
		},
		{
			name:  "a root a hand-over carried into accepting, the architect's own phase",
			start: ArchitectStart{Root: rootAt(phase.Accepting), Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want:  carried(phase.Accepting, true, false, &no),
		},
		{
			name:  "a root a hand-over carried in held",
			start: ArchitectStart{Root: rootAt(phase.Held), Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want:  carried(phase.Held, true, false, &no),
		},
		{
			name: "a root a hand-over carried into merging whose pull request has merged",
			start: ArchitectStart{Root: rootAt(phase.Merging), Carried: true, GateCarried: true, PhaseFinished: true,
				Gate: record.TreeGate{Steps: specGated}},
			want: carried(phase.Merging, true, true, &no),
		},
		{
			name:  "a root a hand-over carried into merging whose pull request has not merged",
			start: ArchitectStart{Root: rootAt(phase.Merging), Carried: true, GateCarried: true, Gate: record.TreeGate{Steps: specGated}},
			want:  carried(phase.Merging, true, false, &yes),
		},
		{
			name: "a root whose one gated step a person has approved, so its gate is open",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 2, ApprovedVersion: new(2)}}}},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeGateOpen, Generation: 2, Phase: phase.Admitted,
				Gates: []texts.ArchitectGate{{Step: "spec", Document: "dispatch://LEGION-208/spec", Version: 2, State: texts.GateApproved}},
				Gated: []string{"spec"}, FirstAct: []string{FirstActClaim, FirstActTakeUpTree},
				FirstPhaseWaitsFor: []string{WaitsForSlot, WaitsForChildren, WaitsForBlockers}},
		},
		{
			name: "a root whose two gated steps stand at a changes request and at nothing",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: two, Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepDecisions: {Issue: "LEGION-208", Step: record.DesignStepDecisions, LatestVersion: 3,
					ToldChangesVersion: new(3), ToldChangesReason: "name the alternative"}}}},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeGateStanding, Generation: 2, Phase: phase.Admitted,
				Gates: []texts.ArchitectGate{
					{Step: "decisions", Document: stepRef("LEGION-208", record.DesignStepDecisions), Version: 3, State: texts.GateChangesRequested},
					{Step: "criteria", Document: stepRef("LEGION-208", record.DesignStepCriteria), State: texts.GateUnregistered}},
				Gated: []string{"decisions", "criteria"}, FirstAct: []string{FirstActClaim, FirstActCarryGate}, FirstPhaseWaitsFor: []string{WaitsForGate}},
		},
		{
			name: "a root whose registered step no person has answered",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 4, ApprovedVersion: new(2)}}}},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeGateStanding, Generation: 2, Phase: phase.Admitted,
				Gates: []texts.ArchitectGate{{Step: "spec", Document: "dispatch://LEGION-208/spec", Version: 4, State: texts.GateUnanswered}},
				Gated: []string{"spec"}, FirstAct: []string{FirstActClaim, FirstActCarryGate}, FirstPhaseWaitsFor: []string{WaitsForGate}},
		},
		{
			name: "a root holding a row under a deployment that gates no step, so its gate is open",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepSpec: {Issue: "LEGION-208", Step: record.DesignStepSpec, LatestVersion: 1, ApprovedVersion: new(1)}}}},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeGateOpen, Generation: 2, Phase: phase.Admitted,
				Gates:    []texts.ArchitectGate{{Step: "spec", Document: "dispatch://LEGION-208/spec", Version: 1, State: texts.GateRegistered}},
				FirstAct: []string{FirstActClaim, FirstActTakeUpTree}, FirstPhaseWaitsFor: []string{WaitsForSlot, WaitsForChildren, WaitsForBlockers}},
		},
		{
			name:  "a tree that ran at an earlier generation, under a deployment that gates the spec",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Served: 1},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeEarlierGeneration, Generation: 2, HoldsSlot: true, Phase: phase.Admitted,
				Gated: []string{"spec"}, FirstAct: []string{FirstActClaim, FirstActRequestApproval, FirstActRegisterGate}, FirstPhaseWaitsFor: []string{WaitsForGate}},
		},
		{
			name:  "a tree that ran at an earlier generation, under a deployment that gates no step",
			start: ArchitectStart{Root: admittedRoot(), Served: 1},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeEarlierGeneration, Generation: 2, HoldsSlot: true, Phase: phase.Admitted,
				FirstAct: []string{FirstActClaim, FirstActRegisterGate}, FirstPhaseWaitsFor: []string{WaitsForGate}},
		},
		{
			name:  "a tree re-admitted while a member of it runs, whose architect claim served nothing",
			start: ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, MembersRun: true},
			want: texts.ArchitectStartRecord{TaskHead: startHead, Shape: texts.ShapeEarlierGeneration, Generation: 2, HoldsSlot: true, Phase: phase.Admitted,
				MembersRun: true, Gated: []string{"spec"}, FirstAct: []string{FirstActClaim, FirstActRequestApproval, FirstActRegisterGate},
				FirstPhaseWaitsFor: []string{WaitsForGate}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, served := ArchitectStartFacts(tc.start)
			if !served || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the architect's start task (served %t) =\n%+v\nwant\n%+v", served, got, tc.want)
			}
			if read := readTaskOf(t, ArchitectStartTask(tc.start)); read.fields["shape"] != tc.want.Shape {
				t.Fatalf("the rendered task names %v, want the shape %s", read.fields, tc.want.Shape)
			}
		})
	}
}

// Every label is a label of one shape: a task carries its own shape's label and no other shape's.
// The decomposition is asked for by the one shape that opens a tree.
func TestTheArchitectTaskLabelsOfEachShape(t *testing.T) {
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
		decomposes bool
	}{
		{"a tree nobody has architected", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}}, ArchitectFreshTreeLabel, true},
		{"a root a hand-over carried in", ArchitectStart{Root: rootAt(phase.Accepting), Carried: true, Gate: record.TreeGate{Steps: specGated}},
			ArchitectCarriedPhaseLabel, false},
		{"a root that holds design gate rows", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: registeredSpec()}},
			ArchitectGateRowsLabel, false},
		{"a root whose design gate is open", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated, Rows: approvedSpec()}},
			ArchitectGateOpenLabel, false},
		{"a tree that ran at an earlier generation", ArchitectStart{Root: admittedRoot(), Gate: record.TreeGate{Steps: specGated}, Served: 1},
			ArchitectReadmittedLabel, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := ArchitectStartTask(tc.start)
			if !hasLines(task, tc.label) {
				t.Fatalf("the task %q does not carry its own label %q", task, tc.label)
			}
			for _, other := range labels {
				if other != tc.label && strings.Contains(task, other) {
					t.Fatalf("the task %q carries another shape's label %q", task, other)
				}
			}
			if asks := hasLines(task, ArchitectDecompositionLabel); asks != tc.decomposes {
				t.Fatalf("the task %q asks for a decomposition = %v, want %v", task, asks, tc.decomposes)
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
			never: []string{ArchitectFreshTreeLabel, ArchitectDecompositionLabel, "nothing_else_starts_it: true"},
		},
		{
			name:  "a root at generation 1 while a member of its tree runs, whose architect claim served nothing",
			start: ArchitectStart{Root: firstGenerationRoot(), Gate: record.TreeGate{Steps: specGated}, MembersRun: true},
			label: ArchitectFreshTreeLabel,
			never: []string{"nothing_else_starts_it: true"},
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
	for _, tc := range []struct {
		name     string
		state    record.PullRequestState
		finished bool
		lines    []string
	}{
		{"a pull request that has merged", record.PullRequestMerged, true, []string{"merged: true", "daemon_starts_worker: false"}},
		{"a pull request still open", record.PullRequestOpen, false, []string{"daemon_starts_worker: true"}},
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
			if task := ArchitectStartTask(start); !hasLines(task, tc.lines...) {
				t.Fatalf("the architect's start task = %q, want it to name %q", task, tc.lines)
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
	for _, tc := range []struct {
		name        string
		stands      phase.Phase
		gateCarried bool
		lines       []string
	}{
		{name: "the phase that came with it", stands: phase.Implementing, gateCarried: true,
			lines: []string{ArchitectCarriedPhaseLabel, "phase: implementing", "gate_carried: true"}},
		{name: "a phase of that run it has moved on to", stands: phase.Testing, gateCarried: true,
			lines: []string{ArchitectCarriedPhaseLabel, "phase: testing", "gate_carried: true"}},
		{name: "a hand-over that brought no design gate", stands: phase.Testing,
			lines: []string{ArchitectCarriedPhaseLabel, "phase: testing", "gate_carried: false"}},
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
			if task := ArchitectStartTask(start); !hasLines(task, tc.lines...) {
				t.Fatalf("the architect's start task = %q, want it to name %q", task, tc.lines)
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
				Gate: record.TreeGate{Steps: specGated, Rows: registered}, Served: 1}
			if got := ArchitectStartTask(start); got != "" {
				t.Fatalf("the architect's start task = %q, want none: this root is no root the derivation serves", got)
			}
			start.Decided = decidedOnTheTree
			got := ArchitectStartTask(start)
			if got != decidedOnTheTree {
				t.Fatalf("the architect's start task = %q, want the decided text alone %q", got, decidedOnTheTree)
			}
			if strings.Contains(got, string(texts.ArchitectStart)) {
				t.Fatalf("the task %q carries an architect-start of a shape, want none", got)
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
