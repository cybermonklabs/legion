package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// An update round's question waits behind the task its claim holds (supervise's
// Claim.OwesUpdateAsk): the sweep enqueues no ask for the implementer while it holds a task, in
// its turn or kept for its next send, and asks it once the task's turn ends with no error. The
// tester, which holds no task, is asked at the first sweep: the control that the round asks at
// all, through the same sweep and the same lane.
func TestPodUpdateAskWaitsBehindTheTaskItsClaimHolds(t *testing.T) {
	const task = "outbox:7"
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	implementer := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: implementer, ID: task,
		Task: "Carry the implementation on.", Phase: phase.Implementing, Generation: 1})
	rig.settle(claim.RoleImplementer)
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: implementer})
	rig.drifts(podUpdateDrift)
	asksOf := func(role claim.Role) int {
		asked := 0
		for _, ask := range rig.asks() {
			if ask.Role == role {
				asked++
			}
		}
		return asked
	}

	rig.tick()
	rig.awaitAsked(claim.RoleTester, podUpdateRound)
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	for range 3 {
		rig.tick()
	}
	if n := asksOf(claim.RoleImplementer); n != 0 {
		t.Fatalf("the implementer in the turn of its task was asked %d times, want none while it holds the task", n)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateTaskedAt != nil || c.Pending == nil || c.Pending.ID != task {
		t.Fatalf("the implementer records an ask at %v and holds %+v, want no ask and its task", c.UpdateTaskedAt, c.Pending)
	}

	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: implementer})
	rig.handle(claim.RoleImplementer, supervise.StreamPromptResult{Claim: implementer, DeliveryID: task, AgentInvoked: true})
	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if n := asksOf(role); n != 1 {
			t.Errorf("the %s was asked %d times, want once", role, n)
		}
	}
}
