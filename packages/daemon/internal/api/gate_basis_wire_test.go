package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A step on an ancestor's basis answers words of its own (dispatch://INFRA-3320, R2): a step a
// basis opened reads `open_on_basis`, never `approved`, with the root's version registered as its
// `approvedVersion` and the ancestor's approval beside it, and a step that waits on the ancestor's
// open request reads `pending_on_basis`, never `waiting`, with the person's approval of the root's
// own earlier version, where it holds one, and the request beside it. Once the wait ends, or a new
// version of the root's document closes the basis, the step reads as registered on the root's own
// document with no basis, and its `approvedVersion` is the person's approval it kept, never the
// version the basis opened.
func TestGatedStepsAnswerAStepOnAnAncestorsBasisInItsOwnWords(t *testing.T) {
	approvedAt := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	approved := record.GateBasis{Issue: "LEGION-100", ArtifactID: "f3b8d1a6-5c27-4e90-8a14-6e9c2d7b0f58", Version: 3,
		Approval: record.EventMark{Seq: 40, At: approvedAt}, Approver: "user:alice", RegisteredAt: approvedAt.Add(time.Hour)}
	// LEGION-208's spec at version 4, which a person approved at version 2 before the basis.
	personal, registered := 2, 4
	spec := gateRow(record.DesignStepSpec, "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", registered)
	spec.ApprovedVersion, spec.ApprovedMark = &personal, record.EventMark{Seq: 12, At: approvedAt.Add(-time.Hour)}
	open := record.OpenOnBasis(spec, approved)
	pending := record.PendOnBasis(spec, approved, "c81e5d3a-7f42-4b96-a0d8-3e6f9b1c2a75")
	// The basis that stood is closed by version 5 of the root's own document.
	closed := record.CloseBasis(open)
	closed.LatestVersion = 5

	for _, tc := range []struct {
		name string
		gate record.DesignGate
		want GatedStepView
	}{
		{"a step a basis opened", open, GatedStepView{
			Step: record.DesignStepSpec, ArtifactID: spec.ArtifactID, CurrentVersion: 4, ApprovedVersion: &registered,
			Status: GatedStepOpenOnBasis,
			Basis: &GatedStepBasis{Issue: "LEGION-100", ArtifactID: approved.ArtifactID, Version: 3, Approver: "user:alice",
				ApprovedAt: &approvedAt},
		}},
		{"a step that waits on the ancestor's request", pending, GatedStepView{
			Step: record.DesignStepSpec, ArtifactID: spec.ArtifactID, CurrentVersion: 4, ApprovedVersion: &personal,
			Status: GatedStepPendingOnBasis,
			Basis:  &GatedStepBasis{Issue: "LEGION-100", ArtifactID: approved.ArtifactID, Version: 3, AskID: "c81e5d3a-7f42-4b96-a0d8-3e6f9b1c2a75"},
		}},
		{"a step whose wait ended", record.EndPendingBasis(pending), GatedStepView{
			Step: record.DesignStepSpec, ArtifactID: spec.ArtifactID, CurrentVersion: 4, ApprovedVersion: &personal,
			Status: GatedStepWaiting,
		}},
		{"a step whose basis a new version closed", closed, GatedStepView{
			Step: record.DesignStepSpec, ArtifactID: spec.ArtifactID, CurrentVersion: 5, ApprovedVersion: &personal,
			Status: GatedStepWaiting,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GatedStepViewsOf(config.DesignGateRootIssues.Steps(), map[record.DesignStep]record.DesignGate{record.DesignStepSpec: tc.gate})
			if want := []GatedStepView{tc.want}; !reflect.DeepEqual(got, want) {
				t.Fatalf("gatedSteps = %+v (basis %+v), want %+v (basis %+v)", got, got[0].Basis, want, want[0].Basis)
			}
		})
	}
}

// gateBasisContract is the contract whose entry in version.go added a step's basis, its two
// status words and each root's `treeRuns`, and this daemon's own contract is never below it: a
// caller at this contract is served them.
func TestTheGateBasisContractIsTheContractThatAddedTheBasis(t *testing.T) {
	const addedGateBasis = 126
	if gateBasisContract != addedGateBasis {
		t.Fatalf("gateBasisContract = %d, want %d, the contract whose entry in version.go added the gated steps' basis and the roots' treeRuns",
			gateBasisContract, addedGateBasis)
	}
	if gateBasisContract > DaemonAPIVersion {
		t.Fatalf("gateBasisContract = %d, above DaemonAPIVersion %d: no caller could be served the basis",
			gateBasisContract, DaemonAPIVersion)
	}
}

// A plugin below gateBasisContract reads `gatedSteps[].status` with an enum of the four words
// before it, and a strict one refuses the whole body for a key it does not name. So a caller
// whose pod records a contract below it is served no `treeRuns` on any issue, and no `gatedSteps`
// on an issue any step of which reads `open_on_basis` or `pending_on_basis` — neither `waiting`
// nor `approved` is true of such a step — while every other issue keeps its gated steps and every
// key beside them, its `designGate` among them. A caller at the contract is served both: every
// root's `treeRuns`, and the two steps with their basis.
func TestACallerBelowTheGateBasisContractIsServedNoTreeRunsAndNoStepOnABasis(t *testing.T) {
	state := populatedState(t)
	onBasis := map[string]GatedStepStatus{"LEGION-207": GatedStepOpenOnBasis, "LEGION-214": GatedStepPendingOnBasis}

	behind := issueViews(t, stateFrom(t, state, statePodAddr+":41000", podClaims(gateBasisContract-1)))
	for key, view := range behind {
		if runs, carried := view["treeRuns"]; carried {
			t.Errorf("%s is served treeRuns = %v to a pod below %d; its plugin's reader does not name the key", key, runs, gateBasisContract)
		}
		steps, carried := view["gatedSteps"]
		if _, basis := onBasis[key]; basis && carried {
			t.Errorf("%s is served gatedSteps %v to a pod below %d; no word that reader knows is true of a step on a basis", key, steps, gateBasisContract)
		} else if want := state.Issues[key].GatedSteps; !basis && (len(want) > 0) != carried {
			t.Errorf("%s is served gatedSteps = %v to a pod below %d, want them as the record says: %+v", key, carried, gateBasisContract, want)
		}
	}
	for _, key := range []string{"pullRequest", "check"} {
		if value := behind["LEGION-207"][key]; value == nil {
			t.Errorf("LEGION-207 is served no %s to a pod below %d, want every key of it beside the basis", key, gateBasisContract)
		}
	}

	at := issueViews(t, stateFrom(t, state, statePodAddr+":41000", podClaims(gateBasisContract)))
	for key, issue := range state.Issues {
		view := at[key]
		runs, carried := view["treeRuns"]
		switch {
		case issue.TreeRuns == nil && carried:
			t.Errorf("%s, no root, is served treeRuns = %v", key, runs)
		case issue.TreeRuns != nil && runs != *issue.TreeRuns:
			t.Errorf("%s is served treeRuns = %v at %d, want %v", key, runs, gateBasisContract, *issue.TreeRuns)
		}
		status, basis := onBasis[key]
		if !basis {
			continue
		}
		steps, _ := view["gatedSteps"].([]any)
		if len(steps) != 1 {
			t.Fatalf("%s is served gatedSteps %v at %d, want its one step", key, view["gatedSteps"], gateBasisContract)
		}
		step := steps[0].(map[string]any)
		if step["status"] != string(status) || step["basis"] == nil {
			t.Errorf("%s is served the step %v at %d, want %s with its basis", key, step, gateBasisContract, status)
		}
	}
}
