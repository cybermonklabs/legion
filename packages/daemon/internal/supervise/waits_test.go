package supervise

// R5's two waits (waits.go): the first bound over a value the worker saw and did not take, or
// that nobody reported seeing; the second over an accepted take whose turn did not start; the
// count a turn resets and a take does not; the holds that charge nobody; and departure 7 of
// dispatch://INFRA-2811's plan — the time a daemon was not serving counts toward neither bound,
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
	h := newBareHarness(t)
	withWaits(&h.deps)
	for _, change := range set {
		change(&h.deps)
	}
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	return h
}

// outage is the daemon stopped for d and started again: the machine goes with everything it
// held in memory, its timers with it, the time passes with no daemon to see any of it, and a new
// machine is built from what the store holds.
func (h *harness) outage(d time.Duration) {
	h.t.Helper()
	h.m.Wait()
	h.clock.dropAll()
	h.clock.Advance(d)
	h.start(h.store.load(h.token))
}

// charges is the why of every charge the machine made, in order: each prompt failure, and each
// retirement a charge at the limit made.
func (h *harness) charges() []string {
	h.records.mu.Lock()
	defer h.records.mu.Unlock()
	var whys []string
	for _, r := range h.records.kept {
		if r.message != "supervise: prompt failure" && r.message != "supervise: retired after prompt failures" {
			continue
		}
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
	if c := h.claim(); c.TaskWritten != taken.Number || c.TaskWrittenRevision != written[1].Revision {
		t.Fatalf("the claim records written %d at revision %d, want %d at the rewrite's %d",
			c.TaskWritten, c.TaskWrittenRevision, taken.Number, written[1].Revision)
	}
	if back := h.records.of("supervise: a task was taken back"); len(back) != 1 {
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
// wait and in the second.
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
// bound after that hello, in each wait, and its third charge retires it: the clock's restart
// spares the daemon's outage, and nothing more.
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
