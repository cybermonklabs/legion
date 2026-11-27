package supervise

// The task's value (taskvalue.go): written once under its number when the task is queued, written
// again at the claim's next chance after a write the bucket refused or a take that failed on the
// store, and deleted when the record no longer owes the task; and what a registering or relaunched
// worker is told of it, which it takes once.

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A write of the claim's value the bucket refuses fails nothing of the decision that made it: the
// task is queued all the same, and the value is written at the claim's next chance, whatever the
// worker last asked (valueUnwritten) — here the agent's ready.
func TestTakeValueTheBucketRefusedIsWrittenAtTheNextChance(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.values.fail("Write", errBoom)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	if _, held := h.values.value(testToken); held {
		t.Fatal("the bucket holds a value it refused to write")
	}
	if lines := h.logs.lines("supervise: the task value could not be written"); len(lines) != 1 {
		t.Fatalf("logged %q, want the refused write named once", lines)
	}
	number := h.pending().Number

	h.values.fail("Write", nil)
	h.ready()

	value, held := h.values.value(testToken)
	if !held || value.Number != number {
		t.Fatalf("the bucket holds %+v (held %v) after the next chance, want task %d's value", value, held, number)
	}
	if c := h.claim(); c.TaskWrittenRevision.Revision != value.Revision {
		t.Fatalf("the claim records the write at revision %d, want the bucket's %d", c.TaskWrittenRevision.Revision, value.Revision)
	}
	if text := h.wantPrompts(1)[0].Message; text != "the task" {
		t.Fatalf("the worker took %q, want the task", text)
	}
}

// A relaunched worker takes the task its dead process was running once, under the same number:
// its registration hands it the value and it asks once per revision; nothing writes a second
// number, and a later ready or sweep gives it nothing more to take.
func TestRelaunchTakesTheSameNumberOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	first := h.wantPrompts(1)[0]

	h.observe(runtime.Gone)
	h.relaunched()

	again := h.wantPrompts(2)[1]
	if again.Number != first.Number {
		t.Fatalf("the relaunched worker took task %d, want %d", again.Number, first.Number)
	}
	h.ready()
	h.observe(runtime.Alive)
	h.wantPrompts(2)
	if written := h.claim().TaskWritten; written != first.Number {
		t.Fatalf("the claim has written %d tasks, want %d", written, first.Number)
	}
}

// A released pod's role is woken by a task of its own (origin wake), which resumes the released
// claim; the process the resume starts takes that task once, and nothing gives it again.
func TestRelaunchTakesAWakeTaskOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	// The session reports at the turn's end that nothing of its own runs, as Oh My Pi's does, so the
	// release ends no work and the wake's task goes as it was given (recordEnd).
	h.reportActivity()
	h.must(RequestSuspend{Claim: testToken, By: SuspendedByReleased})
	h.wantState(StateSuspended)
	base := len(h.prompts())

	h.must(RequestDeliver{Claim: testToken, Task: "a notice woke this role", Origin: OriginWake})
	if p := h.pending(); p.Origin != OriginWake {
		t.Fatalf("pending %+v, want the wake's task", p)
	}
	h.wantState(StateLaunching)
	h.relaunched()

	taken := h.wantPrompts(base + 1)[base]
	if taken.Message != "a notice woke this role" {
		t.Fatalf("the relaunched worker took %+v, want the wake's task", taken)
	}
	h.ready()
	h.observe(runtime.Alive)
	h.wantPrompts(base + 1)
}

// What a registering worker is told of its task (CurrentTask): the value the bucket holds for the
// claim, none once its task is finished, and the number of the last task the claim finished.
func TestRegisterAnswersTheCurrentValueAndTheFinishedNumber(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	if current, err := h.m.CurrentTask(h.ctx); err != nil || current.Value != nil || current.Finished != 0 {
		t.Fatalf("CurrentTask before any task = %+v, %v, want no value and nothing finished", current, err)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	number := h.pending().Number
	current, err := h.m.CurrentTask(h.ctx)
	if err != nil || current.Value == nil || current.Value.Number != number || current.Finished != 0 {
		t.Fatalf("CurrentTask with task %d pending = %+v, %v, want its value and nothing finished", number, current, err)
	}
	if want, _ := h.values.value(testToken); *current.Value != want {
		t.Fatalf("CurrentTask's value = %+v, want the bucket's %+v", *current.Value, want)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	if current, err := h.m.CurrentTask(h.ctx); err != nil || current.Value != nil || current.Finished != number {
		t.Fatalf("CurrentTask after task %d finished = %+v, %v, want no value and %d finished", number, current, err, number)
	}
}

// A task's value is written once when it is queued, under its number, and recorded on the claim
// at the epoch and revision the write answered; it is deleted when the record drops the task —
// here the daemon's suspension that ends the task's phase.
func TestTaskValueIsWrittenAtTheQueueAndDeletedWhenTheRecordDropsTheTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})

	written := h.values.written()
	if len(written) != 1 || written[0].Number != 1 {
		t.Fatalf("values written = %+v, want task 1's one value", written)
	}
	if c := h.claim(); c.TaskWritten != 1 || c.TaskWrittenRevision != written[0].revision() {
		t.Fatalf("the claim records written %d at %q/%d, want 1 at the write's %q/%d",
			c.TaskWritten, c.TaskWrittenRevision.Epoch, c.TaskWrittenRevision.Revision, written[0].Epoch, written[0].Revision)
	}
	if stored := h.store.load(testToken); stored.TaskWritten != 1 || stored.Pending == nil || stored.Pending.Number != 1 {
		t.Fatalf("the stored claim records written %d with pending %+v, want task 1 durable with its number", stored.TaskWritten, stored.Pending)
	}

	h.must(RequestSuspend{Claim: testToken, Reason: "the issue left the workflow", By: SuspendedByDaemon})

	if deleted := h.values.deleted(); len(deleted) != 1 || deleted[0] != testToken {
		t.Fatalf("values deleted = %v, want the claim's value deleted with the dropped task", deleted)
	}
	if _, held := h.values.value(testToken); held {
		t.Fatal("the bucket still holds the dropped task's value")
	}
}

// decisionFailsHarness is a ready claim queued a phase task whose take's decision fails: the
// record's phase read fails (errBoom) until the case clears the answer it returns, and then
// answers that the phase holds.
func decisionFailsHarness(t *testing.T) (*harness, *bool) {
	t.Helper()
	failing := true
	h := waitsHarness(t, func(d *Deps) {
		d.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) {
			if failing {
				return false, errBoom
			}
			return true, nil
		}
	})
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
	return h, &failing
}

// A take whose decision fails on a read of the store before it is answered — the connection
// dropped under the read of the issue's phase — is answered with no outcome, which the take route
// answers 500 CLAIM_WRITE_FAILED. The plugin sends no take again after an answer that carries a
// code, and asks again only at a newer revision of the value (pi-shared's task-take.ts,
// unanswered), so the record, which still owes the task with no hold standing, writes the value
// again under its number (R2: whenever the record owes a claim a task the claim is free to take);
// otherwise the task never reaches the worker and no bound charges anyone for it.
func TestTakeWhoseDecisionFailedIsWrittenAgainForTheWorkerToAsk(t *testing.T) {
	h, failing := decisionFailsHarness(t)
	asked := h.seen()
	if answer, err := h.takeValue(asked); answer.Outcome != "" || err == nil {
		t.Fatalf("take while the phase read fails = %+v, %v; want no answer and the read's failure", answer, err)
	}
	*failing = false

	h.advance(takeBound)
	h.observe(runtime.Alive)

	value, held := h.values.value(h.token)
	if !held || value.Number != asked.Number || value == asked {
		t.Fatalf("a first bound after the failed take the bucket holds %+v (held %t), want task %d written again at a newer revision than %d, the plugin's only signal to ask again",
			value, held, asked.Number, asked.Revision)
	}
	if answer, err := h.takeValue(value); answer.Outcome != TakeOutcomeAccepted {
		t.Fatalf("the take at the newer revision = %+v, %v; want it accepted", answer, err)
	}
	h.wantCharges()
}

// A delete of the claim's value the bucket refuses is made again at the claim's next chance
// while the record owes the claim no task, the sweep's observation among them, so a finished
// task's value does not outlive it until the claim's next task or the daemon's next boot (R1).
// A task queued meanwhile has its own value in the bucket, which that chance must not delete.
//
// The negative controls are the delete made again nowhere (offerPending with no deleteAgain):
// the finished task's value stands after the next chance; and the delete made again whatever
// the record owes (deleteAgain before offerPending's check of the pending task): the new task's
// value is deleted.
func TestTaskValueDeleteTheBucketRefusedIsMadeAgainAtTheNextChance(t *testing.T) {
	t.Run("the delete is made again at the next chance", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		number := h.pending().Number
		h.values.fail("Delete", errBoom)
		h.must(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the turn was reported, want task %d finished", p, number)
		}
		if _, held := h.values.value(h.token); !held {
			t.Fatal("the bucket holds no value after a delete it refused")
		}

		h.values.fail("Delete", nil)
		h.observe(runtime.Alive)

		if value, held := h.values.value(h.token); held {
			t.Fatalf("the bucket still holds %+v after the next chance, want the finished task's value deleted", value)
		}
		deleted := h.records.of(deletedLine)
		if len(deleted) != 1 {
			t.Fatalf("%q was logged %d times, want once", deletedLine, len(deleted))
		}
		wantAttr(t, deleted[0], "cause", valueDeletedFinished)
	})
	t.Run("a task written meanwhile is not deleted", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		h.values.fail("Delete", errBoom)
		h.must(StreamTurnEnd{Claim: h.token})
		h.values.fail("Delete", nil)
		h.must(RequestDeliver{Claim: h.token, Task: "the next task"})
		next := h.pending().Number

		h.observe(runtime.Alive)

		if value, held := h.values.value(h.token); !held || value.Number != next {
			t.Fatalf("the bucket holds %+v (held %v), want task %d's value", value, held, next)
		}
		if deleted := h.values.deleted(); len(deleted) != 0 {
			t.Fatalf("values deleted %v, want none: the record owes task %d", deleted, next)
		}
	})
}

// A take whose accept the store refused is refused at its revision and its value written again
// once the task is free to take (undecided), but not while the claim still owes the write the
// store did not take: the store is away, a take at the new revision would fail in turn, and a
// task whose take reads nothing of the store — one of no phase — would go round a write, a take
// and a failed answer at every sweep for as long as the store is away. The value is written once
// the owed write lands, and the take at that revision is accepted.
//
// The negative control is the rewrite made whatever the claim owes (offerPending with no check
// of the owed write): the first sweep of the outage writes the value again.
func TestTakeFailedOnTheStoreIsWrittenAgainOnlyOnceTheOwedWriteLands(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "the operator's task"})
	asked := h.seen()
	h.store.fail("PutClaim", errBoom)
	h.store.fail("PutDelivery", errBoom)
	if answer, err := h.takeValue(asked); answer.Outcome != "" || err == nil {
		t.Fatalf("the take while the store refuses writes = %+v, %v; want no answer and the write's failure", answer, err)
	}
	written := len(h.values.written())

	for sweep := 1; sweep <= 3; sweep++ {
		_ = h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}})
		if got := h.values.written(); len(got) != written {
			t.Fatalf("sweep %d of the outage wrote the value again: %+v", sweep, got[written:])
		}
	}

	h.store.fail("PutClaim", nil)
	h.store.fail("PutDelivery", nil)
	h.observe(runtime.Alive)

	rewritten := h.values.written()
	if len(rewritten) != written+1 {
		t.Fatalf("the values written once the store answered are %+v, want one more than the %d before", rewritten, written)
	}
	value := rewritten[len(rewritten)-1]
	if value.Number != asked.Number || value.Revision <= asked.Revision {
		t.Fatalf("the store's return wrote %+v, want task %d again at a revision past %d", value, asked.Number, asked.Revision)
	}
	lines := h.records.of(writtenLine)
	wantAttr(t, lines[len(lines)-1], "cause", TaskValueTakeFailed)
	if answer, err := h.takeValue(value); answer.Outcome != TakeOutcomeAccepted {
		t.Fatalf("the take at the newer revision = %+v, %v; want it accepted", answer, err)
	}
}
