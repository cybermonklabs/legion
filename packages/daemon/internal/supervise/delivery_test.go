package supervise

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A task queued before the agent is ready is a value the worker takes once its agent is ready:
// the queue writes it under the claim's next number, nothing is taken while the claim boots, the
// take at the ready is accepted with the task's text, the take's turn is the task's, and the
// report of that turn finishes it. Nothing is taken twice, and nothing is charged.
func TestATaskQueuedBeforeReadyIsDeliveredOnceAtReady(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	queued := h.pending()
	if stored, ok := h.store.delivery(testToken); !ok || stored.ID != queued.ID || stored.Task != "implement the plan" || stored.Number != 1 {
		t.Fatalf("stored delivery %+v, want the queued task under number 1", stored)
	}
	if value, ok := h.values.value(testToken); !ok || value.Number != 1 {
		t.Fatalf("the bucket holds %+v (held %v), want the queued task's value", value, ok)
	}
	h.connect()
	h.register()
	h.wantPrompts(0)

	h.ready()

	sent := h.wantPrompts(1)[0]
	if sent.DeliveryID != queued.ID || sent.Number != 1 || sent.Message != "implement the plan" {
		t.Errorf("took %+v, want the queued task under its number", sent)
	}
	if !h.pending().awaitsTurn() {
		t.Error("the accepted take is not recorded on the delivery")
	}
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	if !h.pending().turnStarted() {
		t.Error("the turn did not start the take's turn")
	}
	if stored, _ := h.store.delivery(testToken); !stored.turnStarted() {
		t.Error("the take's turn start was not persisted")
	}

	h.must(StreamTurnEnd{Claim: testToken})

	h.wantState(StateIdle)
	if p := h.claim().Pending; p != nil {
		t.Errorf("pending %+v after its turn was reported, want none", p)
	}
	if _, ok := h.store.delivery(testToken); ok {
		t.Error("the store still holds the delivery its turn finished")
	}
	if stored := h.store.load(testToken); stored.TaskFinished != 1 || stored.TaskFinishedSession != session {
		t.Errorf("stored claim finished task %d in session %q, want 1 in %q", stored.TaskFinished, stored.TaskFinishedSession, session)
	}
	if _, ok := h.values.value(testToken); ok {
		t.Error("the bucket still holds the finished task's value")
	}
	h.advance(10 * testRPC)
	h.wantPrompts(1)
	h.wantBudgets(Budgets{})
}

// The phase entry a finished task's retire leaves on the claim, durably, beside the run: the
// worker goes on working that entry in whatever turn comes next — one an Envoy notice starts,
// with no take behind it — and the completion it reports there is still that entry's. A later
// task that carries no entry, a resume or a production check's look, moves the run on and leaves
// the entry alone, so a resumed worker's in-flight completion is never refused.
func TestARetiredTasksEntryStaysOnTheClaimAndAResumeLeavesIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "test it", Generation: 7, Entry: 1})
	if p := h.pending(); p.Entry != 1 {
		t.Fatalf("the queued delivery carries entry %d, want the start's 1", p.Entry)
	}
	if stored, ok := h.store.delivery(testToken); !ok || stored.Entry != 1 {
		t.Fatalf("the stored delivery is %+v, want it to carry entry 1: a restart reads the entry back", stored)
	}
	h.must(StreamTurnStart{Claim: testToken})
	if got := h.claim().ServingEntry(); got != 1 {
		t.Fatalf("during the task's turn the claim serves entry %d, want 1", got)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if stored := h.store.load(testToken); stored.ServedEntry != 1 || stored.ServingGeneration != 7 {
		t.Fatalf("after the turn the stored claim serves entry %d of run %d, want 1 of 7",
			stored.ServedEntry, stored.ServingGeneration)
	}
	if got := h.claim().ServingEntry(); got != 1 {
		t.Fatalf("after the turn the claim serves entry %d, want 1: its next turn's completion is still this entry's", got)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "resume the phase", Generation: 8})
	h.must(StreamTurnStart{Claim: testToken})
	if got := h.claim().ServingEntry(); got != 1 {
		t.Fatalf("during the resume's turn the claim serves entry %d, want the 1 it was given: a resume hands out none", got)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if stored := h.store.load(testToken); stored.ServedEntry != 1 || stored.ServingGeneration != 8 {
		t.Fatalf("after the resume the stored claim serves entry %d of run %d, want 1 of 8",
			stored.ServedEntry, stored.ServingGeneration)
	}
}

// The production check look a finished task's retire leaves on the claim, durably, beside the
// run and the entry: the checking agent goes on working that look in whatever turn comes
// next, and the completion it reports there is still a report on that look (api/handoff.go). A
// task of a phase takes it back, since a worker in the workflow again reports its phase.
func TestARetiredLooksTaskLeavesItsLookOnTheClaimAndAPhaseTaskTakesItBack(t *testing.T) {
	opened := time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)
	look := CheckLook{Look: 2, OpenedAt: opened}
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "Production check of LEGION-209.", Generation: 7, CheckLook: look})
	if p := h.pending(); p.CheckLook != look {
		t.Fatalf("the queued delivery carries look %+v, want the start's %+v", p.CheckLook, look)
	}
	if stored, ok := h.store.delivery(testToken); !ok || stored.CheckLook != look {
		t.Fatalf("the stored delivery is %+v, want it to carry %+v: a restart reads the look back", stored, look)
	}
	h.must(StreamTurnStart{Claim: testToken})
	if got := h.claim().ServingCheck(); got != look {
		t.Fatalf("during the look's turn the claim serves %+v, want %+v", got, look)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if stored := h.store.load(testToken); stored.ServingCheckLook != look {
		t.Fatalf("after the turn the stored claim serves look %+v, want %+v", stored.ServingCheckLook, look)
	}
	if got := h.claim().ServingCheck(); got != look {
		t.Fatalf("after the turn the claim serves look %+v, want %+v: its next turn's report is still this look's", got, look)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "Continue Workflow. Issue: LEGION-209. Phase: testing.", Generation: 8, Phase: phase.Testing, Entry: 2})
	h.must(StreamTurnStart{Claim: testToken})
	if got := h.claim().ServingCheck(); got != (CheckLook{}) {
		t.Fatalf("during the phase task's turn the claim serves look %+v, want none: its completion is the phase's", got)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if stored := h.store.load(testToken); stored.ServingCheckLook != (CheckLook{}) {
		t.Fatalf("after the phase task's turn the stored claim serves look %+v, want none", stored.ServingCheckLook)
	}
}

func TestADeliveryToAnIdleClaimIsSentAtOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.must(RequestDeliver{Claim: testToken, Task: "the next task"})
	if sent := h.wantPrompts(2)[1]; sent.Message != "the next task" || sent.Number != 2 {
		t.Errorf("took %+v, want the next task under number 2", sent)
	}
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	if !h.pending().turnStarted() {
		t.Error("the idle claim's turn did not start its take's turn")
	}
}

func TestATaskGivenBeforeReadyWaitsForReady(t *testing.T) {
	for _, state := range []ClaimState{StateQueued, StateLaunching, StateShimConnected, StateRegistered} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			h.wantPrompts(0)
			h.wantState(state)
			h.reach(StateReady)
			if sent := h.wantPrompts(1)[0]; sent.Message != "the task" || sent.DeliveryID != h.pending().ID {
				t.Errorf("took %+v at ready, want the task queued before it", sent)
			}
		})
	}
}

// A second delivery is refused for an untaken delivery the claim already holds, whatever its
// state: that task has reached nobody, so accepting over it would lose a task. The refusal is
// ErrDeliveryPending, and it says so rather than naming the state. A task whose take was accepted
// and whose turn has not started is in the worker's hands, and refuses a newer one the same way.
func TestASecondDeliveryWhileOneIsPendingIsRefused(t *testing.T) {
	for _, taken := range []bool{false, true} {
		name := "untaken"
		if taken {
			name = "taken, its turn still to come"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if !taken {
				h.manualTakes()
			}
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "row-1"})
			if got := h.pending().awaitsTurn(); got != taken {
				t.Fatalf("the pending delivery at ready is %+v, want the state this case is about", h.pending())
			}

			err := h.handle(RequestDeliver{Claim: testToken, Task: "another", ID: "row-2"})

			var refused *RefusedError
			if !errors.As(err, &refused) || !errors.Is(err, ErrDeliveryPending) || err.Error() != "deliver refused: a delivery is already pending" {
				t.Fatalf("a second delivery returned %v, want the pending-delivery refusal", err)
			}
			if h.pending().Task != "implement the plan" {
				t.Errorf("pending %+v, want the first task kept", h.pending())
			}
		})
	}
}

// A delivery whose take's turn has started holds nothing back: the worker read it, and what the
// delivery records is the turn in flight. A newer task takes its place — the phase's own next
// start, which the daemon queues while that turn runs — and it is taken at the next point a take
// may be accepted, not into the running turn. Refusing it instead left three supervise rows of
// the live daemon answered "a delivery is already pending" with nothing ever sent and the phase
// waiting on a worker that had finished (dispatch://INFRA-1352, INFRA-1443, INFRA-1519).
func TestANewerTaskTakesThePlaceOfOneATurnHasTaken(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if !h.pending().turnStarted() {
		t.Fatalf("the pending delivery at working is %+v, want the task whose turn this is", h.pending())
	}
	sentBefore := len(h.prompts())

	h.must(RequestDeliver{Claim: testToken, Task: "the newer phase start", ID: "row-2", Generation: 8})

	if got := h.pending(); got.Task != "the newer phase start" || got.Generation != 8 {
		t.Fatalf("pending %+v, want the newer task in the taken one's place", got)
	}
	if sent := len(h.prompts()); sent != sentBefore {
		t.Fatalf("%d takes while the turn runs, want the %d before it: the newer task waits for a take that can be accepted",
			sent, sentBefore)
	}

	h.must(StreamTurnEnd{Claim: testToken})

	sent := h.prompts()
	if len(sent) != sentBefore+1 || sent[len(sent)-1].Message != "the newer phase start" {
		t.Fatalf("takes %+v, want the newer task taken once at the turn's end", sent)
	}
}

// A take sent while the claim is in a turn is refused in-turn: the worker's host is busy, and a
// task taken into it would run behind whatever that turn is doing. The refusal is recorded at the
// value's revision, so the end of the turn writes the value again (offerPending), and the take
// after it is accepted.
func TestTakeIsRefusedInTurnAndTheTurnsEndWritesTheValueAgain(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)

	h.must(RequestDeliver{Claim: testToken, Task: "after this turn"})
	h.wantRefused(HoldInTurn)
	written := len(h.values.written())

	h.must(StreamTurnEnd{Claim: testToken})

	if got := len(h.values.written()); got != written+1 {
		t.Fatalf("the turn's end wrote %d values, want one: the take was refused at the latest revision", got-written)
	}
	if text := h.mustTake(); text != "after this turn" {
		t.Errorf("the take after the turn was answered %q, want the task queued during the turn", text)
	}
}

// A turn the agent starts before its worker took the task — an Envoy notice's, a person's steer —
// is not the task's: no take was accepted, so the turn has no task to start. Its end leaves the
// task waiting for its take.
func TestAForeignTurnDoesNotConfirmADeliveryNeverSent(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})

	h.must(StreamTurnStart{Claim: testToken})

	h.wantState(StateWorking)
	if h.pending().taken() {
		t.Fatal("a turn took a task its worker never took")
	}
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)
	if text := h.mustTake(); text != "the task" {
		t.Errorf("the take at the turn's end was answered %q, want the queued task", text)
	}
}

// The turn ended and the claim was written idle, but the delivery's retirement never landed. The
// finished delivery a restart reads back does not stand in the way of the next.
func TestAConfirmedDeliveryLeftBehindByACrashIsRetiredBeforeTheNextTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.store.fail("RetireDelivery", errBoom)
	if err := h.handle(StreamTurnEnd{Claim: testToken}); !errors.Is(err, errBoom) {
		t.Fatalf("turn end over a failing retirement returned %v, want the store's error", err)
	}
	if stored := h.store.load(testToken); stored.State != StateIdle || stored.Pending == nil || !stored.Pending.turnStarted() {
		t.Fatalf("stored %+v, want an idle claim still holding the delivery whose turn ran", stored)
	}
	h.store.fail("RetireDelivery", nil)
	h.restart()

	h.must(RequestDeliver{Claim: testToken, Task: "the next task"})

	if sent := h.wantPrompts(2)[1]; sent.Message != "the next task" {
		t.Errorf("took %+v, want the next task", sent)
	}
}

// The daemon saw a turn start and went down before it saw the turn end. After the restart the
// claim reads as working; the shim's reconnect is when the machine asks, and a turn that is over
// is ended — the claim does not wait in working for an agent_end it already missed. No report of
// that turn comes after it, so the end frame's bound finishes the task.
func TestAfterARestartATurnThatEndedMeanwhileIsEnded(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.restart()

	h.connect()

	h.wantState(StateIdle)
	h.advance(testRPC)
	if p := h.claim().Pending; p != nil {
		t.Errorf("pending %+v, want the finished turn's delivery retired", p)
	}
}

func TestAfterARestartATurnStillRunningStaysWorking(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.restart()
	h.conn.SetStreaming(true)

	h.connect()

	h.wantState(StateWorking)
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)
}

func TestRepeatOutboxDeliveryDoesNotQueueTheTaskTwice(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:42"})
	first := h.pending()
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:42"})

	if pending := h.pending(); pending.ID != "outbox:42" || pending.ID != first.ID || pending.Number != first.Number {
		t.Fatalf("pending delivery = %#v, want the original outbox delivery", pending)
	}
	if prompts := h.wantPrompts(1); prompts[0].DeliveryID != "outbox:42" {
		t.Fatalf("take = %#v, want outbox delivery id", prompts[0])
	}
	if written := h.claim().TaskWritten; written != 1 {
		t.Fatalf("the claim has written %d tasks, want the one", written)
	}
}

// A restored working claim asks its agent whether the turn an earlier daemon saw start is still
// running. An ask that fails (a get_state the agent did not answer in time) says nothing about the
// turn: the claim stays working and its task stays the turn's, and the next sweep asks again.
// Taken as "not streaming", it ended a turn still running and set the claim idle.
func TestAFailedGetStateLeavesARestoredTurnRunning(t *testing.T) {
	restored := fixture(StateWorking)
	h := newHarnessOf(t, restored)
	if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	h.conns.Register(testToken, h.conn)
	h.conn.FailGetState(context.DeadlineExceeded)
	h.start(restored)

	h.must(StreamHello{Claim: testToken, Generation: restored.Generation})
	h.wantState(StateWorking)
	if p := h.claim().Pending; p == nil || p.ID != restored.Pending.ID || !p.turnStarted() {
		t.Fatalf("after the failed ask pending = %+v, want the turn's task kept", p)
	}
	h.observe(runtime.Alive)
	h.wantState(StateWorking)
	// The hello's ask and this sweep's went unanswered: still under the limit, so no warning yet.
	if warned := h.logs.lines("level=WARN", "unanswered"); len(warned) != 0 {
		t.Fatalf("warned after two unanswered asks: %q", warned)
	}
	// The third unanswered ask warns, naming the claim, so an operator sees a turn the daemon
	// cannot read.
	h.observe(runtime.Alive)
	h.wantState(StateWorking)
	if warned := h.logs.lines("level=WARN", "unanswered", string(testToken)); len(warned) != 1 {
		t.Fatalf("after three unanswered asks warned %q, want one warning naming the claim", warned)
	}
	h.wantPrompts(0)

	// The agent answers on a later sweep: the turn is over, and it ends now; the end frame's bound
	// finishes the task no report named.
	h.conn.FailGetState(nil)
	h.conn.SetStreaming(false)
	h.observe(runtime.Alive)
	h.wantState(StateIdle)
	h.advance(testRPC)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("after the answered ask pending = %+v, want the turn's task retired", p)
	}
}

// A sweep can reach a restored working claim before its shim dials back. With no connection it
// asks nothing and leaves the question for the hello, which then ends a turn that is over.
func TestASweepBeforeTheHelloLeavesTheRestartsQuestionForIt(t *testing.T) {
	restored := fixture(StateWorking)
	h := newHarnessOf(t, restored)
	if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	h.start(restored)

	h.observe(runtime.Alive)
	h.wantState(StateWorking)

	h.conns.Register(testToken, h.conn)
	h.conn.SetStreaming(false)
	h.must(StreamHello{Claim: testToken, Generation: restored.Generation})
	h.wantState(StateIdle)
}

// A suspension ends the claim's phase, so the workflow task it leaves is the finished phase's and
// goes with it. An operator's task belongs to no phase: the operator asked for it, was answered
// 200, and a phase change they had no part in must not take it. It waits out the suspension and
// is taken when the claim resumes.
func TestAnOperatorsTaskSurvivesASuspensionAndGoesOnResume(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered) // registered, not ready: the task waits for its take
	h.must(RequestDeliver{Claim: testToken, Task: "look at the tree"})
	h.wantPrompts(0)

	h.must(RequestSuspend{Claim: testToken, By: SuspendedByOperator})
	h.wantState(StateSuspended)

	if pending := h.pending(); pending.Task != "look at the tree" {
		t.Fatalf("pending across the suspension = %+v, want the operator's task kept", pending)
	}
	if stored, ok := h.store.delivery(testToken); !ok || stored.Task != "look at the tree" {
		t.Fatalf("stored delivery across the suspension = %+v (%v), want the operator's task kept", stored, ok)
	}

	h.must(RequestResume{Claim: testToken})
	h.reach(StateReady)

	if sent := h.wantPrompts(1)[0]; sent.Message != "look at the tree" {
		t.Errorf("take after the resume = %+v, want the operator's task", sent)
	}
}

// The start of a take's turn writes the claim and the delivery, and a daemon can die between
// them. The delivery is what the turn's finish retires and what a repeated take is answered from,
// so a claim recorded as working with its delivery's turn unrecorded is a turn the next daemon
// cannot tell from a turn of the agent's own. The two writes go together.
func TestAConfirmationDoesNotSurviveHalfWritten(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.wantPrompts(1)

	h.store.fail("PutDelivery", errBoom)
	err := h.m.Handle(context.Background(), StreamTurnStart{Claim: testToken})
	if err == nil {
		t.Fatal("the turn start reported success with the delivery write failing")
	}

	stored := h.store.load(testToken)
	if stored.State == StateWorking && (stored.Pending == nil || !stored.Pending.turnStarted()) {
		t.Fatalf("the store holds a working claim whose delivery's turn is unrecorded: %+v, pending %+v",
			stored.State, stored.Pending)
	}
}

// A root architect's task of a run is held, never dropped, while its root has nobody to hear it
// (Deps.RootHears): such a task names no phase, so nothing else would keep it from being taken by
// an architect whose tree is parked, lingering or waiting for its slot, and the decisions it
// names are told when its take is accepted. The take is refused root-hears, the task stays
// pending, uncharged, and the next deliver request — the start of the root's next run, which the
// pending task refuses — is what writes its value again. A task of no run is nobody's to hold: an
// operator's own is taken whatever the root is doing.
func TestARootArchitectsTaskIsHeldUntilItsRootHasAnArchitectToHearIt(t *testing.T) {
	h := newBareHarness(t)
	h.token = rootToken
	hears := false
	asked := 0
	h.deps.RootHears = func(_ context.Context, c Claim, d Delivery) (bool, error) {
		asked++
		if c.Token != rootToken || d.Generation == 0 || d.Phase != "" {
			t.Errorf("RootHears asked about claim %s and delivery %+v, want only the root architect's task of a run", c.Token, d)
		}
		return hears, nil
	}
	c := rootClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: rootToken, Task: "a person decided on the tree", Generation: 2, Row: 7})
	h.wantPrompts(0)
	if p := h.pending(); p.Task != "a person decided on the tree" || p.taken() {
		t.Fatalf("pending delivery = %+v, want the held task untaken", p)
	}
	h.wantBudgets(Budgets{})
	if asked == 0 {
		t.Fatal("the held task was never put to RootHears")
	}

	// The root runs again: its start row is refused by the held task, and that refusal is what
	// writes the held task's value again.
	hears = true
	err := h.handle(RequestDeliver{Claim: rootToken, Task: "the next run's task", Generation: 3, Row: 11})
	if !errors.Is(err, ErrDeliveryPending) {
		t.Fatalf("the next run's deliver = %v, want it refused by the pending task", err)
	}
	sent := h.wantPrompts(1)[0]
	if sent.Message != "a person decided on the tree" {
		t.Fatalf("take = %q, want the task held through the park", sent.Message)
	}
}

// An operator's own task names no run, so the hold never applies to it: its take is accepted
// while the root has no architect to hear a task of a run.
func TestAnOperatorsTaskIsPromptedWhileTheRootHasNoArchitectToHearOne(t *testing.T) {
	h := newBareHarness(t)
	h.token = rootToken
	h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return false, nil }
	c := rootClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: rootToken, Task: "the operator's own task"})
	if sent := h.wantPrompts(1)[0]; sent.Message != "the operator's own task" {
		t.Fatalf("take = %q, want the operator's task taken", sent.Message)
	}
}
