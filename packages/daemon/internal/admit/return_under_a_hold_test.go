package admit

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// R19 of dispatch://INFRA-3320: an issue with an issue above it in Legion's work "is held, with
// its reason and the issue that holds it, in three cases: ancestry while its parent is not in
// Legion's work and an issue above the parent is ...; root_awaiting_person while its top issue
// waits on a person"; R21: every issue in Legion's work stands in admission.active or
// admission.waiting, "or else in admission.held". An issue the daemon started, which a stop left
// at backlog in implementing, comes back on a person's todo while one of those holds stands over
// it. R20 puts its return in the line among the work that came back, and the hold keeps it there:
// it takes no slot, no start of its phase's worker can run, and the held list names it with the
// hold and the issue that holds it.
func TestAReturnUnderAHoldStartsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason record.HeldReason
	}{
		{name: "ancestry: its parent on the backlog under a root at work", reason: record.HeldAncestry},
		{name: "root_awaiting_person: its top issue waits on a person", reason: record.HeldRootAwaitingPerson},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const top, middle, child = "LEGION-650", "LEGION-651", "LEGION-652"
			pool := migratedPool(t)
			admission := quiet(t, 4)
			parent := top
			switch tc.reason {
			case record.HeldAncestry:
				seedRunning(t, pool, top, "")
				stoppedParent := chainRecord(middle, top, phase.Implementing, "backlog")
				stoppedParent.Tree = top
				putIssue(t, pool, stoppedParent)
				parent = middle
			case record.HeldRootAwaitingPerson:
				putIssue(t, pool, quietRootRecord(top, top, phase.AwaitingPerson))
				takeSlot(t, pool, top)
				seedLifecycle(t, pool, top)
			}
			stopped := chainRecord(child, parent, phase.Implementing, "backlog")
			stopped.Tree, stopped.LastDispatchSeq = top, 1
			putIssue(t, pool, stopped)
			spSeedPhase(t, pool, record.PhaseRow{Issue: child, Role: claim.RoleImplementer, Claim: "implementer", Entry: 3,
				EntryTask: "Continue " + child + ".", EntryRaisedAt: fixedNow.Add(-2 * time.Hour)})
			putWorkerClaim(t, pool, top, child, claim.RoleImplementer, 1, supervise.StateIdle, "")
			putWorkerClaim(t, pool, top, child, claim.RoleArchitect, 1, supervise.StateIdle, "")

			apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "todo", Title: child, Parent: parent, Rank: child}, admission.engine)

			back := issue(t, pool, child)
			if back.Generation != 1 || back.Phase != phase.Implementing {
				t.Fatalf("%s after the todo = phase %s generation %d, want it back in implementing at generation 1", child, back.Phase, back.Generation)
			}
			if holdsSlot(t, pool, child) {
				t.Errorf("%s holds a slot after its return, want none while %s holds it", child, tc.reason)
			}
			for _, start := range pwWorkerStarts(t, pool, child) {
				if start.claimable && start.task != "" {
					t.Errorf("%s has a worker start with work that can run after its return (%+v), want none while %s holds it", child, start, tc.reason)
				}
			}
			line, held := passJudgement(t, pool, admission)
			for _, key := range line {
				if key == child {
					t.Errorf("the waiting line = %v, want no %s while %s holds it", line, child, tc.reason)
				}
			}
			assertHeldBy(t, held, child, tc.reason, top)
		})
	}
}
