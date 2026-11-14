package supervise

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// restoredRootTask is the tree root architect's claim as a restart reads it: ready, its agent
// registered, holding task 1, a task of a run nobody has taken — a start row gave it, and the root
// had no architect to hear it then (Deps.RootHears). The task names no phase and a run, and the
// claim is its tree's root, so it is the one delivery supervise holds to that answer.
func restoredRootTask() Claim {
	c := rootClaim()
	c.State = StateReady
	c.Generation = 3
	c.Session, c.SessionFile = session, sessionFile
	c.Locator = &runtime.Locator{
		Runtime:     runtime.RuntimeTmux,
		Claim:       rootToken,
		Incarnation: "4242:77",
		Tmux:        &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
	}
	c.DeliveredRows = []int64{7}
	c.TaskWritten = 1
	c.Pending = &Delivery{ID: "delivery-root", Number: 1, Origin: OriginPhaseStart, Task: "a person decided on the tree",
		Generation: 2, QueuedAt: epoch}
	return c
}

// restore builds the machine over restored, which the store holds with its pending delivery and
// the bucket with its value, as a restart finds them.
func (h *harness) restore(restored Claim) {
	h.t.Helper()
	h.token = restored.Token
	h.seedValue(&restored)
	if err := h.store.PutClaim(h.ctx, restored); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.PutDelivery(h.ctx, restored.Token, *restored.Pending); err != nil {
		h.t.Fatalf("seed the restored delivery: %v", err)
	}
	h.start(restored)
}

// A take of a root architect's held task is refused while its root has nobody to hear it
// (root-hears): the decisions it names are told by an accepted take, so a take accepted now
// would tell them to a worker no person can answer. A turn the agent starts on its own meanwhile
// is not the task's, and its end retires nothing. Once the root has an architect to hear it, the
// ready writes the value again and the take is accepted with the task's text.
func TestTakeIsRefusedByRootHearsUntilTheRootHasAnArchitect(t *testing.T) {
	h := newBareHarness(t)
	hears := false
	h.deps.RootHears = func(_ context.Context, c Claim, d Delivery) (bool, error) {
		if c.Token != rootToken || d.Generation == 0 || d.Phase != "" {
			t.Errorf("RootHears asked about claim %s and delivery %+v, want only the root architect's task of a run", c.Token, d)
		}
		return hears, nil
	}
	h.conns.Register(rootToken, h.conn)
	h.restore(restoredRootTask())

	h.must(RequestReady{Claim: rootToken, Generation: 3, Session: session})

	h.wantPrompts(0)
	if answers := h.worker.answers; len(answers) != 1 || answers[0].Outcome != TakeOutcomeRefused || answers[0].Hold != HoldRootHears {
		t.Fatalf("the worker's takes were answered %+v, want one refused by %q", answers, HoldRootHears)
	}
	if p := h.pending(); p.taken() {
		t.Fatalf("pending delivery = %+v, want the held task untaken", p)
	}

	// The agent's own turn starts and ends. Neither is the task's: no take of it was accepted.
	h.must(StreamTurnStart{Claim: rootToken})
	if p := h.pending(); p.taken() {
		t.Fatalf("pending delivery = %+v after a turn the agent started itself, want it untaken", p)
	}
	h.must(StreamTurnEnd{Claim: rootToken})
	if p := h.m.Claim().Pending; p == nil || p.taken() {
		t.Fatalf("pending delivery after the turn ended = %+v, want the held task still waiting: a task nobody took is never retired", p)
	}
	h.wantPrompts(0)

	// The root runs again, and the take of the task it held is accepted.
	hears = true
	h.must(RequestReady{Claim: rootToken, Generation: 3, Session: session})
	if sent := h.wantPrompts(1)[0]; sent.Message != "a person decided on the tree" || sent.Number != 1 {
		t.Fatalf("take = %+v, want task 1, held while the root had nobody to hear it", sent)
	}
}

// A turn the agent starts on its own — an Envoy notice's, a person's message in its session —
// takes no root architect's held task, however the machine came by it: a restored claim whose
// agent has taken nothing is not a task in flight. Only an accepted take makes a turn start the
// task's.
func TestAForeignTurnDoesNotConfirmARootArchitectsHeldTask(t *testing.T) {
	h := newBareHarness(t)
	h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return false, nil }
	h.restore(restoredRootTask())

	h.must(StreamTurnStart{Claim: rootToken})

	h.wantState(StateWorking)
	if p := h.pending(); p.taken() {
		t.Fatalf("pending delivery = %+v, want the held task untaken: the turn is the agent's own", p)
	}
	h.must(StreamTurnEnd{Claim: rootToken})
	if p := h.m.Claim().Pending; p == nil || p.taken() {
		t.Fatalf("pending delivery after the foreign turn = %+v, want the held task still waiting", p)
	}
	if stored := h.store.load(rootToken); stored.Pending == nil || stored.Pending.taken() || stored.TaskFinished != 0 {
		t.Fatalf("the store holds %+v finished %d, want the held task untaken there too: a restart must not read it as served",
			stored.Pending, stored.TaskFinished)
	}
}
