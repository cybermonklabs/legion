package workflow

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A restart of the tester puts it to its testing again, and its earlier completion goes with it:
// the tester completed at headOne while k2 had no state, so the move waited, and the operator then
// restarted the tester. k2's state arriving after the restart is held and moves nothing, since no
// tester's completion stands in testing until the restarted tester completes; that completion
// then decides the move, which is the control.
func TestARestartOfTheTesterLeavesNoCompletionTheStatesDecideOn(t *testing.T) {
	r := newStatesRig(t, phase.Testing)
	r.states(headOne, 1, ofSet(passOf("k1", "tester"), "k1", "k2"))
	r.complete(headOne, 1)
	r.waitsFor(headOne, "checks", "k2")
	result, err := intake.ApplyFact(context.Background(), r.pool, "api", "operator-restart",
		intake.RestartPhase{Issue: "LEGION-208", Role: claim.RoleTester}, r.engine, admissionStub{})
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply the operator's restart of the tester: %+v, %v", result.Refusal, err)
	}
	r.states(headOne, 2, ofSet(passOf("k2", "tester"), "k1", "k2"))
	assertPhase(t, r.pool, phase.Testing)
	if row := phaseRowOf(t, r.pool, claim.RoleTester); row.Completed() {
		t.Fatalf("the restarted tester's row reads completed (commit %q, record %q), want no completion until it completes again", row.HandoffCommit, row.HandoffRecord)
	}
	// The restarted tester writes its own record, as a phase's completion always names a new one.
	completed, err := intake.ApplyFact(context.Background(), r.pool, "api", "test-restarted", intake.HandoffComplete{
		Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Summary: "Every check passes.",
		Commit: headOne, HandoffRecord: "test-2", HandoffVersion: 2, StatesSerial: 2,
	}, r.engine, admissionStub{})
	if err != nil || completed.Refusal != nil {
		t.Fatalf("apply the restarted tester's completion: %+v, %v", completed.Refusal, err)
	}
	assertPhase(t, r.pool, phase.Reviewing)
}
