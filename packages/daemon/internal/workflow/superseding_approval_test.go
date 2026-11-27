package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A person's approval stands while the gate's row holds the approval's document and its approved
// version is the approval's (SupersedingApproval's own rule for an approval): through a new
// version that closes the gate, which keeps that approval as the one below the latest, and no
// longer once the person approves a later version, which replaces it, or requests changes, which
// clears it. An approval of a version the gate never approved names no approval at all, and
// neither does one of a document the step no longer records. Where it no longer stands the answer
// names the approval the gate holds in its place, and none where the gate holds none.
func TestAnApprovalStandsWhileTheGateStillRecordsItsVersionApproved(t *testing.T) {
	t.Parallel()
	gate := record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact", LatestVersion: 4}
	approved := classify.ApplyDesignGateEvent(gate, classify.DesignGateApproved, 4, record.EventMark{Seq: 10})
	closed := classify.ApplyDesignGateEvent(approved, classify.DesignGateVersion, 5, record.EventMark{})
	approvedLater := classify.ApplyDesignGateEvent(closed, classify.DesignGateApproved, 5, record.EventMark{Seq: 12})
	for _, tc := range []struct {
		name     string
		gate     record.DesignGate
		artifact string
		to       int
		stands   bool
		// by is the version the answer names in the approval's place, 0 for none.
		by int
	}{
		{name: "the approval of the version the gate holds approved", gate: approved, to: 4, stands: true},
		{name: "a new version closed the gate since", gate: closed, to: 4, stands: true},
		{name: "the person approved the new version", gate: approvedLater, to: 4, by: 5},
		{name: "that later approval", gate: approvedLater, to: 5, stands: true},
		{name: "the person requested changes on the new version", gate: classify.ApplyDesignGateEvent(closed, classify.DesignGateChangesRequested, 5,
			record.EventMark{Seq: 12}), to: 4},
		{name: "an approval of a version the gate never approved", gate: approved, to: 5, by: 4},
		{name: "a gate that records no approval", gate: gate, to: 4},
		{name: "an approval of a document the step no longer records", gate: approved, artifact: "another artifact", to: 4, by: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := tc.artifact
			if artifact == "" {
				artifact = gate.ArtifactID
			}
			got := SupersedingApproval(&tc.gate, true, Decision{Kind: record.DecisionDesignApproved, Step: record.DesignStepSpec,
				Artifact: artifact, Version: tc.to})
			by := 0
			if got != nil && got.By != nil {
				by = got.By.Version
			}
			if (got == nil) != tc.stands || by != tc.by {
				t.Fatalf("SupersedingApproval(%#v, an approval of %q at %d) = %+v (by %d), want stands %t, by %d", tc.gate, artifact, tc.to,
					got, by, tc.stands, tc.by)
			}
		})
	}
}

// SupersedingApproval asks the gate it is handed only when that gate is of the step the review is of
// (Decision.Step). A person's changes request on the decisions document, answered by an approval
// of a later version, is superseded by that approval where the gate is the decisions step's. The
// same approval on a gate of another step is no word of the person's on the document reviewed:
// the review stands, as it does where the issue holds no gate of its step at all. A step and a gate
// naming none are the spec step's, as the record reads them (record.DesignStep.OrSpec).
func TestADecisionIsAskedOnlyOfTheGateOfItsOwnStep(t *testing.T) {
	t.Parallel()
	approved := 3
	gateAt := func(step record.DesignStep) *record.DesignGate {
		return &record.DesignGate{Issue: "LEGION-208", Step: step, ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: &approved,
			ApprovedMark: record.EventMark{Seq: 9}}
	}
	review := func(step record.DesignStep) Decision {
		return Decision{Kind: record.DecisionDesignChangesRequested, Step: step, Version: 2, Mark: record.EventMark{Seq: 5}}
	}
	for _, tc := range []struct {
		name       string
		gate       *record.DesignGate
		d          Decision
		superseded *Approval
	}{
		{name: "the gate of the step reviewed", gate: gateAt(record.DesignStepDecisions), d: review(record.DesignStepDecisions),
			superseded: &Approval{Version: 3}},
		{name: "a gate of another step", gate: gateAt(record.DesignStepSpec), d: review(record.DesignStepDecisions)},
		{name: "a gate naming no step, of a review of the spec step", gate: gateAt(""), d: review(record.DesignStepSpec),
			superseded: &Approval{Version: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SupersedingApproval(tc.gate, true, tc.d)
			if (got == nil) != (tc.superseded == nil) || got != nil && (got.By == nil || *got.By != *tc.superseded) {
				t.Fatalf("SupersedingApproval(gate of step %q, review of step %q) = %+v, want by %+v", tc.gate.Step, tc.d.Step, got, tc.superseded)
			}
		})
	}
}

// A person's changes request on a root's Verification document is asked of its criteria gate
// wherever a person approves that step: a deployment that gates criteria, and one that gates spec
// and not criteria, where the root registered the document as criteria and one approval opens both
// (record.TakesStep). The approval of a later version answers the request in both, so neither a
// kept decision nor a queued notice tells the architect a request the person has since approved.
// Where no person approves criteria (a deployment that gates neither spec nor criteria), the gate's
// approval is its registration's, and the request stands.
func TestAChangesRequestOnTheVerificationDocumentIsAskedWhereAPersonApprovesIt(t *testing.T) {
	t.Parallel()
	approved := 3
	gates := map[record.DesignStep]record.DesignGate{record.DesignStepCriteria: {Issue: "LEGION-208",
		Step: record.DesignStepCriteria, ArtifactID: "verification-208", LatestVersion: 3, ApprovedVersion: &approved,
		ApprovedMark: record.EventMark{Seq: 9}}}
	review := Decision{Kind: record.DecisionDesignChangesRequested, Version: 2, Mark: record.EventMark{Seq: 5}}
	ref := stepRef("LEGION-208", record.DesignStepCriteria)
	for _, tc := range []struct {
		name       string
		steps      []record.DesignStep
		superseded bool
	}{
		{name: "a deployment that gates criteria", steps: []record.DesignStep{record.DesignStepSpec, record.DesignStepCriteria},
			superseded: true},
		{name: "a deployment that gates spec and not criteria", steps: []record.DesignStep{record.DesignStepSpec}, superseded: true},
		{name: "a deployment that gates neither", steps: []record.DesignStep{record.DesignStepDecisions}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RefSuperseded(gates, tc.steps, "LEGION-208", ref, review)
			if (got != nil) != tc.superseded {
				t.Fatalf("RefSuperseded(criteria gate approved at 3, steps %v, a changes request of version 2) = %+v, want superseded %t",
					tc.steps, got, tc.superseded)
			}
		})
	}
}
