package supervise

// A task is a value the worker takes (take.go). These tests pin the take itself: each hold a take
// meets refused by name, each drop deleting the value, what an accepted take applies once and
// never again, the number a task keeps through its take-backs and relaunches, and what a
// registering worker is told of its value.

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// adoptingIdentity is a role identity a take of a phase task adopts the working copy with.
func adoptingIdentity(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
	return runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}, nil
}

// Each of the seven holds a send used to stop at refuses a take by its own name, leaves the task
// untaken and its value in the bucket, and records the refusal at the value's revision, which is
// how the end of the hold knows to write the value again (offerPending).
func TestTakeIsRefusedByEachHoldByName(t *testing.T) {
	for _, tc := range []struct {
		hold string
		// arrange brings a harness to a claim holding a task whose take the hold refuses, with
		// the harness's worker taking nothing on its own.
		arrange func(t *testing.T) *harness
	}{
		{hold: HoldIssueInTurn, arrange: func(t *testing.T) *harness {
			h := newHarness(t)
			h.deps.Identity = adoptingIdentity
			h.deps.IssueInTurn = func(string, claim.Token) (claim.Token, bool) { return testerToken, true }
			h.restart()
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
			return h
		}},
		{hold: HoldSuspension, arrange: func(t *testing.T) *harness {
			h := newHarness(t)
			h.reach(StateWorking)
			if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); !suspensionHeld(err) {
				t.Fatalf("suspend mid-turn = %v, want the hold", err)
			}
			h.rt.FailSuspend(errBoom)
			_ = h.handle(StreamTurnEnd{Claim: h.token}) // the stop is refused: the hold stands over an idle claim
			h.manualTakes()
			h.must(RequestDeliver{Claim: h.token, Task: "the operator's task"})
			return h
		}},
		{hold: HoldUpdateRound, arrange: func(t *testing.T) *harness {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateReady)
			h.openUpdate(testRound, testReasons)
			h.must(StreamTurnStart{Claim: h.token})
			h.signalUpdate("c0ffee")
			h.must(StreamTurnEnd{Claim: h.token})
			h.manualTakes()
			h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})
			return h
		}},
		{hold: HoldLaunchWaits, arrange: func(t *testing.T) *harness {
			h := pacedHarness(t, wakeRate)
			h.reach(StateReady)
			h.observe(runtime.StaleAddress)
			h.wantWaitingWake(WakeLaunch)
			h.manualTakes()
			h.must(RequestDeliver{Claim: h.token, Task: "review the pull request", ID: "outbox:42", Row: 42, Generation: 7})
			return h
		}},
		{hold: HoldStalePlugin, arrange: func(t *testing.T) *harness {
			h := newHarness(t)
			h.reach(StateReady)
			h.m.deps.PluginContract = refreshContract
			h.m.deps.PluginRelaunchable = true
			h.manualTakes()
			h.must(RequestDeliver{Claim: h.token, Task: "review the pull request", ID: "outbox:42", Row: 42, Generation: 7})
			return h
		}},
		{hold: HoldRootHears, arrange: func(t *testing.T) *harness {
			h := newBareHarness(t)
			h.token = rootToken
			h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return false, nil }
			h.store.PutClaim(h.ctx, rootClaim())
			h.start(rootClaim())
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: rootToken, Task: "a person decided on the tree", Generation: 2, Row: 7})
			return h
		}},
		{hold: HoldProviderError, arrange: func(t *testing.T) *harness {
			h := workingClaim(t)
			h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
			h.manualTakes()
			return h
		}},
	} {
		t.Run(tc.hold, func(t *testing.T) {
			h := tc.arrange(t)

			h.wantRefused(tc.hold)

			if p := h.pending(); p.taken() {
				t.Fatalf("pending %+v after the refusal, want the task untaken", p)
			}
			if got := h.claim().TaskTakeState; got != TakeRefused {
				t.Fatalf("the claim records the take %q, want it refused at the value's revision", got)
			}
			if _, held := h.values.value(h.token); !held {
				t.Fatal("the refusal deleted the value; a refused take leaves the task owed")
			}
		})
	}
}

// A take the worker sends while its claim is not ready or idle — a turn of its own running, its
// agent not yet ready — is refused by that, in-turn or not-ready, and records the refusal at the
// revision as every hold does.
func TestTakeIsRefusedInTurnAndNotReady(t *testing.T) {
	t.Run(HoldInTurn, func(t *testing.T) {
		h := newHarness(t)
		h.manualTakes()
		h.reach(StateReady)
		h.must(StreamTurnStart{Claim: h.token})
		h.must(RequestDeliver{Claim: h.token, Task: "after this turn"})
		h.wantRefused(HoldInTurn)
	})
	t.Run(HoldNotReady, func(t *testing.T) {
		h := newHarness(t)
		h.manualTakes()
		h.reach(StateRegistered)
		h.must(RequestDeliver{Claim: h.token, Task: "the task"})
		h.wantRefused(HoldNotReady)
	})
}

// A take of a task the record no longer owes is answered dropped, the task retired and its value
// deleted: a task whose issue has left the phase it was queued for, and a root architect's task
// that narrows to nothing (the third drop, the ask of a round the claim left, is
// TestTakeOfAnAskOfARoundTheClaimLeftIsDroppedAndItsValueDeleted).
func TestTakeDropsATaskTheRecordNoLongerOwesAndDeletesItsValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T) *harness
	}{
		{name: "the issue has left the task's phase", arrange: func(t *testing.T) *harness {
			h := newBareHarness(t)
			holds := true
			h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return holds, nil }
			h.store.PutClaim(h.ctx, queuedClaim())
			h.start(queuedClaim())
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: "test the branch", Phase: phase.Testing, Generation: 7})
			holds = false
			return h
		}},
		{name: "a root architect's task that names nothing now", arrange: func(t *testing.T) *harness {
			h := newBareHarness(t)
			h.token = rootToken
			h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return true, nil }
			h.deps.RootTask = func(context.Context, Claim, Delivery) (string, []byte, error) { return "", nil, nil }
			h.store.PutClaim(h.ctx, rootClaim())
			h.start(rootClaim())
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: rootToken, Task: "a person decided on the tree", Generation: 2, Row: 7})
			return h
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.arrange(t)
			if _, held := h.values.value(h.token); !held {
				t.Fatal("the queue wrote no value; this test needs one to delete")
			}

			if answer := h.take(); answer.Outcome != TakeOutcomeDropped {
				t.Fatalf("take = %+v, want it dropped", answer)
			}

			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending %+v after the drop, want none", p)
			}
			if d, held := h.store.delivery(h.token); held {
				t.Fatalf("the store still holds %+v, want the dropped task retired", d)
			}
			if _, held := h.values.value(h.token); held {
				t.Fatal("the bucket still holds the dropped task's value")
			}
			if deleted := h.values.deleted(); len(deleted) != 1 || deleted[0] != h.token {
				t.Fatalf("values deleted = %v, want the claim's one", deleted)
			}
		})
	}
}

// A root architect's held task names decisions that can be owed to another tree by the time its
// root hears again: the take is answered with what it still names (Deps.RootTask), and the
// delivery is written narrowed, so a take sent again is answered the same narrowed text.
func TestTakeOfARootTaskIsNarrowedToWhatItStillNames(t *testing.T) {
	h := newBareHarness(t)
	h.token = rootToken
	h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return true, nil }
	h.deps.RootTask = func(context.Context, Claim, Delivery) (string, []byte, error) {
		return "a person decided on this tree only", []byte(`{"decisions":[]}`), nil
	}
	h.store.PutClaim(h.ctx, rootClaim())
	h.start(rootClaim())
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: rootToken, Task: "a person decided on two trees", Generation: 2, Row: 7,
		Kept: []byte(keptDecision)})

	if text := h.mustTake(); text != "a person decided on this tree only" {
		t.Fatalf("the take was answered %q, want the narrowed task", text)
	}
	p := h.pending()
	if p.Task != "a person decided on this tree only" || !bytes.Equal(p.Kept, []byte(`{"decisions":[]}`)) {
		t.Fatalf("pending %+v, want the delivery narrowed to what it still names", p)
	}
	if stored, _ := h.store.delivery(rootToken); stored.Task != p.Task || !bytes.Equal(stored.Kept, p.Kept) {
		t.Fatalf("stored delivery %+v, want the narrowing written", stored)
	}
	if again := h.mustTake(); again != "a person decided on this tree only" {
		t.Fatalf("the take sent again was answered %q, want the same narrowed text", again)
	}
}

// An accepted take is the write that tells the decisions the task names: the delivery is written
// with the take recorded on it (ConfirmedAt) and still naming what the row kept, in the claim's
// and the delivery's one write, before the take's turn starts — so a person's decision reaches
// the architect at its take rather than at a turn that may never start.
func TestTakeAcceptedWritesTheKeptDecisionsWithTheTake(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "a person decided: dispatch://LEGION-208/ask/ask-1",
		ID: "outbox:40", Row: 40, Phase: phase.Implementing, Generation: 7, Kept: []byte(keptDecision)})
	if stored, _ := h.store.delivery(testToken); stored.taken() {
		t.Fatalf("the stored delivery %+v is taken before any take; the queue tells nothing", stored)
	}

	h.mustTake()

	stored, ok := h.store.delivery(testToken)
	if !ok || !stored.awaitsTurn() || !bytes.Equal(stored.Kept, []byte(keptDecision)) {
		t.Fatalf("the stored delivery after the take = %+v (held %t), want it taken, its turn to come, and still naming %q",
			stored, ok, keptDecision)
	}
	if got := h.store.load(testToken).TaskTaken; got != stored.Number {
		t.Fatalf("the stored claim records task %d taken, want %d: the take's write is the claim's and the delivery's", got, stored.Number)
	}
}

// The roles of an issue share one working copy, so a role whose take was accepted is in a turn
// for every other role of the issue until that take's turn ends (Claim.InTurn): another role's
// take of a phase task is refused issue-in-turn between the take and its turn's start, and through
// the turn, and is accepted once the turn has ended.
func TestTakeAcceptedHoldsTheIssueInTurnForAnotherRoleUntilItsTurnEnds(t *testing.T) {
	implementer := newHarness(t)
	implementer.deps.Identity = adoptingIdentity
	implementer.restart()
	implementer.manualTakes()
	implementer.reach(StateReady)

	tester := newHarnessOf(t, testerClaim())
	tester.deps.Identity = adoptingIdentity
	tester.deps.IssueInTurn = func(issue string, except claim.Token) (claim.Token, bool) {
		if view := implementer.m.View(); view.Issue == issue && view.Token != except && view.InTurn() {
			return view.Token, true
		}
		return "", false
	}
	tester.restart()
	tester.manualTakes()
	tester.reach(StateReady)
	tester.must(RequestDeliver{Claim: testerToken, Task: "test the branch", Phase: phase.Testing, Generation: 7})

	implementer.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
	implementer.mustTake()

	tester.wantRefused(HoldIssueInTurn)
	implementer.must(StreamTurnStart{Claim: testToken})
	tester.must(IssueTurnEnded{Claim: testerToken})
	tester.wantRefused(HoldIssueInTurn)
	tester.wantCalls("AdoptWorkingCopy", 0)

	implementer.must(StreamTurnEnd{Claim: testToken})
	tester.must(IssueTurnEnded{Claim: testerToken})

	if text := tester.mustTake(); text != "test the branch" {
		t.Fatalf("the tester's take after the implementer's turn ended = %q, want its task", text)
	}
	tester.wantCalls("AdoptWorkingCopy", 1)
}

// A take sent again while the first is still being answered — its adoption of the working copy
// still running — waits for that answer and is answered from it: one adoption, one acceptance.
func TestTakeSentTwiceWhileTheFirstIsAnsweredWaitsForItsAnswer(t *testing.T) {
	h := newHarness(t)
	gate := newGatedAdopt(h.rt)
	h.deps.Runtime = gate
	h.deps.Identity = adoptingIdentity
	h.restart()
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
	value := h.seen()
	request := RequestTake{Claim: testToken, Generation: h.generation(), Session: session,
		Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}

	var wg sync.WaitGroup
	answers := make([]TakeAnswer, 2)
	errs := make([]error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		answers[0], errs[0] = h.m.Take(h.ctx, request)
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first take's adoption never started")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		answers[1], errs[1] = h.m.Take(h.ctx, request)
	}()
	// The second take has the time it needs to reach the machine while the first's adoption is
	// held at the gate; a take that did not wait would start an adoption of its own here.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-gate.entered:
		t.Fatal("the second take started an adoption of its own while the first was being answered")
	default:
	}
	gate.release <- struct{}{}
	wg.Wait()
	h.m.Wait()

	for i := range answers {
		if errs[i] != nil || answers[i].Outcome != TakeOutcomeAccepted || answers[i].Text != "implement the plan" {
			t.Fatalf("take %d = %+v, %v, want it accepted with the task's text", i+1, answers[i], errs[i])
		}
	}
	h.wantCalls("AdoptWorkingCopy", 1)
}

// A take of a number already accepted whose turn has not started is a take whose answer was lost:
// it is answered the same text and records nothing — no write, no second telling, no second
// adoption.
func TestTakeRepeatedIsAnsweredTheSameAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	confirmed := 0
	h.deps.TaskConfirmed = func(Claim) { confirmed++ }
	h.deps.Identity = adoptingIdentity
	h.restart()
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
	first := h.mustTake()
	puts, log := len(h.store.history()), h.store.deliveryLog()

	if again := h.mustTake(); again != first {
		t.Fatalf("the take sent again was answered %q, want %q", again, first)
	}
	if got := len(h.store.history()); got != puts {
		t.Errorf("the repeated take wrote the claim %d times, want none", got-puts)
	}
	if got := h.store.deliveryLog(); len(got) != len(log) {
		t.Errorf("the repeated take wrote the delivery: %v", got[len(log):])
	}
	if confirmed != 1 {
		t.Errorf("the workflow was told %d times, want once", confirmed)
	}
	h.wantCalls("AdoptWorkingCopy", 1)
}

// A take of a number the record does not owe the claim — a later number than any written, an
// earlier task's, or any number once nothing is pending — is refused not-owed and records
// nothing: no seen revision, no take state, no write.
func TestTaskNumberOfAnotherTaskIsRefusedAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	value := h.seen()
	before, puts := h.claim(), len(h.store.history())

	for _, number := range []uint64{value.Number + 1, value.Number + 41} {
		other := value
		other.Number = number
		answer, err := h.takeValue(other)
		if err != nil || answer.Outcome != TakeOutcomeRefused || answer.Hold != HoldNotOwed {
			t.Fatalf("the take of task %d = %+v, %v, want it refused not-owed", number, answer, err)
		}
	}
	after := h.claim()
	if after.TaskTakeState != before.TaskTakeState || after.TaskSeenRevision != before.TaskSeenRevision || after.TaskTaken != 0 {
		t.Fatalf("the claim moved from %+v to %+v, want nothing of the refused takes recorded", before, after)
	}
	if got := len(h.store.history()); got != puts {
		t.Fatalf("the refused takes wrote the claim %d times, want none", got-puts)
	}
	if p := h.pending(); p.taken() {
		t.Fatalf("pending %+v, want the owed task untaken", p)
	}

	// The task finished, a take of its number is not owed either.
	h.mustTake()
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	if answer, err := h.takeValue(value); err != nil || answer.Outcome != TakeOutcomeRefused || answer.Hold != HoldNotOwed {
		t.Fatalf("the take of the finished task = %+v, %v, want it refused not-owed", answer, err)
	}
}

// A task a provider error took back keeps its number and its id: the ladder's wait refuses its
// take, and when the wait is over the same number is written again and taken again.
func TestTakeBackByAProviderErrorKeepsTheNumber(t *testing.T) {
	h := workingClaim(t)
	first := h.wantPrompts(1)[0]

	h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})

	if p := h.pending(); p.taken() || p.Number != first.Number || p.ID != first.DeliveryID {
		t.Fatalf("pending %+v after the provider error, want task %d taken back under its id", p, first.Number)
	}
	if written := h.claim().TaskWritten; written != first.Number {
		t.Fatalf("the claim has written %d tasks, want %d: a take-back writes no new number", written, first.Number)
	}
	h.advance(promptRetryFirst)
	again := h.wantPrompts(2)[1]
	if again.Number != first.Number || again.DeliveryID != first.DeliveryID {
		t.Fatalf("the take after the wait = %+v, want task %d again", again, first.Number)
	}
}

// The note a provider error leaves is in front of the text of the next accepted take of its task,
// and only of that take's: the turn that follows ends with no error, and the next task is the
// task alone.
func TestTakeAfterAProviderErrorCarriesItsNote(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
	note := h.pending().ProviderErrorNote
	if note == "" {
		t.Fatal("the provider error left no note on the task")
	}

	h.advance(promptRetryFirst)

	text := h.wantPrompts(2)[1].Message
	if !strings.HasPrefix(text, note) || !strings.HasSuffix(text, providerErrorTaskText) {
		t.Fatalf("the take after the wait was answered %q, want the note %q in front of the task", text, note)
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
	if c := h.claim(); c.TaskWritten != 1 || c.TaskWrittenEpoch != written[0].Epoch || c.TaskWrittenRevision != written[0].Revision {
		t.Fatalf("the claim records written %d at %q/%d, want 1 at the write's %q/%d",
			c.TaskWritten, c.TaskWrittenEpoch, c.TaskWrittenRevision, written[0].Epoch, written[0].Revision)
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

// A take from a claim a restart restored working, holding an owed task no take of it accepted,
// whose turn this daemon has heard nothing of, is the worker's word that its host is idle (R3):
// it is accepted, and the turn that follows is the take's own.
func TestTakeFromAClaimRestoredWorkingIsTheWorkersWordThatItIsIdle(t *testing.T) {
	restored := fixture(StateWorking)
	restored.Pending.ConfirmedAt, restored.TaskTaken = time.Time{}, 0
	h := newBareHarness(t)
	h.restore(restored)
	h.manualTakes()

	if text := h.mustTake(); text != "the task" {
		t.Fatalf("the take of the restored working claim = %q, want the task", text)
	}
	if p := h.pending(); !p.awaitsTurn() {
		t.Fatalf("pending %+v, want the take accepted and its turn to come", p)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	if p := h.pending(); !p.turnStarted() {
		t.Fatalf("pending %+v, want the turn that followed the take's own", p)
	}
}
