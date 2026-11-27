package api

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The gate-basis entry (contract_20261125161152_gate-basis.go) and the task-take entry
// (contract_20261125171504_task-take.go) each add words to fields older than them, and each word
// is served by its own entry's arrival, so the two strips compose. A caller below the gate-basis
// entry admits neither entry's words: it is served an issue in awaiting_children as admitted, its
// held row as children_first, and never_reconnected as never_registered. A caller between the two
// entries, at the gate-basis entry or above it and below the task-take entry, admits the first
// entry's words and not the second's: it is served awaiting_children in both places as the source
// holds it, and never_registered in place of never_reconnected. A caller at the task-take entry is
// served every word as it stands.
func TestACallerBetweenTheGateBasisAndTaskTakeEntriesIsServedEachWordByItsOwnEntry(t *testing.T) {
	state := awaitingChildrenState()
	state.Issues["LEGION-214"] = neverReconnectedIssue()
	gateAt, takeAt := entryNumber(gateBasisContract), TaskTakeContract()
	if !(gateAt < takeAt) {
		t.Fatalf("gate-basis (%d) must sit below task-take (%d) for this test to prove the step between them", gateAt, takeAt)
	}
	const operator = "192.0.2.1:41000"
	for _, tc := range []struct {
		name   string
		stated int
		phase  phase.Phase
		reason record.HeldReason
		cause  supervise.FailureCause
	}{
		{name: "a caller below both entries", stated: gateAt - 1,
			phase: phase.Admitted, reason: record.HeldChildrenFirst, cause: supervise.CauseNeverRegistered},
		{name: "a caller at the gate-basis entry", stated: gateAt,
			phase: phase.AwaitingChildren, reason: record.HeldAwaitingChildren, cause: supervise.CauseNeverRegistered},
		{name: "a caller just below the task-take entry", stated: takeAt - 1,
			phase: phase.AwaitingChildren, reason: record.HeldAwaitingChildren, cause: supervise.CauseNeverRegistered},
		{name: "a caller at the task-take entry", stated: takeAt,
			phase: phase.AwaitingChildren, reason: record.HeldAwaitingChildren, cause: supervise.CauseNeverReconnected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateStating(t, stateServer(t, state, podClaims(pauseContract)), operator, tc.stated)
			phases, reasons := phasesAndHeldWords(t, body)
			if got := phases["LEGION-208"]; got != string(tc.phase) {
				t.Errorf("a caller stating %d is served LEGION-208's phase %q, want %q", tc.stated, got, tc.phase)
			}
			if got := reasons["LEGION-208"]; got != string(tc.reason) {
				t.Errorf("a caller stating %d is served LEGION-208's held reason %q, want %q", tc.stated, got, tc.reason)
			}
			if failure, pending := neverReconnectedCauses(t, body); failure != string(tc.cause) || pending != string(tc.cause) {
				t.Errorf("a caller stating %d is served failure.cause %q and pendingCause %q, want %q in both",
					tc.stated, failure, pending, tc.cause)
			}
		})
	}
	if got := state.Issues["LEGION-214"].Architect.Failure.Cause; got != supervise.CauseNeverReconnected {
		t.Errorf("the source's failure cause = %s, want never_reconnected left as it was", got)
	}
	if got := state.Issues["LEGION-208"].Phase; got != phase.AwaitingChildren {
		t.Errorf("the source issue's phase = %s, want awaiting_children left as it was", got)
	}
}
