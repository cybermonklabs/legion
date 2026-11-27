package supervise

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

func TestATaskQueuedBeforeReadyIsDeliveredOnceAtReady(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	queued := h.pending()
	if stored, ok := h.store.delivery(testToken); !ok || stored.ID != queued.ID || stored.Task != "implement the plan" {
		t.Fatalf("stored delivery %+v, want the queued task", stored)
	}
	h.connect()
	h.register()
	h.wantPrompts(0)

	h.ready()

	sent := h.wantPrompts(1)[0]
	if sent.DeliveryID != queued.ID || sent.Message != "implement the plan" {
		t.Errorf("sent %+v, want the queued task under its delivery id", sent)
	}
	if h.pending().DeliveredAt.IsZero() {
		t.Error("the acknowledged delivery has no DeliveredAt")
	}
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	if h.pending().ConfirmedAt.IsZero() {
		t.Error("the turn did not confirm the delivery")
	}
	if stored, _ := h.store.delivery(testToken); stored.ConfirmedAt.IsZero() {
		t.Error("the confirmation was not persisted")
	}

	h.must(StreamTurnEnd{Claim: testToken})

	h.wantState(StateIdle)
	if p := h.claim().Pending; p != nil {
		t.Errorf("pending %+v after its turn ended, want none", p)
	}
	if _, ok := h.store.delivery(testToken); ok {
		t.Error("the store still holds the delivery its turn finished")
	}
	h.advance(10 * testRPC)
	h.wantPrompts(1)
	h.wantBudgets(Budgets{})
}

// The phase entry a confirmed task's retire leaves on the claim, durably, beside the run: the
// worker goes on working that entry in whatever turn comes next — one an Envoy notice starts,
// with no delivery behind it — and the completion it reports there is still that entry's. A
// later task that carries no entry, a resume or a production check's look, moves the run on
// and leaves the entry alone, so a resumed worker's in-flight completion is never refused.
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

// A confirmed look stays on the claim across phase work. Only another look replaces it: the
// completion request itself, not the pending task's phase, says whether it reports a look.
func TestARetiredLooksTaskLeavesItsLookOnTheClaimUntilAnotherLooksTask(t *testing.T) {
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
	if got := h.claim().ServingCheck(); got != look {
		t.Fatalf("during the phase task's turn the claim serves look %+v, want %+v", got, look)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if stored := h.store.load(testToken); stored.ServingCheckLook != look {
		t.Fatalf("after the phase task's turn the stored claim serves look %+v, want %+v", stored.ServingCheckLook, look)
	}
	next := CheckLook{Look: 3, OpenedAt: opened}
	h.must(RequestDeliver{Claim: testToken, Task: "check again", Generation: 8, CheckLook: next})
	h.must(StreamTurnStart{Claim: testToken})
	if got := h.claim().ServingCheck(); got != next {
		t.Fatalf("during the next look's turn the claim serves %+v, want %+v", got, next)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if got, stored := h.claim().ServingCheck(), h.store.load(testToken).ServingCheckLook; got != next || stored != next {
		t.Fatalf("after the next look the claim serves %+v, stored %+v, want %+v", got, stored, next)
	}
}

// The retro run a confirmed task's retire leaves on the claim, durably, as it leaves a look: the
// agent of the retro Legion runs after an issue's close goes on working that retro in whatever
// turn comes next, and the completion it reports there is still that retro's report
// (api/handoff.go), where it would otherwise be refused as a completion on a done issue. A task
// of a phase takes it back, since a worker in the workflow again reports its phase.
func TestARetiredRetrosTaskLeavesItsRunOnTheClaimAndAPhaseTaskTakesItBack(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "Continue Closed. Issue: LEGION-209. Phase: retro.", Generation: 7, RetroRun: 7})
	if p := h.pending(); p.RetroRun != 7 {
		t.Fatalf("the queued delivery carries retro run %d, want the start's 7", p.RetroRun)
	}
	if stored, ok := h.store.delivery(testToken); !ok || stored.RetroRun != 7 {
		t.Fatalf("the stored delivery is %+v, want it to carry retro run 7: a restart reads the run back", stored)
	}
	h.must(StreamTurnStart{Claim: testToken})
	if got := h.claim().ServingRetro(); got != 7 {
		t.Fatalf("during the retro's turn the claim serves retro run %d, want 7", got)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if stored := h.store.load(testToken); stored.ServingRetroRun != 7 {
		t.Fatalf("after the turn the stored claim serves retro run %d, want 7", stored.ServingRetroRun)
	}
	if got := h.claim().ServingRetro(); got != 7 {
		t.Fatalf("after the turn the claim serves retro run %d, want 7: its next turn's report is still this retro's", got)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "Continue Workflow. Issue: LEGION-209. Phase: testing.", Generation: 8, Phase: phase.Testing, Entry: 2})
	h.must(StreamTurnStart{Claim: testToken})
	if got := h.claim().ServingRetro(); got != 0 {
		t.Fatalf("during the phase task's turn the claim serves retro run %d, want none: its completion is the phase's", got)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if stored := h.store.load(testToken); stored.ServingRetroRun != 0 {
		t.Fatalf("after the phase task's turn the stored claim serves retro run %d, want none", stored.ServingRetroRun)
	}
}

// The retro run a claim serves outlasts what the daemon does to the claim's process: a restart
// builds the machine again from what the store holds, and the rule's resume of the claim's kept
// session (the retro's agent suspended by an idle release, then woken for the retro or a finding,
// as the daemon's wake and its retro start resume it) relaunches it. The claim's next turn there
// is still the retro's, as the run and the entry are, so its report is taken and not refused.
func TestAClaimServingARetroKeepsItAcrossARestartAndARelaunchOfItsSession(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "Continue Closed. Issue: LEGION-209. Phase: retro.", Generation: 7, RetroRun: 7})
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	if got := h.claim().ServingRetro(); got != 7 {
		t.Fatalf("after the retro's turn the claim serves retro run %d, want 7", got)
	}

	h.restart()
	if got := h.claim().ServingRetro(); got != 7 {
		t.Fatalf("after a daemon restart the claim serves retro run %d, want the 7 the store held", got)
	}

	before := h.generation()
	h.must(RequestSuspend{Claim: testToken, Reason: "the idle pod was released", By: SuspendedByReleased})
	h.must(RequestRuleResume{Claim: testToken, Reason: "a wake for the retro's finding"})
	h.wantCalls("Resume", 1)
	h.relaunched()
	if h.generation() == before {
		t.Fatalf("the claim is at launch %d after the resume, want another launch of the kept session", h.generation())
	}
	if got := h.claim().ServingRetro(); got != 7 {
		t.Fatalf("after a relaunch in the kept session the claim serves retro run %d, want 7", got)
	}
}

// A claim that owes a look and a retro serves them in turn (INFRA-5602's s12), and each report is
// routed to its own handler. A task queued behind the running turn of the other kind takes the
// running turn's delivery onto the claim as what it was left serving, and leaves that marker
// standing until the queued task's own turn confirms it: the look's report is still the look's
// while the retro waits, and the retro's the retro's while a look waits. The confirmed look then
// takes the retro back, so a report from its turn that names no check is no retro's. The confirmed
// retro leaves the look on the claim, as a phase task does: only a completion's checkOpenedAt makes
// it a report on that look, and the look is what such a report is attributed to.
func TestARetroAndALookOfOneClaimAreServedInTurn(t *testing.T) {
	opened := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	look := CheckLook{Look: 2, OpenedAt: opened}
	const lookTask, retroTask = "Production check of LEGION-209.", "Continue Closed. Issue: LEGION-209. Phase: retro."
	lookTurn := func(h *harness) {
		h.must(RequestDeliver{Claim: testToken, Task: lookTask, Generation: 7, CheckLook: look})
	}
	retroTurn := func(h *harness) {
		h.must(RequestDeliver{Claim: testToken, Task: retroTask, Generation: 7, RetroRun: 7})
	}
	t.Run("a retro queued behind a look's running turn", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		lookTurn(h)
		h.must(StreamTurnStart{Claim: testToken})
		retroTurn(h)
		if got, serving := h.claim().ServingCheck(), h.claim().ServingRetro(); got != look || serving != 0 {
			t.Fatalf("while the look's turn runs the claim serves look %+v and retro run %d, want %+v and none: the retro's task is not taken yet", got, serving, look)
		}
		h.must(StreamTurnEnd{Claim: testToken})
		h.must(StreamTurnStart{Claim: testToken})
		if got, serving := h.claim().ServingCheck(), h.claim().ServingRetro(); serving != 7 || got != look {
			t.Fatalf("while the retro's own turn runs the claim serves retro run %d and look %+v, want 7 and %+v", serving, got, look)
		}
		h.must(StreamTurnEnd{Claim: testToken})
		if stored := h.store.load(testToken); stored.ServingRetroRun != 7 || stored.ServingCheckLook != look {
			t.Fatalf("after the retro's turn the stored claim serves retro run %d and look %+v, want 7 and %+v", stored.ServingRetroRun, stored.ServingCheckLook, look)
		}
	})
	t.Run("a look queued behind a retro's running turn", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		retroTurn(h)
		h.must(StreamTurnStart{Claim: testToken})
		lookTurn(h)
		if got, serving := h.claim().ServingRetro(), h.claim().ServingCheck(); got != 7 || serving != (CheckLook{}) {
			t.Fatalf("while the retro's turn runs the claim serves retro run %d and look %+v, want 7 and none: the look's task is not taken yet", got, serving)
		}
		h.must(StreamTurnEnd{Claim: testToken})
		h.must(StreamTurnStart{Claim: testToken})
		if got, serving := h.claim().ServingRetro(), h.claim().ServingCheck(); got != 0 || serving != look {
			t.Fatalf("while the look's own turn runs the claim serves retro run %d and look %+v, want none and %+v", got, serving, look)
		}
		h.must(StreamTurnEnd{Claim: testToken})
		if stored := h.store.load(testToken); stored.ServingRetroRun != 0 || stored.ServingCheckLook != look {
			t.Fatalf("after the look's turn the stored claim serves retro run %d and look %+v, want none and %+v", stored.ServingRetroRun, stored.ServingCheckLook, look)
		}
	})
}

func TestADeliveryToAnIdleClaimIsSentAtOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.must(RequestDeliver{Claim: testToken, Task: "the next task"})
	if sent := h.wantPrompts(2)[1]; sent.Message != "the next task" {
		t.Errorf("sent %+v, want the next task", sent)
	}
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	if h.pending().ConfirmedAt.IsZero() {
		t.Error("the idle claim's turn did not confirm its delivery")
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
				t.Errorf("sent %+v at ready, want the task queued before it", sent)
			}
		})
	}
}

// A second delivery is refused for an untaken delivery the claim already holds, whatever its
// state: that task has reached nobody, so accepting over it would lose a task. The refusal is
// ErrDeliveryPending, and it says so rather than naming the state.
//
// This test read StateWorking until dispatch://INFRA-714 — the state where the claim's own task
// has been taken by a turn — and so pinned the refusal this change ends
// (TestANewerTaskTakesThePlaceOfOneATurnHasTaken, below). It reaches ready with a task queued and
// unsent instead, which is the untaken state itself.
func TestASecondDeliveryWhileOneIsPendingIsRefused(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "row-1"})
	if !h.pending().ConfirmedAt.IsZero() {
		t.Fatalf("the pending delivery at ready is confirmed %+v, want the untaken task this test is about", h.pending())
	}

	err := h.handle(RequestDeliver{Claim: testToken, Task: "another", ID: "row-2"})

	var refused *RefusedError
	if !errors.As(err, &refused) || !errors.Is(err, ErrDeliveryPending) || err.Error() != "deliver refused: a delivery is already pending" {
		t.Fatalf("a second delivery returned %v, want the pending-delivery refusal", err)
	}
	if h.pending().Task != "implement the plan" {
		t.Errorf("pending %+v, want the first task kept", h.pending())
	}
}

// A delivery whose task a turn has taken holds nothing back: the worker read it, and what the
// delivery records is the turn in flight. A newer task takes its place — the phase's own next
// start, which the daemon queues while that turn runs — and it is sent at the next point a turn
// may take it, not into the running one. Refusing it instead left three supervise rows of the
// live daemon answered "a delivery is already pending" with nothing ever sent and the phase
// waiting on a worker that had finished (dispatch://INFRA-1352, INFRA-1443, INFRA-1519).
func TestANewerTaskTakesThePlaceOfOneATurnHasTaken(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if h.pending().ConfirmedAt.IsZero() {
		t.Fatalf("the pending delivery at working is %+v, want the task this turn took", h.pending())
	}
	sentBefore := len(h.prompts())

	h.must(RequestDeliver{Claim: testToken, Task: "the newer phase start", ID: "row-2", Generation: 8})

	if got := h.pending(); got.Task != "the newer phase start" || got.Generation != 8 {
		t.Fatalf("pending %+v, want the newer task in the taken one's place", got)
	}
	if sent := len(h.prompts()); sent != sentBefore {
		t.Fatalf("%d prompts while the turn runs, want the %d sent before it: the newer task waits for a turn that can take it",
			sent, sentBefore)
	}

	h.must(StreamTurnEnd{Claim: testToken})

	sent := h.prompts()
	if len(sent) != sentBefore+1 || sent[len(sent)-1].Message != "the newer phase start" {
		t.Fatalf("prompts %+v, want the newer task sent once at the turn's end", sent)
	}
}

func TestASendWithNoConnectionRequeuesWithoutACharge(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.conns.Remove(testToken)

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			h.wantPrompts(base)
			h.wantBudgets(Budgets{})
			if !h.pending().DeliveredAt.IsZero() {
				t.Error("a delivery with no connection reads as delivered")
			}
			if lines := h.logs.lines("no connection"); len(lines) != 1 {
				t.Errorf("logged %v, want the missing connection once", lines)
			}

			h.connect()

			if sent := h.wantPrompts(base + 1)[base]; sent.DeliveryID != h.pending().ID {
				t.Errorf("sent %+v, want the queued delivery", sent)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

// A prompt lost to the transport was never the agent's answer: it waits for the shim's next
// hello, uncharged, under the same id.
func TestATransportErrorRequeuesWithoutACharge(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.conn.FailPrompt(errBoom)

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			first := h.wantPrompts(base + 1)[base]
			h.wantBudgets(Budgets{})
			if !h.pending().DeliveredAt.IsZero() {
				t.Error("a lost delivery reads as delivered")
			}
			h.advance(10 * testRPC)
			h.wantPrompts(base + 1)

			h.conn.FailPrompt(nil)
			h.connect()

			if second := h.wantPrompts(base + 2)[base+1]; second.DeliveryID != first.DeliveryID {
				t.Errorf("re-sent under %s, want the same delivery id %s", second.DeliveryID, first.DeliveryID)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

// A prompt the agent refuses is charged like an acknowledged prompt that started no turn
// (LEGION-60), and retried at the next sweep rather than on the spot, so an agent that refuses is
// not re-prompted into another refusal at once. Oh My Pi's busy answer is no refusal of the work
// and is charged nothing (TestABusyAgentsRefusalOfTheSendIsChargedNothingAndItsTurnsEndSendsTheTask).
func TestARefusedPromptIsChargedAndRetriedAtTheNextSweep(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.conn.RefusePrompt("the model provider refused the request")

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			first := h.wantPrompts(base + 1)[base]
			h.wantBudgets(Budgets{PromptFailures: 1})
			if p := h.pending(); p.ID != first.DeliveryID || !p.DeliveredAt.IsZero() {
				t.Errorf("pending %+v, want the refused delivery queued under its own id", p)
			}
			h.advance(10 * testRPC)
			h.wantPrompts(base + 1)
			h.wantState(state)

			h.conn.FailPrompt(nil)
			h.observe(runtime.Alive)

			if second := h.wantPrompts(base + 2)[base+1]; second.DeliveryID != first.DeliveryID {
				t.Errorf("re-sent under %s, want %s", second.DeliveryID, first.DeliveryID)
			}
			h.wantBudgets(Budgets{PromptFailures: 1})
		})
	}
}

// An agent that refuses every prompt is not retried forever: its refusals spend the prompt
// budget, and the retirement that follows is the same as for prompts that start no turn.
func TestAnAgentThatRefusesEveryPromptRunsOutOfItsBudget(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	refusing := h.locator()
	h.conn.RefusePrompt("the model provider refused the request")
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.observe(runtime.Alive)
	h.wantBudgets(Budgets{PromptFailures: 2})

	h.observe(runtime.Alive)

	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != refusing {
		t.Errorf("suspended %+v, want the refusing process", suspend.Locator)
	}
	h.wantCalls("Resume", 1)
	h.wantBudgets(Budgets{PromptRetires: 1})
	h.wantState(StateLaunching)
}

// Oh My Pi answers a prompt that reaches it mid-turn with the busy code, which the
// stream mints as OutcomeAgentBusy: the agent is in a turn of its own — an Envoy notice's, which
// a tree publishes while its workers boot — so it is working, not refusing the work. That answer
// to the task's own send is charged nothing, as the same answer after an acknowledgement is
// (lateRefused), and the turn's end sends the task again. Charged, three such collisions in a row
// retired the agent in the middle of the turn doing the work, and two retirements failed its
// claim.
func TestABusyAgentsRefusalOfTheSendIsChargedNothingAndItsTurnsEndSendsTheTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	base := len(h.prompts())
	h.conn.FailPrompt(busyRefusal())
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	task := h.pending().ID

	for sent := 1; sent <= 4; sent++ {
		if prompt := h.wantPrompts(base + sent)[base+sent-1]; prompt.DeliveryID != task {
			t.Fatalf("send %d went under %s, want the task's %s", sent, prompt.DeliveryID, task)
		}
		h.wantBudgets(Budgets{})
		h.wantCalls("Suspend", 0)
		if p := h.pending(); p.ID != task || !p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
			t.Fatalf("after busy refusal %d the task is %+v, want it waiting unread", sent, p)
		}
		h.must(StreamTurnStart{Claim: testToken}) // the notice's turn
		if run := h.claim().ServingRun(); run != 0 {
			t.Fatalf("during the notice's turn the claim serves run %d, want none: the agent never read the task", run)
		}
		if sent == 4 {
			h.conn.FailPrompt(nil) // the agent takes the next prompt
		}
		h.must(StreamTurnEnd{Claim: testToken}) // its end sends the task again
	}

	if prompt := h.wantPrompts(base + 5)[base+4]; prompt.DeliveryID != task {
		t.Fatalf("the send after the last notice went under %s, want the task's %s", prompt.DeliveryID, task)
	}
	h.must(StreamTurnStart{Claim: testToken}) // the task's own turn
	if run := h.claim().ServingRun(); run != 7 {
		t.Fatalf("during the task's turn the claim serves run %d, want the task's 7", run)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the task's turn: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
	h.wantBudgets(Budgets{})
	h.wantCalls("Suspend", 0)
	h.wantState(StateIdle)
}

// A claim registered while the agent's own turn was already running never sees that turn start:
// the daemon's only stream event of it is its end, and until this release a turn end in ready
// matched no row at all (table.go's onTurnEnd), so the task the busy refusal left unread waited
// for a turn start that had already happened. Three workers sat idle holding their phase task for
// 35 to 75 minutes that way, and each completion from the blind turn was refused HANDOFF_NO_RUN
// (dispatch://INFRA-1691, INFRA-1545, INFRA-722, A9). The end sends it now, and nothing goes into
// the turn itself.
//
// The busy refusal no longer leaves such a claim ready: it records the claim working for the turn
// it names (recordBusyTurn), so that turn's end reaches turnEnded, which sends the task. A send
// that never reached the agent, one lost on the way, still leaves the claim ready holding its task
// unread while the agent's own turn runs, and that turn's end is sent from blindTurnEnded. Both
// are driven.
//
// The busy-refusal test above drives the same refusal with a turn start before each end, which is
// why this never showed: that is the agent whose turn the daemon watched begin.
func TestATurnEndTheDaemonNeverSawBeginSendsTheTaskItHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		// miss sends the task, which does not reach a turn, and returns the state the claim is
		// left in while the agent's own turn runs.
		miss func(h *harness) ClaimState
	}{
		{name: "the send answered busy", miss: func(h *harness) ClaimState {
			// The prompt is acknowledged and then refused: a turn of the agent's own is running,
			// one that began before this claim was registered. The refusal takes the task back
			// under a new id and records the claim working for that turn.
			h.must(RequestDeliver{Claim: testToken, Task: "the phase task", Generation: 7})
			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: h.pending().ID, Error: busyWords, Outcome: runtime.OutcomeAgentBusy})
			return StateWorking
		}},
		{name: "the send lost on the way", miss: func(h *harness) ClaimState {
			h.conn.FailPrompt(errors.New("the frame was lost on the way"))
			h.must(RequestDeliver{Claim: testToken, Task: "the phase task", Generation: 7})
			h.conn.FailPrompt(nil)
			return StateReady
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			base := len(h.prompts())

			state := tc.miss(h)
			if p := h.pending(); p.Task != "the phase task" || !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
				t.Fatalf("after the send that reached no turn the task is %+v, want it waiting unread", p)
			}
			h.wantPrompts(base + 1)
			h.wantState(state)

			// The blind turn ends. This is the only stream event of it the daemon ever gets.
			h.must(StreamTurnEnd{Claim: testToken})

			if prompt := h.wantPrompts(base + 2)[base+1]; prompt.DeliveryID != h.pending().ID {
				t.Fatalf("the send after the blind turn went under %s, want the task's %s", prompt.DeliveryID, h.pending().ID)
			}
			h.must(StreamTurnStart{Claim: testToken})
			if run := h.claim().ServingRun(); run != 7 {
				t.Fatalf("during the task's own turn the claim serves run %d, want the task's 7: a completion from it would be refused HANDOFF_NO_RUN", run)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

// The notice's turn usually starts while the task's send is still in flight, so its agent_start
// confirms the task (turnStarted's in-flight rule) before the busy answer arrives. The refusal takes
// that confirmation back and is still charged nothing; the claim stays working, and the turn's end
// sends the task again rather than retiring it as served.
func TestABusyRefusalAfterTheNoticesTurnConfirmedTheSendIsChargedNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	task := waitFor(t, "the send", gated.entered)
	if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil { // the notice's turn
		t.Fatal(err)
	}
	if p := h.pending(); p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after a turn started with the send in flight = %+v, want it confirmed", p)
	}
	gated.FailPrompt(busyRefusal())
	gated.release <- struct{}{}
	h.m.Wait()

	if p := h.pending(); p.ID != task || !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
		t.Fatalf("pending after the busy refusal = %+v, want the task waiting unread and unconfirmed", p)
	}
	h.wantBudgets(Budgets{})
	h.wantState(StateWorking)
	if run := h.claim().ServingRun(); run != 0 {
		t.Fatalf("during the notice's turn the claim serves run %d, want none", run)
	}

	gated.FailPrompt(nil)
	gated.release <- struct{}{}             // let the re-send through
	h.must(StreamTurnEnd{Claim: testToken}) // the notice's turn ends: the task goes again
	if sent := gated.Prompts(); len(sent) != 2 || sent[1].DeliveryID != task {
		t.Fatalf("prompts = %+v, want the refused send and its re-send under %s", sent, task)
	}
	h.must(StreamTurnStart{Claim: testToken}) // the task's own turn
	if run := h.claim().ServingRun(); run != 7 {
		t.Fatalf("during the task's turn the claim serves run %d, want the task's 7", run)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the task's turn: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
	h.wantBudgets(Budgets{})
	h.wantCalls("Suspend", 0)
}

// The sweep that finds a ready or idle claim's process alive is what retries a delivery waiting
// for its connection when nothing else will: no hello, no turn end, no request.
func TestAnAliveSweepSendsAWaitingDelivery(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.conns.Remove(testToken)
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			h.observe(runtime.Alive)
			h.wantPrompts(base)

			h.conns.Register(testToken, h.conn)
			h.observe(runtime.Alive)

			if sent := h.wantPrompts(base + 1)[base]; sent.DeliveryID != h.pending().ID {
				t.Errorf("sent %+v, want the waiting delivery", sent)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

func TestAnAckWithNoTurnChargesOneFailureAndRotatesTheDelivery(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			first := h.wantPrompts(base + 1)[base]

			h.advance(testRPC)

			h.wantBudgets(Budgets{PromptFailures: 1})
			rotated := h.pending()
			if rotated.ID == first.DeliveryID {
				t.Fatal("the delivery id was not rotated after an acknowledged prompt started no turn")
			}
			if stored, _ := h.store.delivery(testToken); stored.ID != rotated.ID {
				t.Errorf("stored delivery id %s, want the rotated %s", stored.ID, rotated.ID)
			}
			h.wantPrompts(base + 1) // not re-sent on the spot: a merely slow agent is not re-prompted at once
			h.wantState(state)

			h.observe(runtime.Alive)

			if second := h.wantPrompts(base + 2)[base+1]; second.DeliveryID != rotated.ID || second.Message != "the task" {
				t.Errorf("re-sent %+v at the sweep, want the task under the rotated id", second)
			}
		})
	}
}

// B1: the agent's own agent_start carries no delivery id and confirms the delivery in flight —
// here before the machine has even heard the send's acknowledgement.
func TestARealTurnStartConfirmsTheDeliveryInFlight(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)

	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	inFlight := waitFor(t, "the send", gated.entered)
	// Handle, not must: must waits for the send, which the gate is holding.
	if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
		t.Fatal(err)
	}
	h.wantState(StateWorking)
	if p := h.pending(); p.ID != inFlight || p.ConfirmedAt.IsZero() {
		t.Fatalf("pending %+v, want the in-flight delivery %s confirmed", p, inFlight)
	}
	gated.release <- struct{}{}
	h.m.Wait()

	if live := h.clock.Live(); live != 0 {
		t.Errorf("%d timers armed after the late acknowledgement of a confirmed delivery", live)
	}
	h.advance(10 * testRPC)
	h.wantBudgets(Budgets{})
	h.wantState(StateWorking)
}

// The stream hands the machine a turn's frames on its queue and the send's acknowledgement on the
// send's own goroutine, so a turn the prompt started can start and end before the acknowledgement
// is read. The claim is idle again then, holding the task its turn confirmed for the send's
// answer (resultPending's send window), and takeAck only records the prompt. It arms no wait for a
// turn (acked): that turn has run, and a wait armed for it would charge the prompt when the result
// comes after the bound, and take the task back under a new id (noTurn) which the result then names
// no longer. The result retires the task, once.
func TestAnAcknowledgementReadAfterItsTurnEndedArmsNoWaitForATurn(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	id := testwait.Receive(t, "the send", gated.entered)
	for _, ev := range []Event{StreamTurnStart{Claim: testToken}, StreamTurnEnd{Claim: testToken}} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	h.wantState(StateIdle)
	if p := h.pending(); p.ID != id || p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the turn's end = %+v, want %s still held, confirmed, for the send's answer", p, id)
	}

	gated.release <- struct{}{}
	h.m.Wait()
	if h.armed(TimerTurn) {
		t.Error("a wait for a turn is armed after the acknowledgement of a task whose turn had ended, want none")
	}
	h.advance(3 * testRPC)
	h.wantBudgets(Budgets{})
	if p := h.pending(); p.ID != id || p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the wait for a turn would have run out twice = %+v, want %s unchanged, confirmed", p, id)
	}

	if err := h.m.Handle(h.ctx, StreamPromptResult{Claim: testToken, DeliveryID: id, AgentInvoked: true}); err != nil {
		t.Fatal(err)
	}
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the result: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
	h.wantBudgets(Budgets{})
}

// A result can reach the machine while the turn is still working and its send's acknowledgement
// is still in flight. The acknowledgement must not record the prompt as outstanding
// (Machine.outstanding): the turn's end would then wait for a result the machine has already
// decided.
func TestAnAcknowledgementAfterItsResultDoesNotHoldTheTaskAtTurnEnd(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	id := testwait.Receive(t, "the send", gated.entered)
	for _, ev := range []Event{
		StreamTurnStart{Claim: testToken},
		StreamPromptResult{Claim: testToken, DeliveryID: id, AgentInvoked: true},
	} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	h.wantState(StateWorking)
	if p := h.pending(); p.ID != id || p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the result, before the turn ended = %+v, want %s confirmed", p, id)
	}
	gated.release <- struct{}{}
	h.m.Wait()
	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the acknowledgement and turn end: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
}

// B1: only the shim's replayed agent_start carries a delivery id; one carrying an id the claim
// has moved past is dropped and logged once, and one carrying the current id confirms it.
func TestAReplayedTurnStartIsFencedOnItsDeliveryID(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	stale := h.wantPrompts(1)[0].DeliveryID
	h.advance(testRPC)
	h.observe(runtime.Alive)
	current := h.wantPrompts(2)[1].DeliveryID

	h.must(StreamTurnStart{Claim: testToken, DeliveryID: stale})
	h.must(StreamTurnStart{Claim: testToken, DeliveryID: stale})

	h.wantState(StateReady)
	if !h.pending().ConfirmedAt.IsZero() {
		t.Fatal("a replay carrying a stale delivery id confirmed the current delivery")
	}
	if lines := h.logs.lines("dropped a stale event", stale); len(lines) != 1 {
		t.Errorf("the stale replay was logged %d times, want once: %v", len(lines), lines)
	}

	h.must(StreamTurnStart{Claim: testToken, DeliveryID: current})
	h.wantState(StateWorking)
	if h.pending().ConfirmedAt.IsZero() {
		t.Error("a replay carrying the current delivery id did not confirm it")
	}
}

// A turn that starts after the no-turn bound names no prompt, and while the prompt Oh My Pi
// acknowledged is still outstanding it is that prompt's turn (Machine.outstanding). The pinned Oh
// My Pi acknowledges a prompt at its admission, before its pre-turn work, runs one admitted prompt
// at a time and refuses the rest ("Agent is already processing"), and hands each prompt its one
// prompt_result at the end of the first run after its admission (rpc-prompt-results.ts); the
// Claude Code bridge takes one prompt at a time too (internal/claudebridge's busyWords and
// promptRun). So the late start confirms the task, and the prompt's result — naming the id the
// bound took back — retires it, and no prompt follows. Before, the late start was taken for a
// foreign turn, the result was dropped at the fence, and the task was sent again when the turn
// ended, to a worker that had just done it: the loop dispatch://INFRA-3276 measured.
//
// The prompt's own result is what corrects a start taken for it that was in fact another input's.
// A result that says the prompt ran no turn takes the task back, charged nothing more than the
// bound charged it, and the end of the running turn sends the task again — unless the issue has
// left the phase the task was queued for meanwhile, which drops the task rather than hand the
// worker the task of a phase that is over (sendPending).
//
// So the three cases assert: a late turn that is the prompt's own, whose result retires the task
// with no prompt after it; a late turn the prompt's result shows was foreign, after which the task
// is sent again while the issue holds its phase; and the same turn once the issue has left the
// phase, after which nothing is sent. The name speaks of the last two alone: it is the name the
// acceptance check of dispatch://INFRA-3276 R4 (c-r4-tests) runs the test by, so it stays.
func TestALateForeignTurnResendsTheTaskUnlessTheIssueLeftThePhase(t *testing.T) {
	for _, tc := range []struct {
		name string
		// ranNoTurn is the prompt's result saying it ran no turn, landing while the late turn
		// runs; otherwise the result is that of the turn the prompt ran, right behind its end.
		ranNoTurn     bool
		holdsAfterRun bool
		prompts       int
	}{
		{name: "the late turn is the prompt's, and its result retires the task", holdsAfterRun: true, prompts: 1},
		{name: "the prompt ran no turn, and the issue is still in the phase the task names", ranNoTurn: true, holdsAfterRun: true, prompts: 2},
		{name: "the prompt ran no turn, and the issue left the phase the task names", ranNoTurn: true, holdsAfterRun: false, prompts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			// The issue is in the task's phase while it is delivered; by the time the late turn
			// ends the daemon may have advanced past it.
			holds := true
			h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return holds, nil }
			c := queuedClaim()
			if err := h.store.PutClaim(h.ctx, c); err != nil {
				t.Fatal(err)
			}
			h.start(c)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing, Generation: 7})
			first := h.wantPrompts(1)[0]

			h.advance(testRPC) // the bound passes with no turn: charged, and the id rotated
			if rotated := h.pending().ID; rotated == first.DeliveryID {
				t.Fatal("the delivery id was not rotated when the bound passed")
			}

			h.must(StreamTurnStart{Claim: testToken})
			h.wantState(StateWorking)
			if h.pending().ConfirmedAt.IsZero() {
				t.Fatal("a turn starting after the bound, with the acknowledged prompt still outstanding, confirmed nothing")
			}
			if run := h.claim().ServingRun(); run != 7 {
				t.Fatalf("during the late turn the claim serves run %d, want the task's 7", run)
			}
			holds = tc.holdsAfterRun

			if !tc.ranNoTurn {
				h.must(StreamTurnEnd{Claim: testToken}) // the first prompt's result follows the end
				h.wantPrompts(tc.prompts)
				if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
					t.Fatalf("after the late turn: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
				}
				if lines := h.logs.lines("dropped a stale event", first.DeliveryID); len(lines) != 0 {
					t.Errorf("the first prompt's result was dropped at the fence: %v", lines)
				}
				return
			}

			h.must(StreamPromptResult{Claim: testToken, DeliveryID: first.DeliveryID})
			if p := h.pending(); !p.ConfirmedAt.IsZero() {
				t.Fatalf("pending after the prompt said it ran no turn = %+v, want the confirmation of a turn that was not its own taken back", p)
			}
			h.wantPrompts(1) // nothing goes into the turn that is running
			h.mustQuiet(StreamTurnEnd{Claim: testToken})

			prompts := h.wantPrompts(tc.prompts)
			if tc.holdsAfterRun {
				if resent := prompts[1]; resent.DeliveryID != h.pending().ID || resent.Message != "the task" {
					t.Errorf("re-sent %+v, want the task under the pending id %s", resent, h.pending().ID)
				}
				return
			}
			if h.claim().Pending != nil {
				t.Errorf("the claim still holds %+v after the issue left its phase", h.claim().Pending)
			}
			if _, ok := h.store.delivery(testToken); ok {
				t.Error("the store still holds the delivery after the issue left its phase")
			}
		})
	}
}

// The loop dispatch://INFRA-3276 measured, in the order the daemon's log showed it: Oh My Pi
// acknowledges the task's prompt and spends some thirty seconds in its pre-turn work, the
// daemon's five-second wait for the turn runs out and takes the task back under a new id, the
// sweep's re-send is answered busy — Oh My Pi is preparing that very turn — and takes it back
// under another, and then the first prompt's turn starts. That turn is the task's: its start
// confirms the task, the turn's end waits for the first prompt's result, and that result, naming
// the first id, passes the fence and retires the task. One turn, nothing sent after it, and no
// result dropped as stale; before, the result was dropped and the task sent again at the turn's
// end, which is what kept the loop going until the claim's budget retired it.
func TestALateTurnOfAnOutstandingPromptSettlesItsTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(1)[0].DeliveryID

	h.advance(testRPC)       // the wait for the turn runs out: the task goes back under a new id
	h.observe(runtime.Alive) // and the sweep sends it again
	resend := h.wantPrompts(2)[1].DeliveryID
	if resend == first {
		t.Fatalf("the re-send went under the first id %s, want an id of its own", first)
	}
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: resend, Error: busyWords, Outcome: runtime.OutcomeAgentBusy})
	if p := h.pending(); p.ID == resend || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the busy answer = %+v, want the re-send taken back under an id of its own, unconfirmed", p)
	}

	h.must(StreamTurnStart{Claim: testToken}) // the first prompt's turn, late
	if p := h.pending(); p.ConfirmedAt.IsZero() {
		t.Fatalf("pending in the late turn = %+v, want it confirmed by the first prompt's turn", p)
	}
	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	if h.claim().Pending == nil {
		t.Fatal("the turn's end retired the task before the first prompt's result said the turn was its own")
	}
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: first, AgentInvoked: true})

	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the first prompt's result: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
	if _, ok := h.store.delivery(testToken); ok {
		t.Error("the store still holds the delivery the result retired")
	}
	h.wantPrompts(2)
	h.wantState(StateIdle)
	if starts := h.logs.lines(turnStartedLine); len(starts) != 1 {
		t.Errorf("turns started = %d (%v), want the one late turn", len(starts), starts)
	}
	if lines := h.logs.lines("dropped a stale event"); len(lines) != 0 {
		t.Errorf("an event was dropped as stale: %v", lines)
	}
}

// The first prompt's turn can finish while the resend is still in flight. Its result settles
// the task, so the refusal of the resend cannot take the confirmation back or send it again.
// A provider error in that turn instead keeps work owed to the retry ladder. A result saying
// the first prompt ran no turn leaves the resend free to be acknowledged and run.
func TestTheFirstPromptsResultDecidesTheTaskWhileItsResendIsInFlight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		invoked bool
		status  int
	}{
		{name: "successful turn", invoked: true},
		{name: "provider error", invoked: true, status: 529},
		{name: "no turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			first := h.wantPrompts(1)[0].DeliveryID
			h.advance(testRPC)

			gated := newGatedConn()
			h.conns.Register(testToken, gated)
			// The sweep sends again. Drive stream frames without waiting for the gated send.
			handle := func(ev Event) {
				t.Helper()
				if err := h.m.Handle(h.ctx, ev); err != nil {
					t.Fatalf("handle %T: %v", ev, err)
				}
			}
			handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}})
			resend := testwait.Receive(t, "the resend", gated.entered)
			if resend == first || h.pending().ID != resend {
				t.Fatalf("resend = %s, first = %s, pending = %s; want the resend under a new pending id", resend, first, h.pending().ID)
			}
			if tc.invoked {
				handle(StreamTurnStart{Claim: testToken})
				handle(StreamTurnEnd{Claim: testToken})
				if p := h.pending(); p.ConfirmedAt.IsZero() {
					t.Fatalf("pending after the first prompt's turn ended = %+v, want it confirmed and awaiting the result", p)
				}
				gated.RefusePrompt("the resend was refused")
			}
			result := StreamPromptResult{Claim: testToken, DeliveryID: first, AgentInvoked: tc.invoked, HTTPStatus: tc.status}
			if tc.status != 0 {
				result.Error = "overloaded_error"
			}
			handle(result)
			gated.release <- struct{}{}
			h.m.Wait()

			switch {
			case !tc.invoked:
				if p := h.pending(); p.ID != resend || p.MarkedBy != resend || p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
					t.Fatalf("pending after the first prompt ran no turn = %+v, want the resend %s acknowledged and unconfirmed", p, resend)
				}
			case tc.status != 0:
				if p := h.pending(); p.ID == resend || p.SendFailures != 0 || !p.ConfirmedAt.IsZero() || !h.armed(TimerPromptRetry) {
					t.Fatalf("pending after the provider error and resend refusal = %+v, want the task waiting under a new id on the retry ladder", p)
				}
			default:
				if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
					t.Fatalf("after the first prompt's success and resend refusal: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
				}
				if _, ok := h.store.delivery(testToken); ok {
					t.Fatal("the store still holds the finished task")
				}
				h.conns.Register(testToken, h.conn)
				h.observe(runtime.Alive)
				h.wantPrompts(1)
				h.wantBudgets(Budgets{})
			}
		})
	}
}

// An earlier prompt's no-turn answer takes back the task a turn confirmed once a resend in flight
// has answered too (waitedOutAnswer, sendUnanswered), and a refusal of the resend's prompt answers
// its send as a result does (lateRefused). The order: the first prompt's wait for its turn runs out
// and the sweep sends the task again; a turn of another input's starts with that resend in flight,
// is taken for the task's, and ends; the resend's own answer, read before its acknowledgement, says
// its prompt ran no turn; then the first prompt says the same. Neither turn was the task's. Read as
// still waiting on the resend, the task kept its confirmation with nothing left to come, and settle
// retired it as a finished turn's though no turn of it ran.
func TestAnEarlierPromptsNoTurnAnswerTakesTheTaskBackOnceTheResendHasAnswered(t *testing.T) {
	const refusal = "the model provider refused the request"
	answers := []struct {
		name string
		of   func(prompt string) Event
	}{
		{name: "a result that says it ran no turn", of: func(prompt string) Event {
			return StreamPromptResult{Claim: testToken, DeliveryID: prompt, Outcome: runtime.OutcomeNoTurnRun}
		}},
		{name: "a refusal after its acknowledgement", of: func(prompt string) Event {
			return StreamLateRefusal{Claim: testToken, DeliveryID: prompt, Error: refusal}
		}},
	}
	for _, resend := range answers {
		for _, first := range answers {
			t.Run("the resend: "+resend.name+"; the first prompt: "+first.name, func(t *testing.T) {
				h := newHarness(t)
				h.reach(StateReady)
				gated := newGatedConn()
				h.conns.Register(testToken, gated)
				// Handle, not must: must waits for the send, which the gate is holding.
				handle := func(ev Event) {
					t.Helper()
					if err := h.m.Handle(h.ctx, ev); err != nil {
						t.Fatalf("handle %T: %v", ev, err)
					}
				}
				handle(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
				firstPrompt := testwait.Receive(t, "the first send", gated.entered)
				gated.release <- struct{}{}
				h.m.Wait()
				h.advance(testRPC)
				handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}})
				resent := testwait.Receive(t, "the resend", gated.entered)
				handle(StreamTurnStart{Claim: testToken})
				handle(StreamTurnEnd{Claim: testToken})
				if p := h.pending(); p.ID != resent || p.ConfirmedAt.IsZero() {
					t.Fatalf("pending after the turn's end = %+v, want the resend's task %s confirmed, kept for the answers to come", p, resent)
				}

				handle(resend.of(resent))
				if p := h.pending(); p.ConfirmedAt.IsZero() {
					t.Fatalf("pending after the resend's answer = %+v, want the confirmation kept for the first prompt, still outstanding", p)
				}

				handle(first.of(firstPrompt))
				if c := h.claim(); c.Pending == nil || !c.Pending.ConfirmedAt.IsZero() || c.ServingGeneration == 7 {
					t.Fatalf("after the first prompt's answer: pending %+v serving %d, want the task kept, unconfirmed: no turn of it ran", c.Pending, c.ServingGeneration)
				}

				// The resend's outcome may send the task again: the plain connection takes that
				// send, so no send waits at the gate.
				h.conns.Register(testToken, h.conn)
				gated.release <- struct{}{}
				h.m.Wait()
				if c := h.claim(); c.Pending == nil || !c.Pending.ConfirmedAt.IsZero() || c.ServingGeneration == 7 {
					t.Fatalf("after the resend's outcome: pending %+v serving %d, want the task kept, unconfirmed", c.Pending, c.ServingGeneration)
				}
			})
		}
	}
}

// A turn that starts while the send of the task's prompt is in flight is taken for the task's only
// while that send's answer is still to come (turnAnswers, sendUnanswered). Once the prompt has said
// it ran no turn, by its result or by its refusal after the acknowledgement, read before the
// send's outcome, the answer has taken the task back unconfirmed, under a new id or, where the
// take-back's write fails, under the send's own id, and a turn that starts then is another
// input's. Taken for the task's, it confirmed that task, its entry's handshake and the record of
// the last let-go, and nothing left to come could take them back. So where the agent was in no
// turn, a death in that foreign turn kept the handshake's confirmation and replaced launch 1's
// untold record with its own rather than joining it: the relaunched agent was never told what
// launch 1 ended. Where Oh My Pi's answer was its busy one, the end of the turn it named retired
// the task as served, though no turn of it ran; where the take-back's write failed too, the task
// kept the send's id, and a reading of the pending task's send by its id alone did the same. The
// turn now confirms nothing: the death's record joins launch 1's, and the busy turn's end leaves
// the task to go again once the send's outcome is read.
func TestATurnThatStartsAfterTheSendsPromptRanNoTurnIsNotTheTasks(t *testing.T) {
	const refusal = "the model provider refused the request"
	// Each answer carries the outcome the stream mints from its frame (stream.PromptResult,
	// stream.PromptRefusalOutcome): refused is the one it mints for a provider's refusal with no
	// code, and the busy answer is OutcomeAgentBusy, minted from code session_busy.
	for _, answer := range []struct {
		name    string
		refused runtime.Outcome
		of      func(prompt, why string, o runtime.Outcome) Event
	}{
		{name: "a result that says it ran no turn", refused: runtime.OutcomeNoTurnRun, of: func(prompt, why string, o runtime.Outcome) Event {
			return StreamPromptResult{Claim: testToken, DeliveryID: prompt, Error: why, Outcome: o}
		}},
		{name: "a refusal after its acknowledgement", refused: runtime.OutcomeUnrecognised, of: func(prompt, why string, o runtime.Outcome) Event {
			return StreamLateRefusal{Claim: testToken, DeliveryID: prompt, Error: why, Outcome: o}
		}},
	} {
		t.Run(answer.name+", the agent in no turn, then a death in a turn of another input's", func(t *testing.T) {
			c := queuedClaim()
			c.Generation = 1
			c.EndedWork = EndedWork{At: epoch, Cause: endDisplaced, Generation: 1, Turn: true,
				Background: true, Kinds: []string{"subagent"}, Reported: true}
			h := newHarnessOf(t, c)
			h.reach(StateRegistered)
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
				Phase: phase.Implementing, Generation: 7, Entry: 1})
			gated := newGatedConn()
			h.conns.Register(testToken, gated)
			// Handle, not must: must waits for the send, which the gate is holding.
			handle := func(ev Event) {
				t.Helper()
				if err := h.m.Handle(h.ctx, ev); err != nil {
					t.Fatalf("handle %T: %v", ev, err)
				}
			}
			handle(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})
			sent := testwait.Receive(t, "the ready's send", gated.entered)
			handle(answer.of(sent, refusal, answer.refused))
			unconfirmed := h.claim().Handshake
			if p := h.pending(); p.ID == sent || !p.ConfirmedAt.IsZero() || !unconfirmed.ConfirmedAt.IsZero() {
				t.Fatalf("after the prompt's answer: pending %+v, handshake %+v, want the task taken back under a new id, unconfirmed: this test proves nothing", p, unconfirmed)
			}
			h.wantEndedTold(false, "", "once the prompt said it ran no turn")

			handle(StreamTurnStart{Claim: testToken})
			if p := h.pending(); !p.ConfirmedAt.IsZero() {
				t.Errorf("the turn that started after %s's prompt said it ran no turn confirmed %+v, want it taken for another input's", sent, p)
			}
			cutOff := h.generation()
			handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}})
			gated.release <- struct{}{}
			h.m.Wait()

			for where, got := range map[string]EndedWork{"the machine": h.claim().EndedWork, "the store": h.store.load(h.token).EndedWork} {
				if got.Generation != cutOff || got.Told || !slices.Contains(got.Kinds, "subagent") {
					t.Errorf("%s records the death as %+v, want an untold record of launch %d that joins launch 1's subagent: no prompt told the agent what launch 1 ended", where, got, cutOff)
				}
			}
			h.wantHandshake("after a death in the turn of another input's,", unconfirmed)
		})

		for _, unwritten := range []bool{false, true} {
			name := answer.name + ", Oh My Pi busy, then the turn it named"
			if unwritten {
				name = answer.name + ", Oh My Pi busy and the take-back's write failing, then the turn it named"
			}
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.reach(StateReady)
				gated := newGatedConn()
				h.conns.Register(testToken, gated)
				handle := func(ev Event) {
					t.Helper()
					if err := h.m.Handle(h.ctx, ev); err != nil {
						t.Fatalf("handle %T: %v", ev, err)
					}
				}
				handle(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
				sent := testwait.Receive(t, "the send", gated.entered)
				if unwritten {
					// The take-back's write fails, so the task stays under the send's id
					// (takeBackPending), its confirmation cleared and the claim recorded working.
					h.store.fail("PutDelivery", errBoom)
					if err := h.m.Handle(h.ctx, answer.of(sent, busyWords, runtime.OutcomeAgentBusy)); !errors.Is(err, errBoom) {
						t.Fatalf("the busy answer with the delivery's write failing = %v, want %v: this test proves nothing", err, errBoom)
					}
					h.store.fail("PutDelivery", nil)
				} else {
					handle(answer.of(sent, busyWords, runtime.OutcomeAgentBusy))
				}
				taken := h.pending()
				if state := h.state(); state != StateWorking || (taken.ID == sent) != unwritten || !taken.ConfirmedAt.IsZero() {
					t.Fatalf("after the busy answer the claim is %s holding %+v, want it recorded working for the turn Oh My Pi named and the task taken back unconfirmed, under the send's id %s = %v: this test proves nothing",
						state, taken, sent, unwritten)
				}

				handle(StreamTurnStart{Claim: testToken})
				handle(StreamTurnEnd{Claim: testToken})
				if c := h.claim(); c.Pending == nil || c.Pending.ID != taken.ID || !c.Pending.ConfirmedAt.IsZero() || c.ServingGeneration == 7 {
					t.Fatalf("after the end of the turn the busy answer named: pending %+v serving %d, want %s kept, unconfirmed: no prompt carried it into that turn", c.Pending, c.ServingGeneration, taken.ID)
				}

				// The send's outcome sends the task again: the plain connection takes that send, so
				// no send waits at the gate.
				h.conns.Register(testToken, h.conn)
				gated.release <- struct{}{}
				h.m.Wait()
				if again := h.wantPrompts(1)[0]; again.DeliveryID != taken.ID {
					t.Fatalf("the send's outcome sent %s, want the task again under %s", again.DeliveryID, taken.ID)
				}
			})
		}
	}
}

// dispatch://INFRA-3135's case: a claim held a pending task whose late turn's result was dropped
// at the fence, so the task never settled, and its next task was refused "a delivery is already
// pending" 160 times while every completion was refused HANDOFF_STALE_ASSIGNMENT. The late turn
// now confirms the task it serves, so a newer task of the claim delivered while that turn runs
// takes its place (queue, retirePending): it is queued, not refused ErrDeliveryPending, nothing
// goes into the running turn, and it is sent once the turn ends. The first prompt's result,
// arriving behind that end, names a task the claim no longer holds and decides nothing.
func TestANewerTaskTakesTheLateTurnsTaskPlace(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(1)[0].DeliveryID
	h.advance(testRPC)
	h.observe(runtime.Alive)
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: h.wantPrompts(2)[1].DeliveryID, Error: busyWords, Outcome: runtime.OutcomeAgentBusy})
	h.must(StreamTurnStart{Claim: testToken}) // the first prompt's turn, late

	if err := h.handle(RequestDeliver{Claim: testToken, Task: "the next task", ID: "outbox:2", Generation: 8}); err != nil {
		t.Fatalf("the newer task delivered in the late turn = %v, want it queued (ErrDeliveryPending is %v)", err, errors.Is(err, ErrDeliveryPending))
	}
	if p := h.pending(); p.ID != "outbox:2" || p.Task != "the next task" || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the newer task = %+v, want the newer task waiting in the late task's place", p)
	}
	if c := h.claim(); c.ServingGeneration != 7 {
		t.Fatalf("the claim serves run %d after the late task was replaced, want its 7: the late turn took it", c.ServingGeneration)
	}
	h.wantPrompts(2) // nothing goes into the running turn

	h.must(StreamTurnEnd{Claim: testToken})
	if sent := h.wantPrompts(3)[2]; sent.DeliveryID != "outbox:2" || sent.Message != "the next task" {
		t.Fatalf("sent %+v at the late turn's end, want the newer task", sent)
	}
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: first, AgentInvoked: true})
	if p := h.pending(); p.ID != "outbox:2" || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the late task's result = %+v, want the newer task untouched", p)
	}
	h.wantPrompts(3)
}

// The busy answer of a re-send can be handled before the re-send's own acknowledgement: the answer
// is a frame the stream reads, the acknowledgement the reply the send's goroutine posts, and
// nothing orders the two. The answer takes the re-send back under a new id, so its
// acknowledgement, arriving after, names an id the task has left and is fenced out (fenceOutcome):
// it adds no outstanding prompt, which would be one Oh My Pi never runs and whose result never
// comes. The same answer recorded the claim working for the turn Oh My Pi is preparing
// (recordBusyTurn), so the fenced acknowledgement lets nothing go into it either. The first
// prompt, outstanding all along, is the one the late turn is matched to, and its result still
// settles the task.
func TestABusyAnswerBeforeItsOwnAcknowledgementAddsNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(1)[0].DeliveryID
	h.advance(testRPC)

	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}}); err != nil {
		t.Fatal(err)
	}
	resend := waitFor(t, "the re-send", gated.entered)
	if err := h.m.Handle(h.ctx, StreamPromptResult{Claim: testToken, DeliveryID: resend, Error: busyWords, Outcome: runtime.OutcomeAgentBusy}); err != nil {
		t.Fatal(err)
	}
	if p := h.pending(); p.ID == resend {
		t.Fatalf("pending after the busy answer = %+v, want the re-send taken back under an id of its own", p)
	}
	h.wantState(StateWorking)

	// The re-send's acknowledgement lands now. Fenced out, it adds nothing and sends nothing: the
	// claim the busy answer recorded working is one sendPending holds the task for (fenceOutcome).
	// The gate lets the re-send through and would let any later send through at once, so a send
	// the fence made would be counted rather than held.
	for range 3 {
		gated.release <- struct{}{}
	}
	h.m.Wait()
	if got := h.outstanding(); len(got) != 1 || got[0] != first {
		t.Fatalf("outstanding prompts after the re-send's acknowledgement = %v, want only the first prompt %s: the re-send %s was answered busy",
			got, first, resend)
	}
	if sent := gated.Prompts(); len(sent) != 1 || sent[0].DeliveryID != resend {
		t.Fatalf("prompts on the re-sending connection after its fenced acknowledgement = %+v, want the re-send %s alone", sent, resend)
	}
	h.wantState(StateWorking)

	h.must(StreamTurnStart{Claim: testToken}) // the first prompt's turn, late
	if p := h.pending(); p.ConfirmedAt.IsZero() {
		t.Fatalf("pending in the late turn = %+v, want it confirmed by the first prompt's turn", p)
	}
	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: first, AgentInvoked: true})
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the first prompt's result: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
	if sent := gated.Prompts(); len(sent) != 1 {
		t.Errorf("prompts on the re-sending connection = %+v, want the re-send alone, and none after the task settled", sent)
	}
}

// A guard that this change keeps the bound a silent prompt has and adds none of its own (the
// charge is dispatch://INFRA-2811's): a prompt Oh My Pi acknowledges that never starts a turn and
// never answers is still charged at each acknowledgement, and the task is still sent again at the
// next sweep, so the claim walks its prompt budget to its retirement exactly as before the
// outstanding prompts were kept. Those prompts go with the retired process (letGo). In the same
// walk, a turn that starts late while a prompt of the relaunched process is outstanding is that
// prompt's, and confirms the task.
func TestASilentAcknowledgedPromptStillRetiresTheClaim(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	retiring := h.locator()
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	for charged := 1; charged <= 2; charged++ {
		h.advance(testRPC)
		h.wantBudgets(Budgets{PromptFailures: charged})
		h.observe(runtime.Alive)
	}
	h.wantPrompts(3)
	if got := h.outstanding(); len(got) != 3 {
		t.Fatalf("outstanding prompts = %v, want all three silent prompts kept", got)
	}

	h.advance(testRPC)

	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != retiring {
		t.Errorf("suspended %+v, want the pane that took the prompts", suspend.Locator)
	}
	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{PromptRetires: 1})
	if got := h.outstanding(); len(got) != 0 {
		t.Fatalf("outstanding prompts after the retirement = %v, want none: they went with the retired process", got)
	}

	h.relaunched()
	h.wantPrompts(4)
	h.advance(testRPC)
	h.wantBudgets(Budgets{PromptFailures: 1, PromptRetires: 1})
	h.must(StreamTurnStart{Claim: testToken})
	if p := h.pending(); p.ConfirmedAt.IsZero() {
		t.Fatalf("pending in the late turn = %+v, want it confirmed by the outstanding prompt's turn", p)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the late turn: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
}

func TestAForeignTurnDoesNotConfirmADeliveryNeverSent(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.conns.Remove(testToken)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})

	h.must(StreamTurnStart{Claim: testToken})

	h.wantState(StateWorking)
	if !h.pending().ConfirmedAt.IsZero() {
		t.Fatal("a turn confirmed a delivery that was never sent")
	}
	h.conns.Register(testToken, h.conn)
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)
	if sent := h.wantPrompts(1)[0]; sent.DeliveryID != h.pending().ID {
		t.Errorf("sent %+v at the turn's end, want the queued delivery", sent)
	}
}

func TestADeliveryDuringAForeignTurnIsSentWhenTheTurnEnds(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)

	h.must(RequestDeliver{Claim: testToken, Task: "after this turn"})
	h.wantPrompts(0)

	h.must(StreamTurnEnd{Claim: testToken})
	if sent := h.wantPrompts(1)[0]; sent.Message != "after this turn" {
		t.Errorf("sent %+v, want the task queued during the turn", sent)
	}
}

// A late refusal under a turn that confirmed the delivery: the turn may be foreign and really
// running, so the claim stays working and only the delivery is taken back and rotated. Nothing is
// charged — the agent is in a turn, not refusing to work — and the task is sent again once that
// turn ends.
func TestALateRefusalUnderAConfirmingTurnReversesOnlyTheDelivery(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	refused := h.wantPrompts(1)[0].DeliveryID
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: refused, Error: "agent busy"})

	h.wantState(StateWorking)
	h.wantBudgets(Budgets{})
	p := h.pending()
	if p.ID == refused || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending %+v, want the delivery unconfirmed under a rotated id", p)
	}
	if stored, _ := h.store.delivery(testToken); stored.ID != p.ID || !stored.ConfirmedAt.IsZero() {
		t.Errorf("stored delivery %+v, want the reversal persisted", stored)
	}
	h.wantPrompts(1)

	h.must(StreamTurnEnd{Claim: testToken})

	h.wantState(StateIdle)
	if sent := h.wantPrompts(2)[1]; sent.DeliveryID != p.ID || sent.Message != "the task" {
		t.Errorf("sent %+v after the turn, want the task under the rotated id", sent)
	}
}

// A late refusal while the acknowledged prompt is still waiting for its turn: the no-turn timer
// for the refused id is cancelled, so the prompt it belonged to charges nothing later, and the
// rotated delivery goes as soon as the agent says its own turn is over. The agent's answer that it
// is streaming is the turn's witness, and records the claim working for that turn
// (recordBusyTurn), so nothing sends the task into it. The prompt that carries it is an ordinary
// one: acknowledged and starting no turn, it charges as any other does.
func TestALateRefusalCancelsTheRefusedPromptsTurnTimer(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			refused := h.wantPrompts(base + 1)[base].DeliveryID
			h.conn.SetStreaming(true) // a turn of the agent's own: the collision, not a refusal to work
			h.advance(time.Second)

			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: refused, Error: "agent busy"})

			h.wantState(StateWorking)
			h.wantBudgets(Budgets{})
			rotated := h.pending().ID
			if rotated == refused {
				t.Fatal("the delivery id was not rotated after the late refusal")
			}
			h.advance(testRPC) // the refused prompt's timer would have fired here
			h.wantBudgets(Budgets{})
			if prompts := h.prompts(); len(prompts) != base+1 {
				t.Fatalf("prompts sent = %d, want the refused one alone: the turn the agent is in sends the task at its end", len(prompts))
			}
			if p := h.pending(); p.ID != rotated || !p.ConfirmedAt.IsZero() {
				t.Fatalf("pending = %+v, want the task waiting unconfirmed under the rotated id %s", p, rotated)
			}

			// The turn the agent was in ends, and the task goes under its rotated id. Leaving the
			// refused prompt's timer armed loses it here: that timer confirms the foreign turn as
			// this delivery's and retires the task with it.
			h.must(StreamTurnStart{Claim: testToken})
			h.must(StreamTurnEnd{Claim: testToken})
			if sent := h.wantPrompts(base + 2)[base+1]; sent.DeliveryID != rotated {
				t.Fatalf("sent %+v at the turn's end, want the task under the rotated id %s", sent, rotated)
			}
		})
	}
}

// A late refusal for a turn the agent really is in is what a notice delivered to the pane
// produces: an assignment prompt sent a moment after that notice is refused the same way. That is
// not the agent failing to take work, and charging it let the daemon's own notices walk a healthy
// worker toward the retirement its prompt budget ends in. Two witnesses say the turn is real, and
// either is enough: Oh My Pi's own busy answer, whose code the stream mints as OutcomeAgentBusy, and a
// turn the stream saw start. The task is taken back under a new id, nothing is charged, and it is
// not re-sent on the spot — the turn's end sends it.
func TestALateRefusalIsTheAgentsTurn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		working bool
		refusal string
		outcome runtime.Outcome
	}{
		{name: "Oh My Pi's own answer says so", refusal: busyWords, outcome: runtime.OutcomeAgentBusy},
		// This row's witness is the stream alone, so its refusal says nothing about a turn: with
		// Oh My Pi's busy outcome here too, deleting the stream arm would leave the row passing.
		{name: "the stream saw the turn start", working: true, refusal: "the model provider refused the request", outcome: runtime.OutcomePromptRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			refused := h.wantPrompts(1)[0].DeliveryID
			if tc.working {
				// Oh My Pi's busy answer is not the only witness: a turn the stream saw start is
				// one the daemon knows about without asking.
				h.must(StreamTurnStart{Claim: testToken})
			}

			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: refused, Error: tc.refusal, Outcome: tc.outcome})

			h.wantBudgets(Budgets{})
			rotated := h.pending()
			if rotated.ID == "" || rotated.ID == refused || !rotated.ConfirmedAt.IsZero() {
				t.Fatalf("pending after the refusal = %+v, want the task kept, unconfirmed, under a new id", rotated)
			}
			h.wantPrompts(1) // nothing is re-sent on the spot: the turn's end or the sweep sends it
		})
	}
}

func TestALateRefusalForAnotherDeliveryIsDroppedAndLoggedOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	held := h.pending().ID

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: "delivery-elsewhere", Error: "busy"})
	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: "delivery-elsewhere", Error: "busy"})

	h.wantBudgets(Budgets{})
	if h.pending().ID != held {
		t.Errorf("pending %s, want %s untouched", h.pending().ID, held)
	}
	if lines := h.logs.lines("dropped a stale event", "delivery-elsewhere"); len(lines) != 1 {
		t.Errorf("logged %d times, want once: %v", len(lines), lines)
	}
}

// A prompt refused after a foreign turn already confirmed the send (B1's matching) was never taken:
// the confirmation is taken back. The refusal lands while the daemon's record holds the claim in
// that turn, so it is the agent's busy answer whatever its words or its code (agentRefusalOutcome),
// and is charged nothing. A prompt lost to the transport takes the confirmation back too, at no
// charge — the shim answers the same id again without a second turn if the turn was in fact the
// delivery's.
func TestAFailedSendAfterAConfirmingTurnTakesTheConfirmationBack(t *testing.T) {
	for _, failure := range []struct {
		name    string
		script  func(*gatedConn)
		charged Budgets
	}{
		{"refused", func(g *gatedConn) { g.RefusePrompt("the model provider refused the request") }, Budgets{}},
		{"transport", func(g *gatedConn) { g.FailPrompt(errBoom) }, Budgets{}},
	} {
		t.Run(failure.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			gated := newGatedConn()
			failure.script(gated)
			h.conns.Register(testToken, gated)
			if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
				t.Fatal(err)
			}
			id := waitFor(t, "the send", gated.entered)
			if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
				t.Fatal(err)
			}
			h.wantState(StateWorking)

			gated.release <- struct{}{}
			h.m.Wait()

			h.wantState(StateWorking)
			h.wantBudgets(failure.charged)
			if p := h.pending(); p.ID != id || !p.ConfirmedAt.IsZero() {
				t.Fatalf("pending %+v, want %s unconfirmed", p, id)
			}
			gated.FailPrompt(nil)
			gated.release <- struct{}{}
			h.must(StreamTurnEnd{Claim: testToken})
			if prompts := gated.Prompts(); len(prompts) != 2 || prompts[1].DeliveryID != id {
				t.Errorf("prompts %+v, want the delivery sent again under its own id after the turn", prompts)
			}
			h.wantBudgets(failure.charged)
		})
	}
}

// The same race, and the turn taken for the task's — an Envoy turn that started while the send
// was still adopting the working copy — ends before the send's outcome. The claim is idle with
// the task confirmed, and only the send's outcome says whether the prompt went out at all, so the
// turn's end retires nothing (settle). The send then fails, and its refusal takes the
// confirmation back: the task goes again rather than being retired unsent.
func TestAForeignTurnThatEndsBeforeItsSendFailsRetiresNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	gated.FailPrompt(errBoom)
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	id := waitFor(t, "the send", gated.entered)
	for _, ev := range []Event{StreamTurnStart{Claim: testToken}, StreamTurnEnd{Claim: testToken}} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	if p := h.claim().Pending; p == nil || p.ID != id {
		t.Fatalf("the claim holds %+v after the foreign turn's end, want %s kept while its send is in flight", p, id)
	}

	gated.release <- struct{}{}
	h.m.Wait()

	if p := h.pending(); p.ID != id || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending %+v after the send failed, want %s unconfirmed and owed", p, id)
	}
}

// The same race, and the operator suspends the idle claim before the send's outcome comes. The
// suspension lets the process go and forgets the send, whose outcome is then fenced out, so the
// let-go takes back the confirmation the foreign turn made (letGo): the operator's suspension
// keeps the task, and it is still owed, unconfirmed, for the resume to send.
func TestASuspensionWhileTheSendIsInFlightKeepsTheTaskOwed(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	id := waitFor(t, "the send", gated.entered)
	for _, ev := range []Event{StreamTurnStart{Claim: testToken}, StreamTurnEnd{Claim: testToken}} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}

	if err := h.m.Handle(h.ctx, RequestSuspend{Claim: testToken, By: SuspendedByOperator}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	h.wantState(StateSuspended)
	if p := h.claim().Pending; p == nil || p.ID != id || !p.ConfirmedAt.IsZero() {
		t.Fatalf("after the suspension the claim holds %+v, want %s owed and unconfirmed", p, id)
	}
	if stored := h.store.load(h.token).Pending; stored == nil || stored.ID != id || !stored.ConfirmedAt.IsZero() {
		t.Fatalf("the store holds %+v after the suspension, want %s owed and unconfirmed", stored, id)
	}
	gated.release <- struct{}{}
	h.m.Wait()
}

// The same suspension, and the store will not take the delivery's write. The take-back and the
// suspended claim are one write (persist), so the store never holds a suspended claim beside a
// delivery confirmed by a turn that may have been another input's: a restart would read that task
// as served and the resume would send nothing. Written apart, the claim's write landed and the
// delivery's failed.
func TestASuspensionsTakeBackOfAConfirmationIsOneWriteWithTheClaim(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the send", gated.entered)
	for _, ev := range []Event{StreamTurnStart{Claim: testToken}, StreamTurnEnd{Claim: testToken}} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	h.store.fail("PutDelivery", errBoom)

	_ = h.m.Handle(h.ctx, RequestSuspend{Claim: testToken, By: SuspendedByOperator})

	wantNoSuspendedConfirmed := func(after string) {
		t.Helper()
		stored := h.store.load(h.token)
		if stored.State == StateSuspended && stored.Pending != nil && !stored.Pending.ConfirmedAt.IsZero() {
			t.Fatalf("after %s the store holds the claim suspended beside %+v, a delivery still confirmed: the take-back was not written with the claim", after, stored.Pending)
		}
	}
	wantNoSuspendedConfirmed("the suspension")

	// A write of the claim no decision makes — the mark that an open update round was told —
	// writes the claim row directly (markRound), and carries the take-back all the same.
	const round = "pod:test-round"
	h.m.mu.Lock()
	h.m.claim.UpdateRound = round
	h.m.mu.Unlock()
	_, _ = h.m.PodUpdateTold(h.ctx, round)
	wantNoSuspendedConfirmed("the round's told mark")
	h.store.fail("PutDelivery", nil)
	gated.release <- struct{}{}
	h.m.Wait()
}

// The send's acknowledgement is still on its way when the turn it started ends and the prompt's
// successful result comes. The result says a turn ran the prompt and marks the send answered
// (sending.resulted), so the task is retired then, and an operator's suspension before the
// acknowledgement takes back nothing and sends nothing again.
func TestAPromptsResultEndsTheDoubtItsSendInFlightLeft(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	id := waitFor(t, "the send", gated.entered)
	for _, ev := range []Event{
		StreamTurnStart{Claim: testToken},
		StreamTurnEnd{Claim: testToken},
		StreamPromptResult{Claim: testToken, DeliveryID: id, AgentInvoked: true},
	} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the prompt's result: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}

	if err := h.m.Handle(h.ctx, RequestSuspend{Claim: testToken, By: SuspendedByOperator}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	gated.release <- struct{}{}
	h.m.Wait()
	if p := h.claim().Pending; p != nil {
		t.Fatalf("after the suspension the claim holds %+v, want nothing: the task's prompt ran", p)
	}
	if prompts := gated.Prompts(); len(prompts) != 1 {
		t.Fatalf("the task was prompted %d times, %+v, want once", len(prompts), prompts)
	}
}

// A successful result of the task can end the doubt only where another send is still unanswered:
// the task's own send is marked answered with its result (sending.resulted), so ranTask is needed
// only when the in-flight send is under another id (confirmationInDoubt).
func TestARanTaskResultEndsDoubtOfAnotherSend(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	h.m.claim.Pending = &Delivery{ID: "task", ConfirmedAt: h.clock.Now()}
	h.m.send = &sending{id: "another"}
	h.m.ranTask = "task"
	if h.m.confirmationInDoubt() {
		t.Fatal("a successful result of task ended no doubt of another unanswered send")
	}
	h.m.ranTask = ""
	if !h.m.confirmationInDoubt() {
		t.Fatal("without the successful result of task, another unanswered send leaves the confirmation in doubt")
	}
}

// The result that completes the task can be an earlier prompt's: the wait for that prompt's turn
// ran out and moved the task to a new id, the prompt stayed outstanding, and the sweep's send of
// the new id is still adopting the working copy when the old prompt's turn runs and its result
// comes. That result completes the task under the id it holds now (Machine.ranTask), so the task
// is retired then, and the new send failing afterwards takes nothing back and sends nothing again.
func TestAnEarlierPromptsResultEndsTheDoubtALaterSendLeft(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	first := waitFor(t, "the first send", gated.entered)
	gated.release <- struct{}{}
	h.m.Wait()
	h.advance(2 * testRPC) // the first prompt's turn does not start in time: the task moves to a new id
	if p := h.pending(); p.ID == first {
		t.Fatalf("the task kept the id %s its waited-out prompt carried", first)
	}
	gated.FailPrompt(errBoom)
	if err := h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	second := waitFor(t, "the sweep's send of the task under its new id", gated.entered)

	for _, ev := range []Event{
		StreamTurnStart{Claim: testToken},
		StreamTurnEnd{Claim: testToken},
		StreamPromptResult{Claim: testToken, DeliveryID: first, AgentInvoked: true},
	} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the first prompt's result: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}

	gated.release <- struct{}{}
	h.m.Wait()
	if p := h.claim().Pending; p != nil {
		t.Fatalf("after the send of %s failed the claim holds %+v, want nothing: the task's turn ran", second, p)
	}
}

// An earlier prompt's turn ends in a provider error, whose take-back's write the store does not
// take, so the task keeps the id the later send carries. The errored result marks that later send
// answered (sending.resulted), so an Envoy turn that starts after it confirms nothing: neither the
// send nor a prompt still says that turn is the task's. The resend under the same id is the task's
// next chance to run. TestTheFirstPromptsResultDecidesTheTaskWhileItsResendIsInFlight covers the
// turn-reporting result at line 870.
func TestAnEarlierPromptsErrorEndsNoDoubtALaterSendLeft(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	first := waitFor(t, "the first send", gated.entered)
	gated.release <- struct{}{}
	h.m.Wait()
	h.advance(2 * testRPC) // the first prompt's turn does not start in time: the task moves to a new id
	if err := h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	second := waitFor(t, "the sweep's send of the task under its new id", gated.entered)

	for _, ev := range []Event{StreamTurnStart{Claim: testToken}, StreamTurnEnd{Claim: testToken}} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	h.store.fail("PutDelivery", errBoom)
	_ = h.m.Handle(h.ctx, StreamPromptResult{Claim: testToken, DeliveryID: first, AgentInvoked: true, Error: "overloaded_error", HTTPStatus: 529})
	h.store.fail("PutDelivery", nil)
	if p := h.pending(); p.ID != second || !p.ConfirmedAt.IsZero() {
		t.Fatalf("after the errored result the claim holds %+v, want %s owed and unconfirmed", p, second)
	}

	for _, ev := range []Event{StreamTurnStart{Claim: testToken}, StreamTurnEnd{Claim: testToken}} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	if p := h.claim().Pending; p == nil || p.ID != second || !p.ConfirmedAt.IsZero() {
		t.Fatalf("after the Envoy turn the claim holds %+v, want %s owed and unconfirmed: the turn confirms nothing", p, second)
	}

	gated.release <- struct{}{} // release the send whose outcome is moot
	resend := waitFor(t, "the re-send of the task under the same id", gated.entered)
	if resend != second {
		t.Fatalf("the re-send id is %s, want %s", resend, second)
	}
	gated.FailPrompt(nil)
	gated.release <- struct{}{}
	h.m.Wait()
	if p := h.claim().Pending; p == nil || p.ID != second || !p.ConfirmedAt.IsZero() {
		t.Fatalf("after the re-send's acknowledgement the claim holds %+v, want %s owed and unconfirmed", p, second)
	}
}

// A prompt's turn ends in a provider error, and the store does not take the write of the
// take-back that moves the task to a new id, so the task keeps the id that prompt carried. The
// shim answers a prompt it has a record of from that record, without a second turn, so a retry
// under that id would be answered by the turn that already ended. The next send takes the task
// back again first, and goes under the new id (sendPending).
func TestARetryWhoseTakeBackWriteFailedGoesUnderAFreshID(t *testing.T) {
	h := workingClaim(t)
	first := h.pending().ID
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.store.fail("PutDelivery", errBoom)
	_ = h.m.Handle(h.ctx, StreamPromptResult{Claim: h.token, DeliveryID: first, AgentInvoked: true, Error: "overloaded_error", HTTPStatus: 529})
	h.store.fail("PutDelivery", nil)
	if p := h.pending(); p.ID != first {
		t.Fatalf("after the take-back's write failed the claim holds %+v, want the task kept under %s", p, first)
	}
	h.wantPrompts(1)

	if err := h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	h.m.Wait()

	prompts := h.wantPrompts(2)
	if prompts[1].DeliveryID == first {
		t.Fatalf("the retry went under %s, the id whose prompt's result already came", first)
	}
	if p := h.pending(); p.ID != prompts[1].DeliveryID {
		t.Fatalf("the claim holds %+v, want the task under the id the retry carried, %s", p, prompts[1].DeliveryID)
	}
}

func TestPromptOutcomesAndTimersForAStaleDeliveryAreDropped(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	held := h.pending()

	for _, ev := range []Event{
		PromptAcked{Claim: testToken, Generation: 1, DeliveryID: "delivery-elsewhere"},
		PromptRefused{Claim: testToken, Generation: 1, DeliveryID: "delivery-elsewhere", Err: errBoom},
		PromptAcked{Claim: testToken, Generation: 0, DeliveryID: held.ID},
		Timer{Claim: testToken, Kind: TimerTurn, Generation: 1, Seq: 9999, DeliveryID: held.ID},
	} {
		h.must(ev)
	}

	h.wantBudgets(Budgets{})
	h.wantState(StateReady)
	if p := h.pending(); p.ID != held.ID || !p.DeliveredAt.Equal(held.DeliveredAt) {
		t.Errorf("pending %+v, want %+v untouched", p, held)
	}
	if lines := h.logs.lines("dropped a stale event"); len(lines) != 4 {
		t.Errorf("logged %d stale events, want 4: %v", len(lines), lines)
	}
}

// The turn ended and the claim was written idle, but the delivery's retirement never landed. The
// confirmed delivery a restart reads back is finished, and does not stand in the way of the next.
func TestAConfirmedDeliveryLeftBehindByACrashIsRetiredBeforeTheNextTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.store.fail("RetireDelivery", errBoom)
	if err := h.handle(StreamTurnEnd{Claim: testToken}); !errors.Is(err, errBoom) {
		t.Fatalf("turn end over a failing retirement returned %v, want the store's error", err)
	}
	if stored := h.store.load(testToken); stored.State != StateIdle || stored.Pending == nil || stored.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("stored %+v, want an idle claim still holding its confirmed delivery", stored)
	}
	h.store.fail("RetireDelivery", nil)
	h.restart()

	h.must(RequestDeliver{Claim: testToken, Task: "the next task"})

	if sent := h.wantPrompts(2)[1]; sent.Message != "the next task" {
		t.Errorf("sent %+v, want the next task", sent)
	}
}

// A connection drops under a send and the shim reconnects before the send's failure is handled:
// the hello that triggers a re-send came while the send was still out, and still triggers it.
func TestAHelloDuringAFailingSendTriggersTheResend(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	gated.FailPrompt(errBoom)
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	id := waitFor(t, "the send", gated.entered)
	if err := h.m.Handle(h.ctx, StreamHello{Claim: testToken, Generation: h.generation()}); err != nil {
		t.Fatal(err)
	}

	gated.release <- struct{}{} // the first send fails
	resent := waitFor(t, "the re-send", gated.entered)
	gated.FailPrompt(nil)
	gated.release <- struct{}{}
	h.m.Wait()

	if resent != id || len(gated.Prompts()) != 2 {
		t.Fatalf("re-sent %s after prompts %+v, want the delivery sent again under its own id %s", resent, gated.Prompts(), id)
	}
	h.wantBudgets(Budgets{})
}

// The daemon saw a turn start and went down before it saw the turn end. After the restart the
// claim reads as working; the shim's reconnect is when the machine asks, and a turn that is over
// is ended — the claim does not wait in working for an agent_end it already missed.
func TestAfterARestartATurnThatEndedMeanwhileIsEnded(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.restart()

	h.connect()

	h.wantState(StateIdle)
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

	if pending := h.pending(); pending.ID != "outbox:42" || pending.ID != first.ID {
		t.Fatalf("pending delivery = %#v, want the original outbox delivery", pending)
	}
	if prompts := h.wantPrompts(1); prompts[0].DeliveryID != "outbox:42" {
		t.Fatalf("prompt = %#v, want outbox delivery id", prompts[0])
	}
}

// Dropping a task for its phase clears the claim's pending delivery, and a restored claim also
// carries the daemon's standing question for its agent: whether the turn it may be in is the
// pending delivery's. Leaving that question armed with nothing pending made ready-with-no-task
// reachable for the first time, and the next task then took the agent's own foreign turn as its
// own — confirmed, never prompted, and retired at that turn's end, so the phase stalled with
// nothing sent and the outbox row already finished.
func TestDroppingATaskForItsPhaseAlsoDropsTheQuestionARestartLeft(t *testing.T) {
	restored := fixture(StateReady)
	restored.Pending.Phase = phase.Implementing
	h := newHarnessOf(t, restored)
	if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	h.conns.Register(testToken, h.conn)
	h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return false, nil }
	h.start(restored)

	// The issue has left the phase the restored task was queued for: it is dropped.
	h.must(RequestReady{Claim: testToken, Generation: restored.Generation, Session: session})
	if p := h.m.Claim().Pending; p != nil {
		t.Fatalf("the finished phase's task is still pending as %+v", p)
	}

	// The agent is in a turn of its own, and an operator delivers a task of no phase.
	h.conn.SetStreaming(true)
	h.must(RequestDeliver{Claim: testToken, Task: "look at the tree"})

	sent := h.wantPrompts(1)
	if sent[0].Message != "look at the tree" {
		t.Errorf("prompt = %+v, want the operator's task", sent[0])
	}
	if p := h.m.Claim().Pending; p == nil || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending = %+v, want the operator's task unconfirmed: the agent's own turn is not its delivery's", p)
	}
}

// Dropping a restored claim's task for its phase must answer the question the restart left, not
// discard it: its agent may still be mid-turn. Clearing the question outright left the claim ready
// while the agent worked, so the next task was prompted into a busy agent, charged, and the worker
// relaunched mid-turn. Asked instead, the claim moves to working and the next task waits for the
// turn to end.
func TestDroppingATaskAsksWhetherTheAgentIsStillInATurn(t *testing.T) {
	restored := fixture(StateReady)
	restored.Pending.Phase = phase.Implementing
	h := newHarnessOf(t, restored)
	if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	h.conns.Register(testToken, h.conn)
	h.conn.SetStreaming(true) // the agent is in a turn an earlier daemon saw start
	h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return false, nil }
	h.start(restored)

	h.must(RequestReady{Claim: testToken, Generation: restored.Generation, Session: session})

	if p := h.m.Claim().Pending; p != nil {
		t.Fatalf("the finished phase's task is still pending as %+v", p)
	}
	h.wantState(StateWorking)

	// A task delivered while that turn runs waits for it rather than being prompted into it.
	h.must(RequestDeliver{Claim: testToken, Task: "look at the tree"})
	h.wantPrompts(0)
	h.wantBudgets(Budgets{})

	h.must(StreamTurnEnd{Claim: testToken})

	if sent := h.wantPrompts(1)[0]; sent.Message != "look at the tree" {
		t.Errorf("prompt after the turn = %+v, want the operator's task", sent)
	}
}

// A restored working claim asks its agent whether the turn an earlier daemon saw start is still
// running. An ask that fails (a get_state the agent did not answer in time) says nothing about the
// turn: the claim stays working and its task stays the turn's, and the next sweep asks again. Taken
// as "not streaming", it ended a turn still running and set the claim idle, and the sweep then sent
// the task into that busy agent every interval.
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
	if p := h.claim().Pending; p == nil || p.ID != restored.Pending.ID || p.ConfirmedAt.IsZero() {
		t.Fatalf("after the failed ask pending = %+v, want the turn's confirmed task kept", p)
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

	// The agent answers on a later sweep: the turn is over, and it ends now.
	h.conn.FailGetState(nil)
	h.conn.SetStreaming(false)
	h.observe(runtime.Alive)
	h.wantState(StateIdle)
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
// is delivered when the claim resumes.
func TestAnOperatorsTaskSurvivesASuspensionAndGoesOnResume(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered) // registered, not ready: the task is queued rather than sent
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

	h.must(RequestResume{Claim: testToken, By: SuspendedByOperator})
	h.reach(StateReady)

	if sent := h.wantPrompts(1)[0]; sent.Message != "look at the tree" {
		t.Errorf("prompt after the resume = %+v, want the operator's task", sent)
	}
}

// An agent that cannot start a turn at all answers every prompt with a late refusal: Oh My Pi
// acknowledges the prompt and then rejects it, and a provider failure thrown before the turn
// begins ("No API key found for …") arrives by that same route. Resending on the spot and
// charging nothing made that an unbounded loop — the worker was never relaunched, failed or
// held, and the prompt budget that exists to end it never moved. A refusal with no turn of the
// agent's own running is the agent refusing to work: it is charged, and the retry waits for the
// sweep that follows the charge.
func TestAPersistentLateRefusalIsChargedAndNotResentOnTheSpot(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	refused := h.wantPrompts(1)[0].DeliveryID
	h.conn.SetStreaming(false) // no turn of its own: the agent could not start one

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: refused, Error: "No API key found for anthropic."})

	h.wantBudgets(Budgets{PromptFailures: 1})
	if prompts := h.conn.Prompts(); len(prompts) != 1 {
		t.Fatalf("prompts = %d, want the refused one alone: the retry waits for the sweep", len(prompts))
	}
	rotated := h.pending()
	if rotated.ID == refused || !rotated.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the refusal = %+v, want the task kept, unconfirmed, under a new id", rotated)
	}

	// The sweep that follows sends it again, once, under the rotated id. The refused prompt's own
	// timer must be gone by then: left armed, it charges a second failure for a prompt nobody is
	// waiting on, and confirms whatever turn runs next as this delivery's.
	h.advance(testRPC)
	h.observe(runtime.Alive)
	if sent := h.wantPrompts(2)[1]; sent.DeliveryID != rotated.ID {
		t.Fatalf("re-sent %+v at the sweep, want the task under the rotated id %s", sent, rotated.ID)
	}
	h.wantBudgets(Budgets{PromptFailures: 1})
}

// Confirming a turn writes the claim and the delivery, and a daemon can die between them. The
// delivery is what the turn's end retires and what a re-send is fenced on, so a claim recorded as
// working with its delivery unwritten is a task the shim already answered and nothing will send
// again: the worker is never prompted and the phase waits. The two writes go together.
func TestAConfirmationDoesNotSurviveHalfWritten(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	base := len(h.prompts())
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	sent := h.wantPrompts(base + 1)[base]

	h.store.fail("PutDelivery", errBoom)
	err := h.m.Handle(context.Background(), StreamTurnStart{Claim: testToken, DeliveryID: sent.DeliveryID})
	if err == nil {
		t.Fatal("the turn start reported success with the delivery write failing")
	}

	stored := h.store.load(testToken)
	if stored.State == StateWorking && (stored.Pending == nil || stored.Pending.ConfirmedAt.IsZero()) {
		t.Fatalf("the store holds a working claim whose delivery is unconfirmed: %+v, pending %+v",
			stored.State, stored.Pending)
	}
}

// A root architect's task of a run is held, never dropped, while its root has nobody to hear it
// (Deps.RootHears): such a task names no phase, so nothing else would keep it from being prompted
// to an architect whose tree is parked, lingering or waiting for its slot, and the decisions it
// names are told when the turn that takes it starts. It stays pending, uncharged, and the next deliver
// request — the start of the root's next run, which the pending task refuses — is what sends it.
// A task of no run is nobody's to hold: an operator's own is prompted whatever the root is doing.
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
	if p := h.pending(); p.Task != "a person decided on the tree" || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending delivery = %+v, want the held task, unsent and unconfirmed", p)
	}
	h.wantBudgets(Budgets{})
	if asked == 0 {
		t.Fatal("the held task was never put to RootHears")
	}

	// The root runs again: its start row is refused by the held task, and that refusal is what
	// sends the held task on.
	hears = true
	err := h.handle(RequestDeliver{Claim: rootToken, Task: "the next run's task", Generation: 3, Row: 11})
	if !errors.Is(err, ErrDeliveryPending) {
		t.Fatalf("the next run's deliver = %v, want it refused by the pending task", err)
	}
	sent := h.wantPrompts(1)[0]
	if sent.Message != "a person decided on the tree" {
		t.Fatalf("prompt = %q, want the task held through the park", sent.Message)
	}
}

// An operator's own task names no run, so the hold never applies to it: it is prompted while the
// root has no architect to hear a task of a run.
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
		t.Fatalf("prompt = %q, want the operator's task sent", sent.Message)
	}
}
