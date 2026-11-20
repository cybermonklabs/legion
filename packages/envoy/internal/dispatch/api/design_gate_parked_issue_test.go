package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The approved requirement says "A gate is live for this sentence while the issue holding it is
// not `done` in Dispatch", and the gate clause "names every issue whose gate records that
// document". So a gate is named whatever status its issue holds, `triage`, `icebox` and `backlog`
// among them, until that issue is `done`. The project gates the spec step, the one these gates
// record, so the status is all that differs between the rows.
func TestAGateIsLiveWhileItsIssueIsNotDone(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "PARKED")
	publishGatedSteps(t, handler, "PARKED", []string{model.GatedStepSpec})
	spec := "# Design\n\nThe design, as approved.\n"
	for _, row := range []struct {
		status string
		live   bool
	}{
		{status: "in_progress", live: true}, // control: the clause names a live gate
		{status: "done", live: false},       // control: the one status the approved sentence leaves out
		{status: "triage", live: true},
		{status: "icebox", live: true},
		{status: "backlog", live: true},
		{status: "todo", live: true},
		{status: "testing", live: true},
		{status: "needs_review", live: true},
		{status: "retro", live: true},
	} {
		t.Run(row.status, func(t *testing.T) {
			document := createWorkingIssue(t, handler, "PARKED", "The document's own issue", &spec)
			holder := createWorkingIssue(t, handler, "PARKED", "The issue whose gate records it", &spec)
			publishDesignGates(t, handler, holder.Key, []issueDesignGate{
				{Step: model.GatedStepSpec, ArtifactID: document.PrimaryArtifactID},
			})
			if row.status != "in_progress" {
				setIssueStatus(t, handler, holder.Key, row.status)
			}
			approved := approveAdviceDocument(t, handler, document.PrimaryArtifactID)

			superseded := supersededByEdit(t, handler, document.PrimaryArtifactID, "A sentence the gate had not seen.", approved)
			t.Logf("gate of %s in %s: design_gates %#v", holder.Key, row.status, superseded.DesignGates)
			named := len(superseded.DesignGates) == 1 &&
				superseded.DesignGates[0] == supersededApprovalGate{Issue: holder.Key, Step: model.GatedStepSpec}
			if row.live && !named {
				t.Fatalf("design_gates = %#v, want %s's spec gate: its issue is in %s, not done, so its gate is live",
					superseded.DesignGates, holder.Key, row.status)
			}
			if !row.live && len(superseded.DesignGates) != 0 {
				t.Fatalf("design_gates = %#v, want none: a gate whose issue is done is not live", superseded.DesignGates)
			}
		})
	}
}
