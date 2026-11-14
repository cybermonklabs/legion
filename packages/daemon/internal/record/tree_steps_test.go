package record

import (
	"testing"
	"time"
)

// TreeGate reads the steps its tree is gated on in all three of its answers, so under the one
// step spec a registered criteria row holds them as the spec row does: the gate opens only with
// both rows approved, it has opened once only when both have, and it opened at the later of the
// two approvals. A tree without a criteria row answers from its spec row alone, and under `off`
// a criteria row kept from before is read by none of the three.
func TestTreeGateReadsARegisteredCriteriaRowUnderSpecAlone(t *testing.T) {
	specAt := time.Date(2026, 11, 13, 17, 0, 0, 0, time.UTC)
	criteriaAt := specAt.Add(time.Minute)
	spec := DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-spec", LatestVersion: 4,
		ApprovedVersion: new(4), ApprovedMark: EventMark{Seq: 7, At: specAt}, EverOpened: true}
	criteria := DesignGate{Issue: "LEGION-208", Step: DesignStepCriteria, ArtifactID: "artifact-verification", LatestVersion: 2,
		ApprovedVersion: new(2), ApprovedMark: EventMark{Seq: 8, At: criteriaAt}, EverOpened: true}
	criteriaClosed := DesignGate{Issue: "LEGION-208", Step: DesignStepCriteria, ArtifactID: "artifact-verification", LatestVersion: 2}
	specOnly := []DesignStep{DesignStepSpec}
	for _, tc := range []struct {
		name       string
		gate       TreeGate
		open, ever bool
		at         time.Time
	}{
		{name: "both approved", gate: TreeGate{Steps: specOnly, Rows: map[DesignStep]DesignGate{DesignStepSpec: spec, DesignStepCriteria: criteria}},
			open: true, ever: true, at: criteriaAt},
		{name: "the criteria row never approved", gate: TreeGate{Steps: specOnly, Rows: map[DesignStep]DesignGate{DesignStepSpec: spec, DesignStepCriteria: criteriaClosed}}},
		{name: "no criteria row", gate: TreeGate{Steps: specOnly, Rows: map[DesignStep]DesignGate{DesignStepSpec: spec}},
			open: true, ever: true, at: specAt},
		{name: "off, a closed criteria row kept", gate: TreeGate{Steps: []DesignStep{}, Rows: map[DesignStep]DesignGate{DesignStepSpec: spec, DesignStepCriteria: criteriaClosed}},
			open: true, ever: true, at: specAt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.gate.Open(); got != tc.open {
				t.Errorf("Open = %t, want %t", got, tc.open)
			}
			if got := tc.gate.EverOpened(); got != tc.ever {
				t.Errorf("EverOpened = %t, want %t", got, tc.ever)
			}
			if got := tc.gate.OpenedAt(); !got.Equal(tc.at) {
				t.Errorf("OpenedAt = %s, want %s", got, tc.at)
			}
		})
	}
}
