package admit

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// An operator's `legion claims stop` of a phase worker holds against admission's repair of a slot
// whose worker has no task (resumeSlottedWorkers): the stop retires the claim, and the repair
// passes over a retired claim, so no pass hands the operator back, a second later, the worker they
// stopped. What the stop costs is read here too: the issue keeps its admission slot with nobody in
// it, which the workflow's own starts — the held phase's retry, the tree's re-admission — are what
// end.
//
// The fixture is one the repair would otherwise act on. The member holds a slot, stands in a worker
// phase of a tree that runs, and owes no queued start, so every term of the repair's rule is met
// but the claim's state. The claim holds the task of that phase and generation confirmed, which a
// claim that is not working no longer works (supervise.Delivery.StillWorked), so what the repair
// reads is not its older skip of a task the claim still has in hand.
//
// The contrast run is what says so. The same fixture with the claim suspended by the daemon rather
// than retired — the one difference between the two runs — gets its start from the first pass. A
// fixture the repair never reached would give that run no start either, and the retired run would
// prove nothing.
func TestARetiredWorkersSlotIsNotRepaired(t *testing.T) {
	const root, member = "LEGION-240", "LEGION-241"
	// stopped records the fixture every row of slot_repair_test.go's first group is written from
	// (seedSlotRepairRow), so the slot this test reads and the slots that hold the repair's rule
	// to the stop are one fixture: the root waiting in accepting while its member works, so it
	// holds no slot of its own and the member's is the one slot the cap gives out; the member
	// slotted mid-testing at generation 1; the tester's phases row with the assignment a repair
	// hands back; and the claim in state with suspendedBy, holding that phase's task confirmed.
	// Nothing else differs between the two runs: the claim's session, its task and the slot are
	// the same in both.
	stopped := func(t *testing.T, state supervise.ClaimState, suspendedBy supervise.SuspendSource) *pgxpool.Pool {
		t.Helper()
		pool := migratedPool(t)
		seedSlotRepairRow(t, pool, slotRepairRow{root: root, key: member, stands: phase.Testing, generation: 1,
			state: state, suspendedBy: suspendedBy, taskGeneration: 1, confirmed: true})
		return pool
	}
	// The fact that drives the Dispatch pass: a todo issue of the project nobody handed to Legion,
	// which admission records nothing of and promotes nothing for (Admission.applyFact). So the
	// repair is the only thing the pass can write for the member.
	elsewhere := intake.DispatchIssue{Key: "LEGION-900", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated",
		Status: "todo", Title: "an issue of another team", Rank: "Z"}

	pool := stopped(t, supervise.StateRetired, "")
	admission := quiet(t, 1)
	for _, pass := range []struct {
		name string
		run  func()
	}{
		{name: "the pass a Dispatch fact drives", run: func() { applyFact(t, pool, admission, "dispatch-1", elsewhere) }},
		{name: "the controller tick", run: func() { tick(t, pool, admission, "tick-1") }},
		{name: "the boot's read", run: func() { reconcile(t, pool, admission, nil) }},
	} {
		pass.run()
		if got := startsOf(t, pool, member); len(got) != 0 {
			t.Fatalf("after %s the tester of %s has the start rows %+v, want none: the operator stopped that worker, so its claim is retired and no pass starts its phase again",
				pass.name, member, got)
		}
		if !holdsSlot(t, pool, member) {
			t.Fatalf("after %s the slots are %#v, want %s to keep its own: the stop leaves the slot held with no worker in it until a start of the workflow's own",
				pass.name, slots(t, pool), member)
		}
	}

	// The same fixture, the claim suspended by the daemon instead of stopped. The repair reaches
	// it and starts the phase again, as the one row that carries the task, held for the issue's
	// own slot (workflow.StartOrOwe).
	resumable := stopped(t, supervise.StateSuspended, supervise.SuspendedByDaemon)
	applyFact(t, resumable, quiet(t, 1), "dispatch-1", elsewhere)
	rows := startsOf(t, resumable, member)
	owed := owedStarts(t, resumable, member)
	if len(rows) != 1 || len(owed) != 1 || owed[0].role != claim.RoleTester || owed[0].task == "" {
		t.Fatalf("the suspended tester of %s has the start rows %+v, want the one carrying a task, the tester's, marked for the issue's slot: the repair acts on this fixture, so what the retired run above reads is the claim's state",
			member, rows)
	}
}
