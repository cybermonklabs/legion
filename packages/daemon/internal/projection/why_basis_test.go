package projection

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The design gate's sentence of `why` says that the root waits for a person's approval of each
// gated step's document, which is untrue of a step on an ancestor's approval (dispatch://INFRA-3320,
// R1 and R2). Where the root of a shut tree holds such a step, each gated step is named with its
// own state: a step open on an ancestor's approval names that approval and no person's approval of
// the root's document, and a step that waits on the ancestor's approval request names that
// request. A tree whose root holds neither reads the table's sentence, as before.
func TestTheDesignGateWhyGivesAStepOnAnAncestorsApprovalItsOwnWords(t *testing.T) {
	basis := record.GateBasis{Issue: "LEGION-90", ArtifactID: "artifact-90", Version: 5, Approval: record.EventMark{Seq: 40, At: projectedAt},
		Approver: "user:alice", RegisteredAt: projectedAt}
	pending := record.PendOnBasis(record.DesignGate{Issue: "LEGION-100", Step: record.DesignStepSpec, ArtifactID: "artifact-LEGION-100", LatestVersion: 2},
		basis, "ask-90")
	open := record.OpenOnBasis(record.DesignGate{Issue: "LEGION-100", Step: record.DesignStepSpec, ArtifactID: "artifact-LEGION-100", LatestVersion: 2}, basis)
	criteria := record.DesignGate{Issue: "LEGION-100", Step: record.DesignStepCriteria, ArtifactID: "artifact-criteria", LatestVersion: 3}
	const shut = "The design gate of its tree stands shut, and the daemon starts no issue of that tree until it opens."
	for _, tc := range []struct {
		name  string
		steps []record.DesignStep
		gates map[record.DesignStep]record.DesignGate
		want  string
	}{
		{"a spec step that waits on the ancestor's approval request", nil,
			map[record.DesignStep]record.DesignGate{record.DesignStepSpec: pending},
			shut + " The spec step of LEGION-100 waits on the ancestor's approval request ask-90, which asks a person to approve version 5 of the primary document of LEGION-90."},
		{"a spec step open on the ancestor's approval beside a criteria step with its reviewer",
			[]record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec},
			map[record.DesignStep]record.DesignGate{record.DesignStepSpec: open, record.DesignStepCriteria: criteria},
			shut + " The criteria step of LEGION-100 waits for a person's approval of its Verification document at version 3." +
				" The spec step of LEGION-100 stands open on an ancestor's approval: a person approved version 5 of the primary document of LEGION-90."},
		{"a spec step with no basis", nil, shutGate("LEGION-100"),
			"The design gate of its tree stands shut: LEGION-100 waits for a person's approval of each gated step's document at its current version, and the daemon starts no issue of that tree until it opens."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := projectionStore{
				issues: []record.Issue{treeRoot("LEGION-100", "todo", phase.Admitted),
					treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)},
				slots:      []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: projectedAt}},
				lifecycles: []string{"LEGION-100"},
				gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-100": tc.gates},
			}
			projected, err := Project(context.Background(), nil, store, "LEGION", tc.steps, nil, nil, nil, projectedAt)
			if err != nil {
				t.Fatalf("Project: %v", err)
			}
			if row := heldRow(t, projected, "LEGION-101"); row.Reason != record.HeldDesignGate {
				t.Fatalf("the held row of LEGION-101 = %#v, want the design gate's word", row)
			}
			view := projected.Issues["LEGION-101"]
			want := api.WhyView{State: api.WhyUnlisted, Reason: tc.want}
			if view.Why == nil || *view.Why != want {
				t.Fatalf("LEGION-101's why = %+v, want %+v", view.Why, want)
			}
		})
	}
}
