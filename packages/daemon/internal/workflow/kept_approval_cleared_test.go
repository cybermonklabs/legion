package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A kept approval is a person's recorded decision, and the one predicate decides whether it is still
// that person's word (dispatch://INFRA-3136/spec R2). A later changes request clears the gate's
// approval (classify.ApplyDesignGateEvent), so the person's word is now that request: the start task
// and the catch-up, which both render through KeptDecisionsTask, must not tell the architect the
// document is approved at the kept version. Asked of a gate that holds that approval, the sentence
// stands; asked of a gate a later approval moved on, or one a changes request cleared, it goes.
func TestAKeptApprovalALaterChangesRequestClearedIsNotNamed(t *testing.T) {
	t.Parallel()
	const sentence = "is approved at version 2"
	owed := record.KeptDecisions{KeptApproval: record.KeptApproval{Step: record.DesignStepSpec, Approved: 2, Latest: 3}}
	for _, tc := range []struct {
		name  string
		gate  record.DesignGate
		named bool
	}{
		{name: "the gate still records the kept approval", gate: record.DesignGate{LatestVersion: 3, ApprovedVersion: new(2)}, named: true},
		{name: "a later approval replaced it", gate: record.DesignGate{LatestVersion: 3, ApprovedVersion: new(3)}},
		{name: "a later changes request cleared it", gate: record.DesignGate{LatestVersion: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gates := KeptGates{"LEGION-208": {record.DesignStepSpec: tc.gate}}
			task, kept := KeptDecisionsTask(gates, specGated, "LEGION-208", owed)
			if strings.Contains(task, sentence) != tc.named || (kept.Approved > 0) != tc.named {
				t.Fatalf("the task %q names %q = %t and keeps approval %d, want named %t",
					task, sentence, strings.Contains(task, sentence), kept.Approved, tc.named)
			}
		})
	}
}
