package daemon

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root whose admission has just given it a slot is the one case where nothing the daemon has
// already written tells its architect what to do: no phase of its tree has ended, no person has
// answered anything, and nobody has decided anything on it while it waited. The start admission
// queues for such a root carries the root's own first task, of the shape of a tree nobody has
// architected (workflow.ArchitectStartTask): it asks the architect to claim the tree, have its
// spec written, file every child of its decomposition and register its design gate. Without that
// task the first thing the architect would be given is the catch-up its ready writes, which
// states the tree and leaves the first act to the agent.
//
// The start carries the phase entry admission raised for that architect
// (record.SuperviseRequest.Entry), and the architect's phases row carries the same number with
// the task as the work of it: a root's architect holds that row as every phase worker does, so
// the delivery records what it was given on the claim (supervise.Claim.GivenEntry) and the work
// survives the row that carried it.
//
// The whole path main runs is held here: admission queues the start, the outbox runner launches
// the architect on it, and the agent that comes up is prompted that task in its first turn, with
// no message from anyone.
//
// Nothing wakes the project's controller about this root. Each of the three stall arms is a
// reading that the architect did not take its work, is not there, or has not moved (admit's
// architectStalls), and a root the daemon has just served is none of the three: a wake here
// would send a person after a tree that is running.
func TestAFreshAdmissionGivesItsArchitectItsStartTask(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()

	pending := r.machine().Claim().Pending
	if pending == nil || pending.Task == "" {
		t.Fatalf("the architect's pending task after its admission's start = %#v, want the start carrying the root's first task", pending)
	}
	for _, want := range []string{mainPathRoot, workflow.ArchitectFreshTreeLabel, workflow.ArchitectDecompositionLabel} {
		if !strings.Contains(pending.Task, want) {
			t.Fatalf("the architect's start task = %q, want it naming %q: a fresh tree's architect is given the whole first act", pending.Task, want)
		}
	}
	row := rolePhaseRow(t, r.pool, mainPathRoot, claim.RoleArchitect)
	if row.Entry != 1 || row.EntryTask != pending.Task || row.EntryRaisedAt.IsZero() {
		t.Fatalf("the architect's phases row = entry %d, task %q, raised at %s; want entry 1 carrying the task it was given, with the moment it was raised",
			row.Entry, row.EntryTask, row.EntryRaisedAt)
	}
	if row.EntryVerbatim {
		t.Fatalf("the architect's phases row marks the derivation's task %q to be sent again as written, want it derived again: "+
			"a send again names what is still unclaimed then (workflow.Engine.SendArchitectStart)", row.EntryTask)
	}
	if pending.Entry != 1 {
		t.Fatalf("the architect's pending task is of entry %d, want the 1 its row names: the start carries the entry its own transaction raised", pending.Entry)
	}
	if given := r.machine().Claim().GivenEntry; given != 1 {
		t.Fatalf("the architect's claim was given entry %d, want 1: the delivery records the entry it carried", given)
	}

	r.bringUp()
	prompts := r.prompts()
	if len(prompts) != 1 || prompts[0].Message != pending.Task {
		t.Fatalf("the architect was prompted %#v, want the one start task %q in its first turn", prompts, pending.Task)
	}
	// The arms are read on every pass of the project, so one runs here with the architect up and
	// holding the task it was sent.
	r.admitPass()
	if reasons := architectStalledWakes(t, r.pool, mainPathRoot); len(reasons) != 0 {
		t.Fatalf("the architect wakes of %s = %q, want none: the root was served its first task, so no arm of the stall reading holds",
			mainPathRoot, reasons)
	}
}
