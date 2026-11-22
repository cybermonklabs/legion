package api

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// holdState is a state whose merger of LEGION-215 carries the design gate's hold on it, beside the
// issue's implementer, which the gate holds nothing of: the projection writes `hold` on the claim
// whose hold stands and on no other (internal/projection's Project, handed the holds that stand by
// workflow.HoldsNow).
func holdState() State {
	return State{
		Admission: Admission{Cap: 2, Active: []string{"LEGION-208"}, Trees: []string{"LEGION-208"}},
		Issues: map[string]Issue{
			"LEGION-215": {Key: "LEGION-215", Tree: "LEGION-208", Phase: phase.Merging, Status: "retro",
				Workers: map[claim.Role]PhaseView{
					claim.RoleMerger: {Claim: ClaimView{Session: "ses_merger_215", State: "suspended",
						Hold: &HoldView{Step: record.DesignStepSpec, ArtifactID: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", Version: 7}}},
					claim.RoleImplementer: {Claim: ClaimView{Session: "ses_implementer_215", State: "idle"}},
				}},
		},
	}
}

// `GET /legion/v1/state` — the body `read_record` reads an issue's record from — shows each hold
// that stands on the merger it holds: the gated step, the document registered for that step and
// the version of it that is with its reviewer. A plugin below waitContract, the contract that
// added the key beside the wait, is served no claim view carrying `hold`: one built before
// sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body for a
// key it does not name, and one built after strips the key. The callers are the wait's
// (waitBandCallers): each one that the band places below waitContract is served the hold on no
// claim, and each other one is served it on the merger alone, in the same reading of the same
// state.
func TestTheStateServesAHoldOnlyToACallerAtTheHoldContract(t *testing.T) {
	want := `{"step":"spec","artifactId":"d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093","version":7}`
	for _, tc := range waitBandCallers() {
		t.Run(tc.name, func(t *testing.T) {
			served := claimKeyServed(t, stateStating(t, stateServer(t, holdState(), tc.claims), tc.remote, tc.stated), "hold")
			if !tc.served {
				if len(served) != 0 {
					t.Errorf("the caller is served the holds %v, want none: the key is not its contract's", served)
				}
				return
			}
			if len(served) != 1 {
				t.Fatalf("the state carries the holds %v, want the merger's alone: the gate holds no implementer", served)
			}
			if got := string(served["LEGION-215 merger"]); got != want {
				t.Errorf("LEGION-215's merger is served the hold %s, want %s", got, want)
			}
		})
	}
}

// A row of the design gate that was built with no step is the spec's row, so the hold names the
// spec step, as the note of the merger's task does (workflow.MergerGateNote); a row of another
// step names that step. The version is the row's latest, the one with its reviewer, and never the
// version a person approved.
func TestHoldViewOfNamesTheStepTheDocumentAndTheVersionWithTheReviewer(t *testing.T) {
	approved := 6
	for _, tc := range []struct {
		name string
		gate record.DesignGate
		want HoldView
	}{
		{
			name: "a row of the spec step",
			gate: record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "spec-208", LatestVersion: 7, ApprovedVersion: &approved},
			want: HoldView{Step: record.DesignStepSpec, ArtifactID: "spec-208", Version: 7},
		},
		{
			name: "a row built with no step",
			gate: record.DesignGate{Issue: "LEGION-208", ArtifactID: "spec-208", LatestVersion: 2},
			want: HoldView{Step: record.DesignStepSpec, ArtifactID: "spec-208", Version: 2},
		},
		{
			name: "a row of the criteria step",
			gate: record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepCriteria, ArtifactID: "criteria-208", LatestVersion: 3},
			want: HoldView{Step: record.DesignStepCriteria, ArtifactID: "criteria-208", Version: 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HoldViewOf(tc.gate); got == nil || *got != tc.want {
				t.Fatalf("HoldViewOf(%+v) = %+v, want %+v", tc.gate, got, tc.want)
			}
		})
	}
}
