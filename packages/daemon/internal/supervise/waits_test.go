package supervise

// R5's two waits (waits.go): the first bound over a value the worker saw and did not take, or
// that nobody reported seeing; the second over an accepted take whose turn did not start; the
// count a turn resets and a take does not; the holds that charge nobody; and that the time a
// daemon was not serving counts toward neither bound (dispatch://INFRA-2811),
// and a restored claim whose shim never comes back meets a reconnect deadline.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The two bounds at the daemon's defaults (worker_take_timeout_seconds,
// worker_turn_start_timeout_seconds).
const (
	takeBound      = 300 * time.Second
	turnStartBound = 1500 * time.Second
)

// withWaits sets R5's two bounds on deps, as the daemon sets them from its configuration.
func withWaits(d *Deps) { d.Timeouts.Take, d.Timeouts.TurnStart = takeBound, turnStartBound }

// waitsHarness is newHarness with R5's two bounds set, and whatever else set changes of the
// machine's deps before it is built.
func waitsHarness(t *testing.T, set ...func(*Deps)) *harness {
	t.Helper()
	return newHarnessOf(t, queuedClaim(), append([]func(*Deps){withWaits}, set...)...)
}

// charges is the why of every charge the machine made, in order: each prompt failure, and each
// retirement a charge at the limit made.
func (h *harness) charges() []string {
	var whys []string
	for _, r := range h.records.ofAny(promptFailed, retiredAfterLimit) {
		why, _ := r.attr("why")
		whys = append(whys, why.String())
	}
	return whys
}

// wantCharges is charges, failing the test unless each charge's why begins with the wait named
// at its place.
func (h *harness) wantCharges(waits ...string) {
	h.t.Helper()
	whys := h.charges()
	if len(whys) != len(waits) {
		h.t.Fatalf("charges %q, want %d naming %q", whys, len(waits), waits)
	}
	for i, wait := range waits {
		if !strings.HasPrefix(whys[i], wait+":") {
			h.t.Fatalf("charge %d's why is %q, want it to name the %s wait", i+1, whys[i], wait)
		}
	}
}

// A worker whose turn starts 30 seconds after its accepted take — the setup a harness takes
// before it runs a turn — is not charged: the second bound is the setup's, and 30 seconds is
// well inside it. Its turn and what follows charge nothing either.
func TestChargeNoneForATurnStarting30sAfterItsTake(t *testing.T) {
	h := waitsHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	if p := h.pending(); !p.awaitsTurn() {
		t.Fatalf("pending %+v, want the worker's take accepted", p)
	}

	h.advance(30 * time.Second)
	h.must(StreamTurnStart{Claim: testToken})
	h.advance(2 * turnStartBound)
	h.must(StreamTurnEnd{Claim: testToken})
	h.advance(2 * turnStartBound)

	h.wantCharges()
	h.wantBudgets(Budgets{})
	h.wantPrompts(1)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v, want the task finished by its turn", p)
	}
}

// A ready, idle worker under no hold that reported its value seen and asks no take of it is
// charged once the first bound passes, naming the take wait, and not a moment sooner; nothing is
// written again for it, since the worker holds the value.
func TestChargeASeenUntakenValueAfterTheFirstBound(t *testing.T) {
	h := waitsHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.seen()

	h.advance(takeBound - time.Second)
	h.wantCharges()
	h.advance(time.Second)

	h.wantCharges(WaitTake)
	h.wantBudgets(Budgets{PromptFailures: 1})
	if stored := h.store.load(testToken).Budgets; stored != (Budgets{PromptFailures: 1}) {
		t.Fatalf("stored budgets %+v, want the charge written", stored)
	}
	h.wantState(StateReady)
	if p := h.pending(); p.taken() {
		t.Fatalf("pending %+v, want the task still waiting for its take", p)
	}
	if written := h.values.written(); len(written) != 1 {
		t.Fatalf("values written %+v, want the queue's one: a value the worker saw is not written again", written)
	}
}

// A value nobody reported seeing charges nobody: the relay failed, not the worker. With the
// relaying listener stopped while the process stays up (the sweep keeps finding it alive), an
// idle worker owed a task is not charged across three first bounds; its value is written again
// once per bound, under the same number, and the claim is listed as unseen, dated at the latest
// write. A seen report, once the relay is back, ends the listing, and the worker takes once.
func TestSilentListenerChargesNobodyAcrossThreeBounds(t *testing.T) {
	h := waitsHarness(t)
	h.reach(StateReady)
	h.worker.relayDown = true
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	number := h.pending().Number
	if listed := h.m.UnseenTaskValue(); listed != nil {
		t.Fatalf("the claim is listed unseen as %+v before a bound has passed", listed)
	}

	for bound := 1; bound <= 3; bound++ {
		h.advance(takeBound)
		h.observe(runtime.Alive)

		written := h.values.written()
		if len(written) != 1+bound || written[bound].Number != number {
			t.Fatalf("after bound %d the values written are %+v, want task %d written %d times", bound, written, number, 1+bound)
		}
		listed := h.m.UnseenTaskValue()
		if listed == nil || listed.Claim != testToken || listed.Issue != "LEGION-209" || listed.Role != claim.RoleImplementer ||
			listed.Task != number || !listed.WrittenAt.Equal(h.clock.Now()) {
			t.Fatalf("after bound %d the claim is listed %+v, want it unseen with task %d written at %s", bound, listed, number, h.clock.Now())
		}
	}
	h.wantCharges()
	h.wantBudgets(Budgets{})

	h.worker.relayDown = false
	h.observe(runtime.Alive)

	if listed := h.m.UnseenTaskValue(); listed != nil {
		t.Fatalf("the claim is still listed unseen as %+v after its worker reported the value seen", listed)
	}
	h.wantPrompts(1)
	h.wantCharges()
}

// A hold's end writes the value again for a worker whose take it refused; with the relaying
// listener stopped across that end the new revision reaches nobody, which charges nobody and is
// written again at the first bound. Once a rewrite reaches the worker it asks exactly once, and
// that take is accepted.
func TestSilentListenerAcrossAHoldsEndAsksOnce(t *testing.T) {
	inTurn := true
	h := waitsHarness(t, func(d *Deps) {
		d.Identity = adoptingIdentity
		d.IssueInTurn = func(string, claim.Token) (claim.Token, bool) { return testerToken, inTurn }
	})
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
	if answers := h.worker.answers; len(answers) != 1 || answers[0].Hold != HoldIssueInTurn {
		t.Fatalf("the worker's takes were answered %+v, want one refused by %q", answers, HoldIssueInTurn)
	}
	// A take the hold refused is waited on by no bound: the hold's end writes the value again.
	h.advance(takeBound)
	h.wantCharges()

	h.worker.relayDown = true
	inTurn = false
	h.must(IssueTurnEnded{Claim: testToken})
	if written := h.values.written(); len(written) != 2 {
		t.Fatalf("values written %+v, want the hold's end to write the value again", written)
	}
	h.advance(takeBound)
	if written := h.values.written(); len(written) != 3 {
		t.Fatalf("values written %+v, want the revision nobody saw written again at the first bound", written)
	}
	h.wantCharges()

	h.worker.relayDown = false
	h.observe(runtime.Alive)
	h.advance(3 * takeBound)

	h.wantCharges()
	if answers := h.worker.answers; len(answers) != 2 || answers[1].Outcome != TakeOutcomeAccepted {
		t.Fatalf("the worker's takes were answered %+v, want one more take after the hold's end, accepted", answers)
	}
	h.wantPrompts(1)
	h.wantCalls("AdoptWorkingCopy", 1)
}

// A take refused because the claim records a session it does not hold (session-not-held) charges
// the worker nothing and starts neither bound: what is missing is the daemon's hold of the session,
// not anything of the worker's. The hold ends by a write, as every hold does: once the store holds
// the session for the claim again, the sweep's decision writes the value again under the cause
// hold-ended, and the worker asks once more, and that take is accepted, exactly once.
func TestASessionNotHeldTakeChargesNothingAndItsEndWritesTheValueAgain(t *testing.T) {
	h := waitsHarness(t)
	h.reach(StateReady)
	// A restarted machine admits no name of its session yet (Machine.heldNames), so its next take
	// asks the store, which now holds the session id for another claim.
	h.store.setHold(sessionID(session), claim.Token("legion-legion-LEGION-209-tester"))
	h.restart()
	h.connect()
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	if answers := h.worker.answers; len(answers) != 1 || answers[0].Hold != HoldSessionNotHeld {
		t.Fatalf("the worker's takes were answered %+v, want one refused by %q", answers, HoldSessionNotHeld)
	}

	h.advance(3 * turnStartBound)
	h.wantCharges()
	h.wantBudgets(Budgets{})
	h.wantPrompts(0)
	if p := h.pending(); p.taken() {
		t.Fatalf("pending %+v while the claim does not hold its session, want it untaken", p)
	}
	written := len(h.values.written())

	h.store.setHold(sessionID(session), testToken)
	h.observe(runtime.Alive)
	if got := h.values.written(); len(got) != written+1 {
		t.Fatalf("values written %+v, want the hold's end to write the value once more", got)
	}
	lines := h.records.of(valueWrittenLine)
	if cause, _ := lines[len(lines)-1].attr("cause"); cause.String() != TaskValueHoldEnded {
		t.Fatalf("the value was written again under cause %q, want %q", cause, TaskValueHoldEnded)
	}
	h.advance(3 * takeBound)

	h.wantCharges()
	if answers := h.worker.answers; len(answers) != 2 || answers[1].Outcome != TakeOutcomeAccepted {
		t.Fatalf("the worker's takes were answered %+v, want one more take after the hold's end, accepted", answers)
	}
	h.wantPrompts(1)
}

// A worker whose accepted take starts no turn within the second bound is charged, naming the
// turn-start wait, and the task is taken back under its number and id: the next take of it is
// decided afresh, and its value is written again under the same number so the worker can ask.
func TestChargeATakeWithNoTurnWithinTheSecondBoundTakesItBack(t *testing.T) {
	h := waitsHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.mustTake()
	taken := h.pending()

	h.advance(turnStartBound - time.Second)
	h.wantCharges()
	h.advance(time.Second)

	h.wantCharges(WaitTurnStart)
	h.wantBudgets(Budgets{PromptFailures: 1})
	p := h.pending()
	if p.taken() || p.Number != taken.Number || p.ID != taken.ID {
		t.Fatalf("pending %+v after the second bound, want task %d taken back under its id", p, taken.Number)
	}
	if stored, _ := h.store.delivery(testToken); stored.taken() {
		t.Fatalf("the stored delivery %+v is still taken, want the take-back written", stored)
	}
	written := h.values.written()
	if len(written) != 2 || written[1].Number != taken.Number {
		t.Fatalf("values written %+v, want task %d written again under its number", written, taken.Number)
	}
	if c := h.claim(); c.TaskWritten != taken.Number || c.TaskWrittenRevision.Revision != written[1].Revision {
		t.Fatalf("the claim records written %d at revision %d, want %d at the rewrite's %d",
			c.TaskWritten, c.TaskWrittenRevision.Revision, taken.Number, written[1].Revision)
	}
	if back := h.records.of(takenBackLiteral); len(back) != 1 {
		t.Fatalf("logged %d take-backs, want one", len(back))
	} else if cause, _ := back[0].attr("cause"); cause.String() != WaitTurnStart {
		t.Fatalf("the take-back names cause %q, want %q", cause, WaitTurnStart)
	}

	if text := h.mustTake(); text != "the task" {
		t.Fatalf("the take after the take-back was answered %q, want the task", text)
	}
}

// The third charge retires the process and relaunches the same session, even for a worker that
// takes again after every charge: a take resets no count, so taking and never starting is still
// retired.
func TestChargeTheThirdRetiresEvenWhenItTakesAfterEach(t *testing.T) {
	h := waitsHarness(t)
	h.reach(StateReady)
	retiring := h.locator()
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	for charge := 1; charge < testLimits().PromptFailures; charge++ {
		h.advance(turnStartBound)
		h.wantBudgets(Budgets{PromptFailures: charge})
		if p := h.pending(); !p.awaitsTurn() {
			t.Fatalf("pending %+v after charge %d, want the worker to have taken it again", p, charge)
		}
	}
	h.wantPrompts(testLimits().PromptFailures)

	h.advance(turnStartBound)

	h.wantCharges(WaitTurnStart, WaitTurnStart, WaitTurnStart)
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != retiring {
		t.Fatalf("suspended %+v, want the process that took and never started", suspend.Locator)
	}
	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{PromptRetires: 1})
	if p := h.pending(); p.taken() {
		t.Fatalf("pending %+v, want the task taken back for the relaunched worker", p)
	}
}

// A worker under a hold is not charged while the hold stands. A hold that arises while the first
// wait is armed — another role of the issue starts a turn, which the sweep's next decision reads
// — disarms it; its end arms the wait again from there, so the charge comes a whole bound after
// the end and no sooner. A hold the bound itself finds — a root whose tree has no architect to
// hear its task, which only a take asks the record about — charges nobody either.
func TestChargeNoneWhileAHoldStands(t *testing.T) {
	t.Run("a hold that arises while the wait is armed", func(t *testing.T) {
		inTurn := false
		h := waitsHarness(t, func(d *Deps) {
			d.Identity = adoptingIdentity
			d.IssueInTurn = func(string, claim.Token) (claim.Token, bool) { return testerToken, inTurn }
		})
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
		h.seen()

		h.advance(takeBound / 2)
		inTurn = true
		h.observe(runtime.Alive)
		h.advance(3 * takeBound)
		h.wantCharges()

		inTurn = false
		h.must(IssueTurnEnded{Claim: testToken})
		h.advance(takeBound - time.Second)
		h.wantCharges()
		h.advance(time.Second)
		h.wantCharges(WaitTake)
	})
	t.Run("a hold the bound finds", func(t *testing.T) {
		hears := false
		h := newBareHarness(t)
		withWaits(&h.deps)
		h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return hears, nil }
		h.restore(restoredRootTask())
		h.manualTakes()
		h.connect()
		h.seen()

		h.advance(3 * takeBound)
		h.wantCharges()
		if p := h.pending(); p.taken() {
			t.Fatalf("pending %+v, want the root's task still held", p)
		}

		hears = true
		h.advance(takeBound)
		h.wantCharges(WaitTake)
	})
}

// A take refused because the shim refused to adopt the working copy is charged, and the third
// such charge retires the process and relaunches the same session: the refusal cannot repeat
// without bound. Each charge's why names the adoption.
func TestChargeARefusedAdoptionAndRetireAtTheThird(t *testing.T) {
	h := waitsHarness(t)
	h.reach(StateReady)
	retiring := h.locator()
	refuseAdoptions(h)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing, Generation: 7})
	h.wantBudgets(Budgets{PromptFailures: 1})
	h.observe(runtime.Alive)
	h.wantBudgets(Budgets{PromptFailures: 2})

	h.observe(runtime.Alive)

	whys := h.charges()
	if len(whys) != 3 {
		t.Fatalf("charges %q, want three", whys)
	}
	for i, why := range whys {
		if !strings.Contains(why, "the working copy was not adopted") {
			t.Fatalf("charge %d's why is %q, want the refused adoption named", i+1, why)
		}
	}
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != retiring {
		t.Fatalf("suspended %+v, want the process whose adoptions were refused", suspend.Locator)
	}
	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{PromptRetires: 1})
	h.wantPrompts(0)
}

// A turn's start resets the count and so does a turn's end — the end of a turn the daemon never
// saw begin included — each written with the claim; a take resets nothing, whichever wait charged
// before it.
func TestChargeResetByATurnStartAndATurnEndNotByATake(t *testing.T) {
	h := waitsHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.seen()
	h.advance(takeBound)
	h.wantBudgets(Budgets{PromptFailures: 1})

	h.mustTake()
	h.wantBudgets(Budgets{PromptFailures: 1})
	h.advance(turnStartBound)
	h.wantBudgets(Budgets{PromptFailures: 2})
	h.mustTake()
	h.wantBudgets(Budgets{PromptFailures: 2})

	h.must(StreamTurnStart{Claim: testToken})
	h.wantBudgets(Budgets{})
	if stored := h.store.load(testToken).Budgets; stored != (Budgets{}) {
		t.Fatalf("stored budgets %+v after the turn's start, want the reset written", stored)
	}

	h.must(StreamTurnEnd{Claim: testToken})
	h.must(RequestDeliver{Claim: testToken, Task: "the next task"})
	h.seen()
	h.advance(takeBound)
	h.wantBudgets(Budgets{PromptFailures: 1})

	h.must(StreamTurnEnd{Claim: testToken})

	h.wantBudgets(Budgets{})
	if stored := h.store.load(testToken).Budgets; stored != (Budgets{}) {
		t.Fatalf("stored budgets %+v after the turn's end, want the reset written", stored)
	}
	h.wantCharges(WaitTake, WaitTurnStart, WaitTake)
}

// A daemon stopped for longer than a bound and started again charges nobody for the time it was
// down: each wait's clock starts again at the worker's first act after the start, here the shim's
// reconnect hello, and nobody is charged until one whole bound has passed after it — in the first
// wait and in the second. In the first wait the seen report the started daemon holds is the
// store's, so that bound's end writes the value again and charges nobody (decision 20); the
// charge comes one bound after the worker reports the new revision seen.
func TestChargeNoneAfterADaemonRestartUntilABoundAfterTheReconnectHello(t *testing.T) {
	t.Run("the first wait", func(t *testing.T) {
		h := waitsHarness(t)
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		h.seen()
		h.advance(takeBound / 2)

		h.outage(3 * takeBound)
		h.advance(deadline - time.Second)
		h.wantCharges()
		h.connect()
		h.advance(takeBound - time.Second)
		h.wantCharges()
		h.advance(time.Second)
		h.wantCharges()
		h.seen()
		h.advance(takeBound)

		h.wantCharges(WaitTake)
	})
	t.Run("the second wait", func(t *testing.T) {
		h := waitsHarness(t)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		if p := h.pending(); !p.awaitsTurn() {
			t.Fatalf("pending %+v, want the worker's take accepted", p)
		}
		h.advance(turnStartBound / 2)

		h.outage(2 * turnStartBound)
		h.advance(deadline - time.Second)
		h.wantCharges()
		h.connect()
		h.advance(turnStartBound - time.Second)
		h.wantCharges()
		h.advance(time.Second)

		h.wantCharges(WaitTurnStart)
	})
}

// A worker that makes no act after a daemon's start but its shim's reconnect hello is charged one
// bound after that hello in the second wait, and its third charge retires it: the clock's restart
// spares the daemon's outage, and nothing more. In the first wait the seen report the started
// daemon holds is the store's, so the first bound after the hello writes the value again and
// charges nobody (decision 20); a worker that then reports the new revision seen and takes
// nothing is charged one bound after that report, and its third charge retires it.
func TestSilentWorkerAfterItsReconnectIsChargedOneBoundAfterItAndRetiredAtTheThird(t *testing.T) {
	t.Run("the first wait", func(t *testing.T) {
		h := waitsHarness(t)
		h.manualTakes()
		h.reach(StateReady)
		retiring := h.locator()
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		h.seen()

		h.outage(3 * takeBound)
		h.connect()
		h.advance(takeBound)
		h.wantCharges()
		h.seen()
		for charge := 1; charge < testLimits().PromptFailures; charge++ {
			h.advance(takeBound)
			h.wantBudgets(Budgets{PromptFailures: charge})
		}
		h.advance(takeBound)

		h.wantCharges(WaitTake, WaitTake, WaitTake)
		if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != retiring {
			t.Fatalf("suspended %+v, want the silent worker's process", suspend.Locator)
		}
		h.wantCalls("Resume", 1)
		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{PromptRetires: 1})
	})
	t.Run("the second wait", func(t *testing.T) {
		h := waitsHarness(t)
		h.manualTakes()
		h.reach(StateReady)
		retiring := h.locator()
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		h.seen()
		h.advance(takeBound)
		h.advance(takeBound)
		h.wantBudgets(Budgets{PromptFailures: 2})
		h.mustTake()

		h.outage(2 * turnStartBound)
		h.connect()
		h.advance(turnStartBound - time.Second)
		h.wantBudgets(Budgets{PromptFailures: 2})
		h.advance(time.Second)

		h.wantCharges(WaitTake, WaitTake, WaitTurnStart)
		if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != retiring {
			t.Fatalf("suspended %+v, want the silent worker's process", suspend.Locator)
		}
		h.wantCalls("Resume", 1)
		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{PromptRetires: 1})
		if p := h.pending(); p.taken() {
			t.Fatalf("pending %+v, want the task taken back for the relaunched worker", p)
		}
	})
}

// A claim a daemon start restored with its agent ready, idle or working is given the
// registration deadline's bound as a reconnect deadline. A live process whose shim never says
// hello by then is retired and relaunched with a charged launch failure of its own cause,
// never_reconnected, and a task whose turn it was running is taken back for the relaunch. A
// hello inside the deadline leaves the claim untouched; a process the probe finds gone takes the
// death path.
func TestSilentRestoredClaimWithNoHelloIsRetiredAtTheReconnectDeadline(t *testing.T) {
	t.Run("a live idle process whose shim never says hello is retired and relaunched", func(t *testing.T) {
		h := newBareHarness(t)
		h.restore(fixture(StateIdle))
		alive := h.locator()

		h.advance(deadline - time.Second)
		h.wantCalls("Probe", 0)
		h.wantState(StateIdle)
		h.advance(time.Second)

		if probe := h.wantCalls("Probe", 1)[0]; probe.Locator != alive {
			t.Fatalf("probed %+v, want the restored process", probe.Locator)
		}
		if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != alive {
			t.Fatalf("suspended %+v, want the process whose shim never came back", suspend.Locator)
		}
		h.wantCalls("Resume", 1)
		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{LaunchFailures: 1})
		if cause := h.claim().PendingCause; cause != CauseNeverReconnected {
			t.Fatalf("the relaunch is pending cause %q, want %q", cause, CauseNeverReconnected)
		}
		if lines := h.logs.lines("the shim never reconnected after the daemon started; retired its process"); len(lines) != 1 {
			t.Fatalf("logged %v, want the retirement named once", lines)
		}
	})
	t.Run("a working claim's task is taken back for the relaunch", func(t *testing.T) {
		h := newBareHarness(t)
		h.restore(fixture(StateWorking))

		h.advance(deadline)

		h.wantCalls("Suspend", 1)
		h.wantState(StateLaunching)
		if p := h.pending(); p.taken() || !p.Interrupted || p.Number != 1 {
			t.Fatalf("pending %+v, want task 1 taken back, marked interrupted, for the relaunched worker", p)
		}
	})
	t.Run("a hello inside the deadline leaves the claim untouched", func(t *testing.T) {
		h := newBareHarness(t)
		h.restore(fixture(StateIdle))

		h.advance(deadline - time.Second)
		h.connect()
		h.advance(2 * deadline)

		h.wantCalls("Probe", 0)
		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 0)
		h.wantState(StateIdle)
		h.wantBudgets(Budgets{})
	})
	t.Run("a process found gone takes the death path", func(t *testing.T) {
		h := newBareHarness(t)
		h.restore(fixture(StateIdle))
		h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})

		h.advance(deadline)

		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 1)
		h.wantState(StateLaunching)
		if cause := h.claim().PendingCause; cause != CauseDied {
			t.Fatalf("the relaunch is pending cause %q, want the death's %q", cause, CauseDied)
		}
		if lines := h.logs.lines("supervise: process died"); len(lines) != 1 {
			t.Fatalf("logged %v, want the death named once", lines)
		}
	})
}

// A take whose accept the store refused to write is answered with no outcome, which the take route
// answers 500 CLAIM_WRITE_FAILED, and the plugin sends no take again after an answer that carries a
// code (pi-shared's task-take.ts, unanswered): its worker never learned the take was accepted, so
// no turn of it can start. The second bound passing over that take would charge the worker for the
// daemon's own failed write. The worker is not charged, and the task still reaches it: by an
// accepted answer, or at a newer revision of the value, which is when the plugin asks again.
func TestChargeNoneForATakeWhoseAcceptTheStoreRefused(t *testing.T) {
	h := waitsHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	asked := h.seen()
	h.store.fail("PutClaim", errBoom)
	h.store.fail("PutDelivery", errBoom)
	answer, _ := h.takeValue(asked)
	h.store.fail("PutClaim", nil)
	h.store.fail("PutDelivery", nil)

	accepted := answer.Outcome == TakeOutcomeAccepted
	for waited := time.Duration(0); !accepted && waited <= turnStartBound; waited += time.Minute {
		h.advance(time.Minute)
		h.observe(runtime.Alive)
		if value, held := h.values.value(h.token); held && value != asked {
			asked = value
			next, _ := h.takeValue(value)
			accepted = next.Outcome == TakeOutcomeAccepted
		}
	}

	h.wantCharges()
	if !accepted {
		t.Fatalf("no take of task %d was accepted within the second bound after the store refused the first take's write", asked.Number)
	}
}

// A daemon started again while a claim's latest revision is stored seen and not asked holds a seen
// report no worker made to it: a take of that revision may have failed on its first store write
// under the daemon that stopped, and that process's worker asks once per revision. The first
// expiry of the first wait after the start writes the value again, under the same number at a
// newer revision, and charges nobody (decision 20 of dispatch://INFRA-2811). A worker that then
// reports the new revision seen and does not take it is charged one bound after that report; one
// that reports nothing more holds a value nobody reported seeing, which charges nobody, is written
// again once per bound and is listed unseen.
func TestChargeNoneAtTheFirstExpiryForASeenReportReadFromTheStore(t *testing.T) {
	restarted := func(t *testing.T) (*harness, TaskValue) {
		t.Helper()
		h := waitsHarness(t)
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		stored := h.seen()
		h.restart()
		h.connect()
		h.advance(takeBound)

		h.wantCharges()
		h.wantBudgets(Budgets{})
		written := h.values.written()
		rewritten := written[len(written)-1]
		if len(written) != 2 || rewritten.Number != stored.Number || rewritten.Revision <= stored.Revision {
			t.Fatalf("the values written are %+v, want task %d written again at a newer revision than %d at the first expiry",
				written, stored.Number, stored.Revision)
		}
		if listed := h.m.UnseenTaskValue(); listed != nil {
			t.Fatalf("the claim is listed unseen as %+v at the rewrite, want no listing before a bound passes over the new revision", listed)
		}
		return h, rewritten
	}
	t.Run("a worker that reports the new revision seen is charged one bound after that report", func(t *testing.T) {
		h, rewritten := restarted(t)
		h.advance(time.Minute)
		if seen := h.seen(); seen != rewritten {
			t.Fatalf("the worker saw %+v, want the rewrite %+v", seen, rewritten)
		}
		h.advance(takeBound - time.Second)
		h.wantCharges()
		h.advance(time.Second)

		h.wantCharges(WaitTake)
	})
	t.Run("a worker that reports nothing more is charged by nobody, written again once per bound and listed unseen", func(t *testing.T) {
		h, rewritten := restarted(t)
		for bound := 1; bound <= 2; bound++ {
			h.advance(takeBound)
			written := h.values.written()
			if last := written[len(written)-1]; len(written) != 2+bound || last.Number != rewritten.Number {
				t.Fatalf("after bound %d the values written are %+v, want task %d written %d times", bound, written, rewritten.Number, 2+bound)
			}
			listed := h.m.UnseenTaskValue()
			if listed == nil || listed.Task != rewritten.Number || !listed.WrittenAt.Equal(h.clock.Now()) {
				t.Fatalf("after bound %d the claim is listed %+v, want it unseen with task %d written at %s", bound, listed, rewritten.Number, h.clock.Now())
			}
		}

		h.wantCharges()
		h.wantBudgets(Budgets{})
	})
}

// The registration of a new process clears the seen report and the take the record kept: they
// are the process before it's, and the new one has seen and asked nothing. A relaunched process
// whose registration read no value — its read of the bucket failed — and which the listener hands
// none, since neither its session nor the revision moved, reports nothing seen; its value is one
// nobody reported seeing, so the first bound charges nobody and writes the value again once,
// under its number, and lists the claim as unseen (R5).
//
// The negative control is the seen report kept across the registration (register before this
// change): the first bound reads the old process's report as the new one's, and charges the new
// process for a value it was never handed.
func TestSilentRegistrationOfANewProcessClearsTheSeenAndTakeState(t *testing.T) {
	h := waitsHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	seen := h.seen()
	h.store.fail("PutDelivery", errBoom)
	_, _ = h.takeValue(seen)
	h.store.fail("PutDelivery", nil)
	if c := h.claim(); c.TaskSeenRevision.Revision != seen.Revision || c.TaskTakeState != TakeRefused {
		t.Fatalf("the old process left seen revision %d and take %q, want revision %d seen and its take refused", c.TaskSeenRevision.Revision, c.TaskTakeState, seen.Revision)
	}
	written := len(h.values.written())

	h.observe(runtime.Gone)
	h.relaunched()

	for where, c := range map[string]Claim{"the machine": h.claim(), "the store": h.store.load(testToken)} {
		if c.TaskSeenRevision != (TaskRevision{}) || c.TaskTakeState != "" {
			t.Fatalf("%s holds seen %+v and take %q after the new process registered, want none", where, c.TaskSeenRevision, c.TaskTakeState)
		}
	}
	h.advance(takeBound)

	h.wantCharges()
	rewritten := h.values.written()
	if len(rewritten) != written+1 {
		t.Fatalf("the values written after the first bound are %+v, want one more than the %d before it", rewritten, written)
	}
	if last := rewritten[len(rewritten)-1]; last.Number != seen.Number || last.Revision <= seen.Revision {
		t.Fatalf("the first bound wrote %+v, want task %d again at a revision past %d", last, seen.Number, seen.Revision)
	}
	if listed := h.m.UnseenTaskValue(); listed == nil || listed.Task != seen.Number {
		t.Fatalf("the claim is listed %+v, want it unseen with task %d", listed, seen.Number)
	}
}

// The shim's reconnect hello after a daemon start is the same process coming back, not a new
// one: it clears neither the seen report nor the take the record holds, so a take the worker made
// before the restart still stands at its revision, and the worker, which asks once per revision,
// is not left owing a take it already made.
//
// The negative control is the reconnect hello clearing them as a registration does (reconnected
// given register's clearing).
func TestSilentReconnectHelloAfterADaemonRestartClearsNothing(t *testing.T) {
	h := waitsHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.mustTake()
	before := h.claim()
	if before.TaskTakeState != TakeAccepted || before.TaskSeenRevision.Revision == 0 {
		t.Fatalf("before the restart the claim holds seen %q/%d and take %q, want the accepted take at its revision",
			before.TaskSeenRevision.Epoch, before.TaskSeenRevision.Revision, before.TaskTakeState)
	}

	h.outage(time.Minute)
	h.connect()

	for where, c := range map[string]Claim{"the machine": h.claim(), "the store": h.store.load(testToken)} {
		if c.TaskSeenRevision != before.TaskSeenRevision || c.TaskTakeState != before.TaskTakeState {
			t.Fatalf("%s holds seen %+v and take %q after the reconnect hello, want %+v and %q as before the restart",
				where, c.TaskSeenRevision, c.TaskTakeState, before.TaskSeenRevision, before.TaskTakeState)
		}
	}
	if p := h.pending(); !p.awaitsTurn() {
		t.Fatalf("pending %+v after the reconnect hello, want the accepted take still awaiting its turn", p)
	}
}

// The second wait's clock starts at the take's put-back, not at its acceptance. A run the daemon
// gave the take — the first that started after it — turns out, by its report naming no task, not
// to have been the take's (notTheTakesTurn), so the take awaits its turn again from that report:
// a worker that then starts no turn is charged one whole bound after the put-back, though the
// take was accepted twenty-one minutes before it.
//
// The negative control is the bound armed for what is left of it counted from the acceptance
// (armWaits arming TimerTurnStart for the bound less the time since ConfirmedAt): the charge then
// comes four minutes after the put-back.
func TestChargeTheSecondWaitOneBoundAfterThePutBackNotTheAcceptance(t *testing.T) {
	h := waitsHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.mustTake()
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: testToken})
	h.advance(10 * time.Minute)
	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	h.advance(10 * time.Minute)
	if err := h.reportTurn(0, nil); err != nil {
		t.Fatalf("report the run as no task's: %v", err)
	}
	if p := h.pending(); !p.awaitsTurn() {
		t.Fatalf("pending %+v after the report, want the take awaiting its turn again", p)
	}

	h.advance(turnStartBound - time.Second)
	h.wantCharges()
	h.advance(time.Second)

	h.wantCharges(WaitTurnStart)
}

// A take that awaits its turn while the claim runs another turn is held (claimHold, in-turn), and
// the time inside that hold is never charged: the second wait's clock starts at the hold's end,
// the end of that turn. Here the run the daemon gave the take reports no task ahead of its own
// end frame, so the take awaits its turn again while that 40-minute run goes on; its end charges
// nothing, and a worker that then starts no turn is charged one bound after it.
//
// The negative control is the bound counted from the acceptance, as in the case above: the
// charge then comes at the first moment after the run's end.
func TestChargeNoneAtTheEndOfALongTurnOverATakeAwaitingItsTurn(t *testing.T) {
	h := waitsHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.mustTake()
	h.must(StreamTurnStart{Claim: testToken})
	if err := h.reportTurn(0, nil); err != nil {
		t.Fatalf("report the run as no task's: %v", err)
	}
	if p := h.pending(); !p.awaitsTurn() {
		t.Fatalf("pending %+v after the report, want the take awaiting its turn again", p)
	}

	h.advance(40 * time.Minute)
	h.wantCharges()
	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	h.wantCharges()
	h.advance(turnStartBound - time.Second)
	h.wantCharges()
	h.advance(time.Second)

	h.wantCharges(WaitTurnStart)
}
