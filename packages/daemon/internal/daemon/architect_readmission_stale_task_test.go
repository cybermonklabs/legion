package daemon

import (
	"testing"
)

// A root whose architect was given its first task and never confirmed it, then parked and handed
// back, is re-admitted at the next generation, and its architect is given that generation's task
// (dispatch://INFRA-2614, R3: each task the architect is given says what is true of its root).
// The task the claim still held from the run before is no such task: it names generation 1, that
// nothing else starts the tree, and the whole first act with the decomposition, all of a run
// re-admission ended. So the first prompt the re-admitted architect is sent is the new
// generation's task, and the claim holds nothing of the old run.
//
// The path is the one the first stall arm leads to: an architect that took nothing is reported to
// the controller, and a person parks its root and sets it todo again. Measured on the whole daemon
// at c28f2ea12f32: the relaunch sent the old task of generation 1 first (outbox:5), and the
// re-admission's start only after the architect worked that one through a whole turn.
func TestAReadmittedArchitectIsNotSentTheUnconfirmedTaskOfTheRunBefore(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	held := r.machine().Claim().Pending
	if held == nil || held.Generation != 1 || !held.ConfirmedAt.IsZero() {
		t.Fatalf("the architect's task before the park = %#v, want its generation-1 task, sent and unconfirmed", held)
	}

	r.park()
	r.runSuspend()
	r.handBack()
	r.runStarts()
	sent := len(r.prompts())
	r.bringUp()

	prompts := r.prompts()[sent:]
	if len(prompts) == 0 {
		t.Fatalf("the re-admitted architect was sent nothing; it holds %#v", r.machine().Claim().Pending)
	}
	if first := prompts[0]; hasTaskLines(first.Message, "generation: 1") || !hasTaskLines(first.Message, "generation: 2") {
		t.Fatalf("the re-admitted architect's first prompt is %s: %q, the unconfirmed task of the run re-admission ended, want the task of generation 2",
			first.DeliveryID, first.Message)
	}
	if pending := r.machine().Claim().Pending; pending != nil && pending.Generation != 2 {
		t.Fatalf("the re-admitted architect holds %#v, want nothing of generation 1", pending)
	}
}
