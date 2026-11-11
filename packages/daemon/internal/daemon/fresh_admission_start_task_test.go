package daemon

import (
	"strings"
	"testing"
)

// A root whose admission has just given it a slot is the one case where nothing the daemon had
// already written tells its architect what to do: no phase of its tree has ended, no person has
// answered anything, and nobody has decided anything on it while it waited. Before this test the
// architect start admission queued for such a root carried no task at all — a task only where a
// person's decision had been kept for it (workflow.ParkedDecisionTask) — so the daemon launched
// the architect of a fresh tree with nothing to do, and the first thing it would ever be given was
// the catch-up its ready writes, which states the tree and leaves the first act to the agent. A
// root measured that way sat in its slot with an idle architect that had neither claimed it nor
// registered its design gate until a person messaged the session.
//
// So the start carries the root's own first task, as a phase worker's start carries its phase's,
// and this holds the whole path main runs: admission queues the start, the outbox runner launches
// the architect on it, and the agent that comes up is prompted that task in its first turn, with
// no message from anyone.
func TestAFreshAdmissionGivesItsArchitectItsStartTask(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()

	pending := r.machine().Claim().Pending
	if pending == nil || pending.Task == "" {
		t.Fatalf("the architect's pending task after its admission's start = %#v, want the start carrying the root's first task", pending)
	}
	for _, want := range []string{mainPathRoot, "claim", "design gate"} {
		if !strings.Contains(pending.Task, want) {
			t.Fatalf("the architect's start task = %q, want it naming %q", pending.Task, want)
		}
	}

	r.bringUp()
	prompts := r.prompts()
	if len(prompts) != 1 || prompts[0].Message != pending.Task {
		t.Fatalf("the architect was prompted %#v, want the one start task %q in its first turn", prompts, pending.Task)
	}
}
