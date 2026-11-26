package supervise

import (
	"context"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The task of a phase the workflow stopped under its worker (a parent moved into
// awaiting_children) is retired by RetireStoppedTask, and these tests drive it on the take: the
// task is a value the worker takes (take.go), so what must not happen is a take of the stopped
// phase's task, during the wait or after the return puts the issue back in that same phase at the
// same run, where the phase fence holds a task of that phase again. Every harness here answers that
// fence with true, as the record does after the return.

const (
	stoppedRun   = 5
	stoppedEntry = 3
)

// stoppedTask is the start of the phase the move stops, and returnedTask the return's own start, on
// an entry of its own.
var (
	stoppedTask = RequestDeliver{Task: "implement the plan", ID: "outbox:3",
		Phase: phase.Implementing, Generation: stoppedRun, Entry: stoppedEntry}
	returnedTask = RequestDeliver{Task: "carry the implementation on after the wait", ID: "outbox:9",
		Phase: phase.Implementing, Generation: stoppedRun, Entry: stoppedEntry + 1}
)

// stoppedPhaseHarness is a ready claim on the current plugin, so only the report naming a take's
// number finishes its task (TakesNoTask), whose issue stands in the phase its tasks are queued for
// (Deps.PhaseHolds), with every take the test's own (manualTakes).
func stoppedPhaseHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessOf(t, queuedClaim(), currentPlugin, func(d *Deps) {
		d.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return true, nil }
	})
	h.reach(StateReady)
	h.manualTakes()
	return h
}

// deliver queues request on the harness's claim.
func (h *harness) deliver(request RequestDeliver) {
	h.t.Helper()
	request.Claim = h.token
	h.must(request)
}

// retireStopped is the `retire_task` row of the move, run through the claim's machine, which must
// retire a task.
func (h *harness) retireStopped() {
	h.t.Helper()
	retired, err := h.m.RetireStoppedTask(h.ctx, phase.Implementing, stoppedRun, stoppedEntry)
	h.m.Wait()
	if err != nil || !retired {
		h.t.Fatalf("the retirement of the stopped phase's task = %t, %v; want it retired", retired, err)
	}
}

// takePhaseNow is the phase of the wait the pending take stands in (takePhase), read under the
// machine's lock.
func (h *harness) takePhaseNow() waitPhase {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.takePhase()
}

// wantStoppedTaskRetired holds that task number is gone, from memory, the store and the bucket,
// its value deleted once under the stop's cause, and that a second run of the row finds nothing of
// it to retire.
func (h *harness) wantStoppedTaskRetired(number uint64) {
	h.t.Helper()
	if p := h.claim().Pending; p != nil && p.Number == number {
		h.t.Fatalf("the stopped phase's task is still pending as %+v", p)
	}
	if d, held := h.store.delivery(h.token); held && d.Number == number {
		h.t.Fatalf("the store still holds %+v, want the stopped phase's task retired", d)
	}
	if value, held := h.values.value(h.token); held && value.Number == number {
		h.t.Fatalf("the bucket still holds %+v, want task %d's value deleted", value, number)
	}
	n := strconv.FormatUint(number, 10)
	if lines := h.logs.lines(valueDeletedLine, "task="+n, "cause="+valueDeletedStopped); len(lines) != 1 {
		h.t.Fatalf("logged %v, want task %d's value deleted once under the stop's cause", lines, number)
	}
	if retired, err := h.m.RetireStoppedTask(h.ctx, phase.Implementing, stoppedRun, stoppedEntry); err != nil || retired {
		h.t.Fatalf("the row's second run = %t, %v; want nothing of the stopped phase left to retire", retired, err)
	}
}

// wantReportFinishesNothing is the worker's report of the turn of task number, a take the
// retirement took from under it: no take holds that number, so the report finishes nothing.
func (h *harness) wantReportFinishesNothing(number uint64) {
	h.t.Helper()
	if err := h.reportTurn(number, nil); err != nil {
		h.t.Fatalf("the report of task %d's turn: %v", number, err)
	}
	n := strconv.FormatUint(number, 10)
	if lines := h.logs.lines("a turn's report names a task that no take of this claim holds; it finishes nothing", "task="+n); len(lines) != 1 {
		h.t.Fatalf("logged %v, want the report of task %d read as no take's", lines, number)
	}
	if finished := h.recordsNaming(finishedLine, number); len(finished) != 0 {
		h.t.Fatalf("task %d was finished %d times after its retirement, want never", number, len(finished))
	}
	if c := h.claim(); c.TaskFinished == number {
		h.t.Fatalf("the claim records task %d finished, want the retired task never finished", number)
	}
}

// wantReturnTaken is the return's own start queued and taken: the worker is answered that task and
// nothing of the stopped phase's in front of it.
func (h *harness) wantReturnTaken() {
	h.t.Helper()
	h.deliver(returnedTask)
	if got := h.mustTake(); got != returnedTask.Task {
		h.t.Fatalf("the take after the return was answered %q, want the return's own task alone", got)
	}
}

// The retirement takes only the stopped phase's own task, and leaves nothing of it for any process
// to take: not the agent a relaunch brings back during the wait, and not after the return, where
// the phase fence holds a task of that phase again. A task of another phase, another run or a later
// entry is not this phase's work and stays; the return's own start, on an entry of its own, is
// taken as any start is.
func TestRetiringAStoppedPhasesTaskLeavesNothingOfItToTake(t *testing.T) {
	h := stoppedPhaseHarness(t)
	h.deliver(stoppedTask)
	number := h.pending().Number

	for _, other := range []struct {
		name       string
		phase      phase.Phase
		generation uint64
		entry      int64
	}{
		{name: "another phase", phase: phase.Reviewing, generation: stoppedRun, entry: stoppedEntry},
		{name: "another run", phase: phase.Implementing, generation: stoppedRun + 1, entry: stoppedEntry},
		{name: "a later entry than the stop names", phase: phase.Implementing, generation: stoppedRun, entry: stoppedEntry - 1},
	} {
		retired, err := h.m.RetireStoppedTask(h.ctx, other.phase, other.generation, other.entry)
		if err != nil || retired || h.claim().Pending == nil {
			t.Fatalf("a retirement of %s = %t, %v, pending %+v; want the task left where it is", other.name, retired, err, h.claim().Pending)
		}
	}
	if _, held := h.values.value(h.token); !held {
		t.Fatal("a retirement of another phase's work deleted the task's value")
	}

	h.retireStopped()
	h.wantStoppedTaskRetired(number)

	// The process dies during the wait and the relaunched agent comes up: its worker reads its value
	// and finds nothing of the stopped phase to take.
	h.observe(runtime.Gone)
	h.relaunched()
	h.worker.manual = false
	h.workerAsks()
	h.worker.manual = true
	h.wantPrompts(0)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("the relaunched claim holds %+v, want nothing", p)
	}

	h.wantReturnTaken()
}

// The retirement holds in each state of the take, and each is retired at once, through the path a
// newer task retires a taken one by (retirePending): the claim keeps its state, its turn and its
// session, and the turn and the report that come after are no task's and finish nothing. A take
// whose report came and whose finish the store refused is finished by that report instead. A task
// held behind a turn of the agent's own goes, and that turn runs on. A task a death took back from
// its turn goes, and the record of that death, which the task would have carried in front of
// itself, is queued alone in its place and taken, as at each of takeable's drops; the return's own
// start then reaches the worker with nothing in front of it.
func TestRetiringAStoppedPhasesTaskInEachStateOfItsTake(t *testing.T) {
	t.Run("an untaken task", func(t *testing.T) {
		h := stoppedPhaseHarness(t)
		h.deliver(stoppedTask)
		number := h.pending().Number

		h.retireStopped()
		h.wantStoppedTaskRetired(number)
		h.wantState(StateReady)
		h.wantReturnTaken()
	})

	t.Run("an accepted take whose turn has not started, then its late turn and report", func(t *testing.T) {
		h := stoppedPhaseHarness(t)
		h.deliver(stoppedTask)
		h.mustTake()
		if p := h.pending(); !p.awaitsStart() {
			t.Fatalf("the claim holds %+v, want the take awaiting its turn: this case proves nothing", p)
		}
		number := h.pending().Number

		h.retireStopped()
		h.wantStoppedTaskRetired(number)
		h.wantState(StateReady)

		// The worker still holds the text and starts its turn late: that turn is no task's.
		h.must(StreamTurnStart{Claim: h.token})
		h.wantState(StateWorking)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the late turn's start gave the claim %+v, want no task: the take it began for was retired", p)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantReportFinishesNothing(number)
		h.wantReturnTaken()
	})

	t.Run("a take whose turn runs, then its report", func(t *testing.T) {
		h := stoppedPhaseHarness(t)
		h.deliver(stoppedTask)
		h.mustTake()
		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.startRecorded() {
			t.Fatalf("the claim holds %+v, want the take's turn running: this case proves nothing", p)
		}
		number := h.pending().Number

		h.retireStopped()
		h.wantStoppedTaskRetired(number)
		// The turn keeps running in its process, as no task's turn.
		h.wantState(StateWorking)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.wantReportFinishesNothing(number)
		h.wantReturnTaken()
	})

	t.Run("a take whose turn ended with its report owed, then the report", func(t *testing.T) {
		h := stoppedPhaseHarness(t)
		h.deliver(stoppedTask)
		h.mustTake()
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		if wait := h.takePhaseNow(); wait != waitReport {
			t.Fatalf("the take stands in wait phase %d, want its report owed: this case proves nothing", wait)
		}
		number := h.pending().Number

		h.retireStopped()
		h.wantStoppedTaskRetired(number)
		h.wantState(StateIdle)
		h.wantReportFinishesNothing(number)
		h.wantReturnTaken()
	})

	t.Run("a take whose report came and whose finish the store refused", func(t *testing.T) {
		h := stoppedPhaseHarness(t)
		h.deliver(stoppedTask)
		h.mustTake()
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		number := h.pending().Number
		h.store.fail("RetireDelivery", errBoom)
		if err := h.reportTurn(number, nil); err == nil {
			t.Fatal("the report whose finish the store refused answered no failure")
		}
		if wait := h.takePhaseNow(); wait != waitFinish {
			t.Fatalf("the take stands in wait phase %d, want its finish owed: this case proves nothing", wait)
		}
		h.store.fail("RetireDelivery", nil)

		h.retireStopped()
		// The report decided the task: it is finished by that report, its value deleted by the
		// finish, and nothing of it is taken back.
		h.wantOwedFinishMade(number)
		if lines := h.logs.lines(valueDeletedLine, "task="+strconv.FormatUint(number, 10)); len(lines) != 1 {
			t.Fatalf("logged %v, want task %d's value deleted once, by its finish", lines, number)
		}
		h.wantReturnTaken()
	})

	t.Run("an untaken task held behind a turn of the agent's own", func(t *testing.T) {
		h := stoppedPhaseHarness(t)
		h.must(StreamTurnStart{Claim: h.token})
		h.deliver(stoppedTask)
		h.wantRefused(HoldInTurn)
		number := h.pending().Number

		h.retireStopped()
		h.wantStoppedTaskRetired(number)
		// The claim keeps the turn it is in; its end offers nothing.
		h.wantState(StateWorking)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.wantPrompts(0)
		if _, held := h.values.value(h.token); held {
			t.Fatal("the bucket holds a value after the turn's end, want none: nothing of the stopped phase is owed")
		}
		h.wantReturnTaken()
	})

	t.Run("a task a death took back while the relaunch's record still tells", func(t *testing.T) {
		h := stoppedPhaseHarness(t)
		h.deliver(stoppedTask)
		h.mustTake()
		h.must(StreamTurnStart{Claim: h.token})
		h.reportActivity("subagent")
		h.observe(runtime.Gone)
		if p := h.pending(); !p.Interrupted || p.taken() || p.Phase != phase.Implementing {
			t.Fatalf("the relaunching claim holds %+v, want the stopped phase's task its death took back", p)
		}
		number := h.pending().Number
		// The relaunched process says it is ready: the claim holds the task its death took back, so
		// the ready queues no delivery of the record.
		h.relaunched()
		ended := h.claim().EndedWork
		if !ended.Tells() || !ended.TaskTurn {
			t.Fatalf("the death recorded %+v, want an untold record of the task's turn", ended)
		}

		h.retireStopped()
		h.wantStoppedTaskRetired(number)
		p := h.pending()
		if !p.Relaunch() || p.Task != RelaunchSentence(ended) || p.Phase != "" || p.taken() {
			t.Fatalf("after the retirement the claim holds %+v, want the daemon's own delivery of the record, untaken", p)
		}
		if value, held := h.values.value(h.token); !held || value.Number != p.Number {
			t.Fatalf("the bucket holds %+v (held %v), want the value of the record's delivery, task %d", value, held, p.Number)
		}

		// The worker takes the record's sentence alone, and its turn tells the record.
		if told := h.mustTake(); told != RelaunchSentence(ended) {
			t.Fatalf("the take after the retirement was answered %q, want the record's sentence alone", told)
		}
		h.must(StreamTurnStart{Claim: h.token})
		h.wantEndedTold(true, p.ID, "once the sentence's turn began")
		h.must(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the sentence's turn, want it finished", p)
		}

		// The return's own start reaches the worker with nothing in front of it: the record is told.
		h.wantReturnTaken()
	})
}
