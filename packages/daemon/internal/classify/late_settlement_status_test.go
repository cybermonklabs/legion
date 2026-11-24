package classify

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// The listener settles a head again when one of its commit statuses changes state, with the same
// check runs: an earlier settlement of the head can differ from a later one in a status alone.
// While the fence stands at the head, its lower generation and its older status make it stale.
// Once the fence is reset - a reopen, or a new generation's clear - an earlier settlement of the
// same runs and checks, delivered late or replayed from a give-up, must still leave the head's
// statuses as the later settlement left them: here, deploy's success must not go back to the
// failure it replaced.
func TestALateSettlementWithAnOlderStatusOfAHeadWhoseFenceWasResetChangesNothing(t *testing.T) {
	earlier := SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}}, Generation: 1,
		Snapshot: "head-1", Failing: []string{}, Cancelled: []string{},
		Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusFailure, 1)}}
	later := SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}}, Generation: 2,
		Snapshot: "head-2", Failing: []string{}, Cancelled: []string{},
		Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 2)}}
	kept := []record.CommitStatus{keptStatus("deploy", record.CommitStatusSuccess, 2, "head")}

	settled, applied := ApplyCheckSettlement(record.PullRequest{HeadSHA: "head", Failing: []string{}}, earlier)
	if !applied {
		t.Fatal("the earlier settlement did not apply to a head with no settlement")
	}
	settled, applied = ApplyCheckSettlement(settled, later)
	if !applied || !reflect.DeepEqual(settled.Statuses, kept) {
		t.Fatalf("the later settlement applied %v with statuses %+v; want deploy's success kept", applied, settled.Statuses)
	}
	if got, applied := ApplyCheckSettlement(settled, earlier); applied || !reflect.DeepEqual(got.Statuses, kept) {
		t.Fatalf("with the fence at head the earlier settlement applied %v, statuses %+v; want it stale", applied, got.Statuses)
	}
	cleared := settled
	cleared.CheckedHead, cleared.Failing, cleared.Cancelled, cleared.CheckRuns, cleared.Statuses = "", []string{}, []string{}, []record.AttemptRun{}, []record.CommitStatus{}
	for name, reset := range map[string]record.PullRequest{"a reopen": Reopen(settled, "head"), "a new generation's clear": cleared} {
		got, applied := ApplyCheckSettlement(reset, earlier)
		for _, status := range got.Statuses {
			if applied && status.Context == "deploy" && status.State == record.CommitStatusFailure {
				t.Errorf("after %s the earlier settlement applied and put deploy back to failure (statuses %+v, generation %d); want deploy's success kept",
					name, got.Statuses, got.Generation)
			}
		}
	}
}
