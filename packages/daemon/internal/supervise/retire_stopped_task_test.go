package supervise

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The task of a phase the workflow stopped under its worker (a parent moved into
// awaiting_children) is retired, and nothing of it is sent afterwards: not to the agent a
// relaunch or a resume brings back during the wait, and not after the return puts the issue back
// in that same phase at the same run, where the phase fence holds a task of that phase again. A
// task of another phase, another run or a later entry is not this phase's work and stays; the
// return's own start, on an entry of its own, is delivered as any start is.
func TestRetiringAStoppedPhasesTaskLeavesNothingOfItToSend(t *testing.T) {
	restored := fixture(StateReady)
	restored.Pending.Phase, restored.Pending.Generation, restored.Pending.Entry = phase.Implementing, restored.Generation, 3
	h := newHarnessOf(t, restored)
	if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	// The issue stands in implementing again, as it does after the return: the fence that drops a
	// task of a phase the issue has left holds this one.
	h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return true, nil }
	// No agent is connected yet, so nothing is sent while the claim is built.
	h.start(restored)

	for _, other := range []struct {
		name       string
		phase      phase.Phase
		generation uint64
		entry      int64
	}{
		{name: "another phase", phase: phase.Reviewing, generation: restored.Generation, entry: 3},
		{name: "another run", phase: phase.Implementing, generation: restored.Generation + 1, entry: 3},
		{name: "a later entry than the stop names", phase: phase.Implementing, generation: restored.Generation, entry: 2},
	} {
		retired, err := h.m.RetireStoppedTask(h.ctx, other.phase, other.generation, other.entry)
		if err != nil || retired || h.m.Claim().Pending == nil {
			t.Fatalf("a retirement of %s = %t, %v, pending %+v; want the task left where it is", other.name, retired, err, h.m.Claim().Pending)
		}
	}

	retired, err := h.m.RetireStoppedTask(h.ctx, phase.Implementing, restored.Generation, 3)
	if err != nil || !retired {
		t.Fatalf("the retirement of the stopped phase's task = %t, %v; want it retired", retired, err)
	}
	if p := h.m.Claim().Pending; p != nil {
		t.Fatalf("the stopped phase's task is still pending as %+v", p)
	}

	// The relaunched agent connects and says it is ready: nothing of the stopped phase is sent.
	h.conns.Register(testToken, h.conn)
	h.must(RequestReady{Claim: testToken, Generation: restored.Generation, Session: session})
	h.wantPrompts(0)

	// The return's own start, on an entry of its own, reaches the worker.
	h.must(RequestDeliver{Claim: testToken, Task: "carry the implementation on after the wait", ID: "outbox:9",
		Phase: phase.Implementing, Generation: restored.Generation, Entry: 4})
	if sent := h.wantPrompts(1)[0]; !strings.Contains(sent.Message, "carry the implementation on after the wait") {
		t.Fatalf("prompt after the return = %+v, want the return's own task", sent)
	}
}

// The retirement of a stopped phase's task holds to the rules a busy answer, a late turn and a
// relaunch's record keep (dispatch://INFRA-3276 R3 and R4), in each state those rules leave the
// task in. A task whose prompt is still leaving waits for the send (ErrTaskSending) and goes once
// it has settled. A task whose prompt Oh My Pi acknowledged and has not started a turn on goes
// with that prompt's wait for its turn, so the return's own start is sent at once rather than held
// behind the retired prompt, and the late turn of that prompt is no task's: Oh My Pi answers the
// return's prompt busy, and the end of that turn sends the return's task again. A task a busy
// answer held goes, and the turn that answer recorded runs on as no task's turn and sends nothing.
// A task a death took back from its turn goes, and the record of that death, which the task would
// have carried in front of itself, is sent alone, as at every drop of sendPending's; the return's
// own start then reaches the worker with nothing in front of it.
func TestRetiringAStoppedPhasesTaskHoldsToTheBusyLateAndRelaunchRules(t *testing.T) {
	const run, stoppedEntry = 5, 3
	stopped := RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:3",
		Phase: phase.Implementing, Generation: run, Entry: stoppedEntry}
	returned := RequestDeliver{Claim: testToken, Task: "carry the implementation on after the wait", ID: "outbox:9",
		Phase: phase.Implementing, Generation: run, Entry: stoppedEntry + 1}
	retire := func(h *harness) {
		h.t.Helper()
		retired, err := h.m.RetireStoppedTask(h.ctx, phase.Implementing, run, stoppedEntry)
		if err != nil || !retired {
			h.t.Fatalf("the retirement of the stopped phase's task = %t, %v; want it retired", retired, err)
		}
	}
	turnBound := func(h *harness) bool {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		_, armed := h.m.timers[TimerTurn]
		return armed
	}

	t.Run("a task on its way waits for its send", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		gated := newGatedConn()
		h.conns.Register(testToken, gated)
		if err := h.m.Handle(h.ctx, stopped); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the send", gated.entered)
		if retired, err := h.m.RetireStoppedTask(h.ctx, phase.Implementing, run, stoppedEntry); retired || !errors.Is(err, ErrTaskSending) {
			t.Fatalf("a retirement while the prompt is leaving = %t, %v; want ErrTaskSending", retired, err)
		}
		if p := h.pending(); p.ID != stopped.ID {
			t.Fatalf("pending while the prompt is leaving = %+v, want the stopped phase's task kept", p)
		}
		gated.release <- struct{}{}
		h.m.Wait()

		retire(h)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the stopped phase's task is still pending as %+v", p)
		}
		if turnBound(h) {
			t.Fatal("the wait for the retired prompt's turn is still armed")
		}
	})

	t.Run("an acknowledged prompt whose turn has not started", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(stopped)
		late := h.wantPrompts(1)[0].DeliveryID
		if !turnBound(h) {
			t.Fatal("the acknowledged prompt waits for its turn on nothing, want the bound armed")
		}

		retire(h)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the stopped phase's task is still pending as %+v", p)
		}
		if turnBound(h) {
			t.Fatal("the wait for the retired prompt's turn is still armed, which holds every later send")
		}
		if outstanding := h.outstanding(); len(outstanding) != 0 {
			t.Fatalf("prompts still waiting on a result after the retirement = %v, want none", outstanding)
		}

		// Oh My Pi still holds the retired prompt, admitted, so it answers the next prompt busy.
		h.conn.SetStreaming(true)
		h.conn.FailPrompt(busyRefusal())
		h.must(returned)
		h.wantPrompts(2)
		h.wantState(StateWorking)
		// The retired prompt's turn starts late, and it is not the return's.
		h.must(StreamTurnStart{Claim: testToken})
		if p := h.pending(); p.Task != returned.Task || !p.ConfirmedAt.IsZero() {
			t.Fatalf("pending in the retired prompt's turn = %+v, want the return's task waiting unconfirmed", p)
		}
		h.conn.FailPrompt(nil)
		h.conn.SetStreaming(false)
		h.mustQuiet(StreamTurnEnd{Claim: testToken})
		h.must(StreamPromptResult{Claim: testToken, DeliveryID: late, AgentInvoked: true})
		if sent := h.wantPrompts(3)[2]; sent.Message != returned.Task || sent.DeliveryID != h.pending().ID {
			t.Fatalf("sent after the retired prompt's turn = %+v, want the return's task under its pending id", sent)
		}
		if p := h.pending(); p.Task != returned.Task || !p.ConfirmedAt.IsZero() {
			t.Fatalf("pending after the retired prompt's result = %+v, want the return's task waiting for its own turn", p)
		}
	})

	t.Run("a task a busy answer holds", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.conn.SetStreaming(true)
		h.conn.FailPrompt(busyRefusal())
		h.must(stopped)
		h.wantPrompts(1)
		h.wantState(StateWorking)

		retire(h)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the stopped phase's task is still pending as %+v", p)
		}
		// The claim keeps the turn the busy answer recorded; its start and its end send nothing.
		h.wantState(StateWorking)
		h.must(StreamTurnStart{Claim: testToken})
		h.conn.FailPrompt(nil)
		h.conn.SetStreaming(false)
		h.mustQuiet(StreamTurnEnd{Claim: testToken})
		h.wantState(StateIdle)
		h.wantPrompts(1)
	})

	t.Run("a relaunch's record that still tells", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(stopped)
		h.must(StreamTurnStart{Claim: testToken})
		h.reportActivity("subagent")
		h.observe(runtime.Gone)
		if p := h.pending(); !p.Interrupted || p.Phase != phase.Implementing {
			t.Fatalf("the relaunching claim holds %+v, want the stopped phase's task its death took back", p)
		}
		// The relaunched process says it is ready while no connection carries a prompt to it, so
		// the task its death took back is held, and the ready queues no delivery of the record.
		h.wantState(StateLaunching)
		h.connect()
		h.register()
		h.conns.Remove(testToken)
		h.ready()
		h.wantPrompts(1)
		ended := h.claim().EndedWork
		if !ended.Tells() || !ended.TaskTurn {
			t.Fatalf("the death recorded %+v, want an untold record of the task's turn", ended)
		}

		retire(h)
		p := h.pending()
		if !relaunchTask(p.ID) || p.Task != RelaunchSentence(ended) || p.Phase != "" {
			t.Fatalf("after the retirement the claim holds %+v, want the daemon's own delivery of the record", p)
		}
		if retired, err := h.m.RetireStoppedTask(h.ctx, phase.Implementing, run, stoppedEntry); err != nil || retired {
			t.Fatalf("the row's retry = %t, %v; want the record's delivery, which names no phase, left where it is", retired, err)
		}

		// The connection comes back: the agent reads the record's sentence alone.
		h.connect()
		if sent := h.wantPrompts(2)[1]; sent.DeliveryID != p.ID || sent.Message != RelaunchSentence(ended) {
			t.Fatalf("the relaunched claim was sent %+v, want the record's sentence alone under %s", sent, p.ID)
		}
		h.must(StreamTurnStart{Claim: testToken})
		h.wantEndedTold(true, p.ID, "once the sentence's turn began")
		h.must(StreamTurnEnd{Claim: testToken})

		// The return's own start reaches the worker with nothing in front of it: the record is told.
		h.must(returned)
		if sent := h.wantPrompts(3)[2]; sent.Message != returned.Task {
			t.Fatalf("prompt after the return = %+v, want the return's task alone", sent)
		}
	})
}
