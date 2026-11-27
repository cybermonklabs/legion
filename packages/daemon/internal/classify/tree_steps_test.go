package classify

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// specOnly is the list `gates.design: root-issues` resolves to.
var specOnly = []record.DesignStep{record.DesignStepSpec}

// treeRow is one registered gate row of LEGION-208 at version 2, approved at that version when
// open, else approved at none.
func treeRow(step record.DesignStep, open bool) record.DesignGate {
	row := record.DesignGate{Issue: "LEGION-208", Step: step, ArtifactID: "artifact-" + string(step), LatestVersion: 2}
	if open {
		row.ApprovedVersion = new(2)
	}
	return row
}

// treeRows is the rows by their step.
func treeRows(rows ...record.DesignGate) map[record.DesignStep]record.DesignGate {
	gates := make(map[record.DesignStep]record.DesignGate, len(rows))
	for _, row := range rows {
		gates[row.Step] = row
	}
	return gates
}

// A tree is gated on the steps the deployment gates and on criteria when its root registered the
// Verification document beside a deployment that gates spec and not criteria. No other row joins
// the list: a decisions row under spec alone, a criteria row under `off` and a criteria row under
// a deployment without spec all leave the deployment's own list, and a deployment that gates
// criteria already names it once.
func TestTreeStepsAddCriteriaOnlyWhereTheRootRegisteredItBesideSpecAlone(t *testing.T) {
	criteria := treeRows(treeRow(record.DesignStepCriteria, false))
	for _, tc := range []struct {
		name       string
		deployment []record.DesignStep
		rows       map[record.DesignStep]record.DesignGate
		want       []record.DesignStep
	}{
		{name: "spec alone, no criteria row", deployment: specOnly, rows: treeRows(treeRow(record.DesignStepSpec, true)),
			want: specOnly},
		{name: "spec alone, a criteria row", deployment: specOnly, rows: criteria,
			want: []record.DesignStep{record.DesignStepSpec, record.DesignStepCriteria}},
		{name: "spec alone, a decisions row", deployment: specOnly, rows: treeRows(treeRow(record.DesignStepDecisions, false)),
			want: specOnly},
		{name: "off, a criteria row kept", deployment: []record.DesignStep{}, rows: criteria, want: []record.DesignStep{}},
		{name: "decisions and spec, a criteria row",
			deployment: []record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec}, rows: criteria,
			want: []record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec, record.DesignStepCriteria}},
		{name: "criteria and spec, a criteria row",
			deployment: []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}, rows: criteria,
			want: []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}},
		{name: "decisions alone, a criteria row", deployment: []record.DesignStep{record.DesignStepDecisions}, rows: criteria,
			want: []record.DesignStep{record.DesignStepDecisions}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TreeSteps(tc.deployment, tc.rows); !slices.Equal(got, tc.want) {
				t.Fatalf("TreeSteps(%v, rows of %v) = %v, want %v", tc.deployment, stepsOf(tc.rows), got, tc.want)
			}
		})
	}

	// The deployment's list is the configuration every tree reads, so a tree that adds criteria
	// never writes it into the room the list's own array has left.
	backing := []record.DesignStep{record.DesignStepSpec, record.DesignStepDecisions}
	if got := TreeSteps(backing[:1], criteria); !slices.Equal(got, []record.DesignStep{record.DesignStepSpec, record.DesignStepCriteria}) ||
		backing[1] != record.DesignStepDecisions {
		t.Fatalf("TreeSteps over a list with room = %v, leaving the list's array %v; want the criteria step added and the array as it was", got, backing)
	}
}

// Under spec alone a registered criteria row holds the tree as the spec row does, in each of the
// three rules that read a tree's rows: the tree's work runs only once both are approved, the step
// that holds it is the first closed of the two with the spec first, and the step a person is
// asked about is the same. A root without a criteria row is gated on the spec alone, and under
// `off` a criteria row kept from before holds nothing.
func TestTheGateRulesHoldARegisteredCriteriaRowUnderSpecAlone(t *testing.T) {
	spec, specClosed := treeRow(record.DesignStepSpec, true), treeRow(record.DesignStepSpec, false)
	criteria, criteriaClosed := treeRow(record.DesignStepCriteria, true), treeRow(record.DesignStepCriteria, false)
	for _, tc := range []struct {
		name       string
		deployment []record.DesignStep
		rows       map[record.DesignStep]record.DesignGate
		open       bool
		// closed is the step FirstClosedStep names, "" when every step is open.
		closed record.DesignStep
		// standing is the step StandingGate names, "" when it stands on none.
		standing record.DesignStep
	}{
		{name: "the spec approved, the Verification document not", deployment: specOnly, rows: treeRows(spec, criteriaClosed),
			closed: record.DesignStepCriteria, standing: record.DesignStepCriteria},
		{name: "neither approved", deployment: specOnly, rows: treeRows(specClosed, criteriaClosed),
			closed: record.DesignStepSpec, standing: record.DesignStepSpec},
		{name: "the Verification document approved, the spec not", deployment: specOnly, rows: treeRows(specClosed, criteria),
			closed: record.DesignStepSpec, standing: record.DesignStepSpec},
		{name: "both approved", deployment: specOnly, rows: treeRows(spec, criteria), open: true},
		{name: "the spec approved and no criteria row", deployment: specOnly, rows: treeRows(spec), open: true},
		{name: "the spec closed and no criteria row", deployment: specOnly, rows: treeRows(specClosed),
			closed: record.DesignStepSpec, standing: record.DesignStepSpec},
		{name: "off, the spec open and a closed criteria row kept", deployment: []record.DesignStep{},
			rows: treeRows(spec, criteriaClosed), open: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DesignGatesOpen(tc.deployment, tc.rows); got != tc.open {
				t.Fatalf("DesignGatesOpen = %v, want %v", got, tc.open)
			}
			step, row, closed := FirstClosedStep(tc.deployment, tc.rows)
			if closed != (tc.closed != "") || step != tc.closed || closed && row != tc.rows[tc.closed] {
				t.Fatalf("FirstClosedStep = %q %#v (closed %t), want %q", step, row, closed, tc.closed)
			}
			gate, standing := StandingGate(tc.deployment, tc.rows)
			if standing != (tc.standing != "") || standing && gate != tc.rows[tc.standing] {
				t.Fatalf("StandingGate = %#v (standing %t), want the %q row", gate, standing, tc.standing)
			}
		})
	}
}

// stepsOf is the steps rows holds, in the order a gate runs them.
func stepsOf(rows map[record.DesignStep]record.DesignGate) []record.DesignStep {
	var steps []record.DesignStep
	for _, step := range record.DesignSteps {
		if _, ok := rows[step]; ok {
			steps = append(steps, step)
		}
	}
	return steps
}
