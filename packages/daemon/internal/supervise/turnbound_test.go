package supervise

import (
	"testing"
	"time"
)

// turnBound is the bound these tests end a controller turn past (controller_turn_bound_minutes'
// default, dispatch://INFRA-2045 R2).
const turnBound = 90 * time.Minute

// inATurn is the controller handed its start message and in the turn that message began, at the
// harness clock's moment: the turn whose start a TurnPastBound names.
func (h *harness) inATurn() time.Time {
	h.t.Helper()
	h.must(RequestDeliver{Claim: h.token, Task: controllerStart})
	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
	return h.claim().LastTurnStart
}

// wantNoStartSince is a claim the runtime was asked to start nothing for since the counts given.
func (h *harness) wantNoStartSince(resumes, spawns int) {
	h.t.Helper()
	if gotResumes, gotSpawns := h.startsSoFar(); gotResumes != resumes || gotSpawns != spawns {
		h.t.Fatalf("the runtime was asked for %d resumes and %d spawns since, want none", gotResumes-resumes, gotSpawns-spawns)
	}
}

// The machine's half of dispatch://INFRA-2045 R2, on the harness's moved clock: a controller turn
// open longer than its bound is ended when the keeper says so (TurnPastBound), whether or not steps
// still arrive in it, and the end counts under R1; the machine re-reads the turn first, and leaves
// alone a turn inside the bound, a turn that ended, and a turn begun since the keeper's read.
// internal/daemon's test of the same name holds the keeper's half, and internal/config's the
// default of 90. The control: on main no such event exists, so the test cannot compile there.
func TestAControllerTurnOpenPastItsBoundIsEnded(t *testing.T) {
	t.Run("a: a turn open one minute past the bound, steps still arriving, is ended and counted, and the session resumed", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		h.advance(45 * time.Minute)
		h.stepped("bash", "toolu_long_1", `{"command":"sleep 1"}`, false)
		h.advance(46 * time.Minute)
		h.stepped("bash", "toolu_long_1", `{"command":"sleep 1"}`, true)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)
		if p := h.claim().Pending; p == nil || p.Task != controllerStart {
			t.Fatalf("the relaunched controller holds %+v, want the task its ended turn served still owed", p)
		}
	})

	t.Run("b: a second such end in a row makes the next launch fresh", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		h.advance(turnBound + time.Minute)
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.relaunched()
		h.must(StreamTurnStart{Claim: h.token})
		start = h.claim().LastTurnStart
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantFreshSince(resumes, spawns, session)
	})

	t.Run("c: a turn that ends one minute before the bound changes nothing", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		h.advance(turnBound - time.Minute)
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.advance(2 * time.Minute)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantState(StateIdle)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
	})

	t.Run("the machine re-reads the turn: one inside the bound, and one begun since the keeper's read, are left alone", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		h.advance(turnBound - time.Minute)
		resumes, spawns := h.startsSoFar()
		// The machine's own clock decides, not the keeper's read.
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.wantState(StateWorking)

		// The turn ends and another begins; the keeper read the first.
		h.must(StreamTurnEnd{Claim: h.token})
		h.must(RequestDeliver{Claim: h.token, Task: "the next wake"})
		h.must(StreamTurnStart{Claim: h.token})
		h.advance(turnBound + time.Minute)
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantState(StateWorking)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
	})

	t.Run("a refused take-back ends nothing and counts nothing, and the next post counts the turn once", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.store.fail("PutDelivery", errBoom)
		_ = h.handle(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.wantState(StateWorking)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)

		// The store answers again, and the keeper's next look posts the same turn.
		h.store.fail("PutDelivery", nil)
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)
	})

	t.Run("an operator's compaction past the bound is left alone and counts nothing", func(t *testing.T) {
		h := controllerUp(t)
		start := h.claim().LastTurnStart
		release, compacted := h.compactHeld()
		h.wantState(StateWorking)
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantState(StateWorking)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
		release()
		if err := <-compacted; err != nil {
			t.Fatalf("the compaction answered %v, want it run to its end", err)
		}
	})
}
