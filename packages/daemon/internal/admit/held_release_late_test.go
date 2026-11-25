package admit

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A release clears the holds it applied in a hook its transaction runs once it has committed, and
// that hook can run late: on the commit watcher's goroutine, when the store could say only later
// that the release committed (store.OnCommit). By then a newer event may have refreshed a held
// key's summary, or a later Reconcile may have held the key again. Neither is what the release
// applied, so each stays held for a release of its own.

// lateRelease holds LEGION-1 and LEGION-2 for the Dispatch consumer to reach position 10, and
// takes the snapshot of both that a release at that position applies.
func lateRelease(t *testing.T) (*Admission, map[string]heldSummary) {
	t.Helper()
	admission := New(record.NewStore(), nil, 1, testProject, nil)
	admission.holdDeferred(map[string]intake.DispatchIssue{
		"LEGION-1": {Key: "LEGION-1", Status: "todo", Seq: 5},
		"LEGION-2": {Key: "LEGION-2", Status: "todo", Seq: 5},
	}, 10)
	releasing := admission.releasable(intake.DispatchConsumerPosition{AckFloorStream: 10})
	if len(releasing) != 2 {
		t.Fatalf("the release at the target takes %d holds, want both", len(releasing))
	}
	return admission, releasing
}

// stillHeld is the summary admission holds for each key.
func stillHeld(admission *Admission) map[string]intake.DispatchIssue {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	held := make(map[string]intake.DispatchIssue, len(admission.pending))
	for key, hold := range admission.pending {
		held[key] = hold.summary
	}
	return held
}

func TestAReleaseClearsOnlyTheHoldsItApplied(t *testing.T) {
	admission, releasing := lateRelease(t)
	admission.refreshHeldSummary(intake.DispatchIssue{Key: "LEGION-1", Status: "backlog", Seq: 7})

	admission.clearReleased(releasing)
	held := stillHeld(admission)
	if got, ok := held["LEGION-1"]; !ok || got.Seq != 7 || got.Status != "backlog" {
		t.Errorf("LEGION-1 held %t at seq %d, status %q; want it held at its refreshed summary, seq 7: the release applied seq 5", ok, got.Seq, got.Status)
	}
	if got, ok := held["LEGION-2"]; ok {
		t.Errorf("LEGION-2 held = %+v, want it cleared: the release applied its summary", got)
	}
}

// The rehold writes the summary the release applied, so only the hold itself tells the two apart.
func TestAReholdSinceTheReleaseStaysHeld(t *testing.T) {
	admission, releasing := lateRelease(t)
	admission.holdDeferred(map[string]intake.DispatchIssue{"LEGION-1": {Key: "LEGION-1", Status: "todo", Seq: 5}}, 20)

	admission.clearReleased(releasing)
	held := stillHeld(admission)
	if _, ok := held["LEGION-1"]; !ok {
		t.Errorf("LEGION-1 is not held, want it held: a Reconcile held it again after the release took its snapshot")
	}
	if got, ok := held["LEGION-2"]; ok {
		t.Errorf("LEGION-2 held = %+v, want it cleared: the release applied its summary", got)
	}
}
