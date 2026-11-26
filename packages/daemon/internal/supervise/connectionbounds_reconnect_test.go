package supervise

// The bounds and the connection (connectionbounds_test.go): the rows of the reconnect deadline
// (TimerRegistration over a live process), which bounds a shim that has not said hello since a
// close or a daemon start, and a hello that neither the agent's answer nor a turn frame followed.

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Row 8: an agent that answers nothing, and sends no turn frame, by the reconnect deadline counted
// from the hello. The hello asks once, and the sweep before the deadline asks nothing again; at
// the deadline the process is relaunched, charged nothing, and the take is taken back as a cut
// turn, so the relaunched worker takes it again told its turn on it may have run.
func TestTakeIsTakenBackAsACutTurnWhenTheAgentAnswersNothingByTheReconnectDeadline(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	h.reconnectLater()
	h.helloOf(true, false)
	if ask := h.conn.LastAsk(); ask != "ask-1" {
		t.Fatalf("the hello's connection was asked up to %q, want the one ask the hello makes", ask)
	}
	h.advance(deadline / 2)
	h.observe(runtime.Alive)
	if ask := h.conn.LastAsk(); ask != "ask-1" {
		t.Fatalf("the connection was asked up to %q after the sweep, want no ask but the hello's", ask)
	}
	h.wantCalls("Resume", 0)

	h.advance(deadline / 2)

	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	h.wantCharges()
	h.wantTakenBackWithItsNote(number)
	// A late end frame of the process let go disarms nothing of the launch that replaces it.
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if !h.armed(TimerRegistration) {
		t.Fatal("a late frame of the process let go disarmed the new launch's registration deadline")
	}
	h.wantTakenOnceMore(number, relaunchedAndTaken)
}

// Row 10: a live process whose shim never says hello again after its connection closed, while
// its claim holds a pending task: at the reconnect deadline counted from the close the process is
// probed, retired and relaunched with a charged launch failure, never_reconnected, and the take is
// taken back as a cut turn. The negative control is the commit under this change, where a close
// arms no deadline and the claim waits on a connection that never comes.
func TestRelaunchAProcessWhoseShimNeverReconnectsAfterAClose(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	alive := h.locator()
	h.reconnectLater()
	h.advance(deadline - time.Second)
	h.wantCalls("Resume", 0)

	h.advance(time.Second)

	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != alive {
		t.Fatalf("suspended %+v, want the process whose shim never came back", suspend.Locator)
	}
	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	if cause := h.claim().PendingCause; cause != CauseNeverReconnected {
		t.Fatalf("the relaunch is pending cause %q, want %q", cause, CauseNeverReconnected)
	}
	h.wantTakenBackWithItsNote(number)
	h.wantTakenOnceMore(number, relaunchedAndTaken)
}

// Row 11: a hello before the reconnect deadline the close armed disarms it, and the bounds are
// armed again as rows 5 to 9a say: here from the agent's answer of no turn.
func TestTakeHelloBeforeTheReconnectDeadlineDisarmsIt(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	h.reconnectLater()
	h.advance(deadline - 10*time.Second)

	h.helloOf(true, false)
	h.advance(20 * time.Second)

	h.wantCalls("Resume", 0)
	h.wantState(StateReady)
	h.answerAsk(false)
	h.wantArmedFromNow(TimerTurnStart, turnStartBound, "at the answer")
	h.wantAwaitingItsTurn(number, "at the answer")
}

// Row 12: a claim holding no pending task arms no deadline at its close, its hello asks nothing,
// and a hello that comes after the registration bound relaunches nothing, as before this change.
// It passes on the commit under this change too; it pins that the rule reaches a claim with a
// pending task only.
func TestRelaunchNothingForALateHelloOfAClaimHoldingNoTask(t *testing.T) {
	h := boundsHarness(t)
	h.reach(StateIdle)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("the claim holds %+v, want no task", p)
	}
	h.reconnectLater()
	h.wantUnarmed("after the close", TimerRegistration)
	h.advance(deadline + time.Minute)

	h.helloOf(true, false)
	h.advance(2 * deadline)

	if ask := h.conn.LastAsk(); ask != "" {
		t.Fatalf("the hello's connection was asked %q, want nothing asked of a claim holding no task", ask)
	}
	h.wantCalls("Resume", 0)
	h.wantState(StateIdle)
	h.wantBudgets(Budgets{})
}

// Row 18: a take awaiting its turn while its finish is owed, when the connection closes and no
// hello comes by the reconnect deadline: the task is finished once by its owed report, nothing of
// it is taken back, and the worker is given nothing of it again. The store refuses the finish at
// the deadline, so the process is kept and the deadline tried again at the probe interval; the
// end-frame bound makes the finish once the store answers, and the deadline's retry goes with the
// task (row 29c, dropReconnectWithTask), so the worker, holding no task now, is not retired as one
// that never reconnected. The negative control is the commit under this change, where a close
// armed no deadline.
func TestTakeAwaitingItsTurnWithAnOwedFinishIsFinishedAtTheReconnectDeadline(t *testing.T) {
	h, number := owedFinishAwaitingItsTurn(t, true)
	h.reconnectLater()

	h.advance(deadline)

	h.wantCalls("Resume", 0)
	if p := h.pending(); p.Number != number || !p.taken() {
		t.Fatalf("the claim holds %+v at the deadline, want task %d's take kept while its finish is refused", p, number)
	}
	h.store.fail("RetireDelivery", nil)
	h.advance(testProbe)
	h.wantOwedFinishMade(number)
	h.wantUnarmed("once the task is finished", TimerRegistration)
	h.wantCalls("Resume", 0)
	h.wantBudgets(Budgets{})
}

// Row 25: the connection of a registered claim holding a pending task closes before its agent
// says ready, and the agent says ready over HTTP with no hello since. The ready keeps a reconnect
// deadline at the later of the registration deadline standing and the close plus the reconnect
// bound (readyView): a hello inside that window, past the registration deadline's own time,
// retires nothing; with no hello by it, the no-hello path runs, the live process retired and
// relaunched with never_reconnected and the take it accepted taken back as a cut turn. The
// negative control is the commit under this change, where the ready disarmed the deadline, and a
// shim that never came back held the claim and its take for ever.
func TestTakeOfAClaimReadiedAfterItsConnectionClosedIsBoundByTheReconnectDeadline(t *testing.T) {
	// readiedAfterAClose is the claim registered, a task delivered, its connection closed late in
	// the registration deadline's bound, and its agent ready and its take accepted after the close:
	// the deadline the ready keeps is the close's, due closeToDeadline from then.
	const sinceRegistration, afterClose = deadline - time.Minute, 10 * time.Second
	closeToDeadline := deadline - afterClose
	readiedAfterAClose := func(t *testing.T) (*harness, uint64) {
		h := waitsHarness(t, currentPlugin)
		h.manualTakes()
		h.reach(StateRegistered)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		h.advance(sinceRegistration)
		h.reconnectLater()
		h.advance(afterClose)
		h.ready()
		h.mustTake()
		return h, h.pending().Number
	}
	t.Run("a hello inside the window", func(t *testing.T) {
		h, number := readiedAfterAClose(t)
		h.advance(2 * time.Minute)
		h.wantCalls("Suspend", 0)

		h.helloOf(true, false)
		h.answerAsk(false)

		h.advance(deadline)
		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 0)
		h.wantState(StateReady)
		h.wantAwaitingItsTurn(number, "after the hello")
	})
	t.Run("no hello", func(t *testing.T) {
		h, number := readiedAfterAClose(t)
		alive := h.locator()
		h.advance(closeToDeadline - time.Second)
		h.wantCalls("Resume", 0)

		h.advance(time.Second)

		if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != alive {
			t.Fatalf("suspended %+v, want the process whose shim never came back", suspend.Locator)
		}
		h.wantCalls("Resume", 1)
		if cause := h.claim().PendingCause; cause != CauseNeverReconnected {
			t.Fatalf("the relaunch is pending cause %q, want %q", cause, CauseNeverReconnected)
		}
		h.wantTakenBackWithItsNote(number)
	})
}

// Row 27: after a daemon start, a claim restored registered whose agent says it is ready over
// HTTP and takes its task, and whose shim never says hello to this daemon. The ready keeps the
// reconnect deadline the start armed (viewNoHello, readyView), so the claim does not hold its take
// for ever: at the deadline the live process is retired and relaunched with never_reconnected,
// the take taken back as a cut turn, and the relaunched worker takes it again told its turn on it
// may have run. The negative control is the commit under this change, where the ready disarmed
// the deadline and nothing ended the wait.
func TestTakeOfARestoredClaimReadiedWithNoHelloIsEndedByTheReconnectDeadline(t *testing.T) {
	h := waitsHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateRegistered)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.restart()
	h.ready()
	h.mustTake()
	number := h.pending().Number
	h.advance(deadline - time.Second)
	h.wantCalls("Resume", 0)

	h.advance(time.Second)

	h.wantCalls("Suspend", 1)
	h.wantCalls("Resume", 1)
	if cause := h.claim().PendingCause; cause != CauseNeverReconnected {
		t.Fatalf("the relaunch is pending cause %q, want %q", cause, CauseNeverReconnected)
	}
	h.wantTakenBackWithItsNote(number)
	h.wantTakenOnceMore(number, relaunchedAndTaken)
}

// Row 29b: a claim whose connection closed while it held no pending task, so no reconnect deadline
// stands, is written a task: the task becoming pending over the cut view arms the deadline whole
// from then (armReconnectForTask, queue). The worker takes it over HTTP, and with no hello by the
// deadline the no-hello path runs: the live process is retired and relaunched with
// never_reconnected, and the take is taken back as a cut turn, which the relaunched worker takes
// told its turn may have run. The negative control is the commit under this change, where the task
// armed no deadline and nothing bounded the take while the process lived.
func TestTakeOfATaskWrittenOverAClosedConnectionIsBoundByTheReconnectDeadline(t *testing.T) {
	h := boundsHarness(t)
	h.reconnectLater()
	h.wantUnarmed("after a close over no task", TimerRegistration)

	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})

	h.wantArmedFromNow(TimerRegistration, deadline, "once the task is pending")
	h.mustTake()
	number := h.pending().Number
	h.advance(deadline)
	h.wantCalls("Resume", 1)
	if cause := h.claim().PendingCause; cause != CauseNeverReconnected {
		t.Fatalf("the relaunch is pending cause %q, want %q", cause, CauseNeverReconnected)
	}
	h.wantTakenBackWithItsNote(number)
	h.wantTakenOnceMore(number, relaunchedAndTaken)
}

// Row 29c: a claim whose connection closed while it held a pending task, the reconnect deadline
// armed, whose worker takes the task and reports its turn over HTTP while the shim stays down, the
// store finishing it: the deadline goes with the task (dropReconnectWithTask), so its time passes
// with nothing retired and nothing charged. In a second case the claim is registered and not ready:
// the close keeps the registration deadline it runs under, and when its task is dropped that
// deadline stays and retires the process that never says ready. The negative control of the first
// case is the commit under this change, where the deadline stood with no task, and its no-hello
// path retired the worker and charged it a launch failure as never reconnected; the second passes
// there too, and pins that only a claim whose agent said it is ready loses the deadline.
func TestTakeFinishedWhileItsConnectionIsClosedTakesTheReconnectDeadlineWithIt(t *testing.T) {
	t.Run("ready, the task taken, reported and finished over HTTP", func(t *testing.T) {
		h := boundsHarness(t)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		h.reconnectLater()
		h.mustTake()
		number := h.pending().Number
		if err := h.reportTurn(number, nil); err != nil {
			t.Fatalf("the report of the take's turn answered %v", err)
		}
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the claim holds %+v after the report, want task %d finished", p, number)
		}
		h.wantUnarmed("once the task is finished", TimerRegistration)

		h.advance(deadline)

		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 0)
		h.wantBudgets(Budgets{})
		h.wantCharges()
	})
	t.Run("registered, the task dropped", func(t *testing.T) {
		holds := true
		h := waitsHarness(t, currentPlugin, func(d *Deps) {
			d.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return holds, nil }
		})
		h.manualTakes()
		h.reach(StateRegistered)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 5})
		h.reconnectLater()
		holds = false

		h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})

		if lines := h.logs.lines("the issue has left the phase this task was queued for"); len(lines) != 1 {
			t.Fatalf("logged %q, want the first task dropped once", lines)
		}
		if !h.armed(TimerRegistration) {
			t.Fatal("the drop disarmed the registration deadline of a claim not ready")
		}
		h.advance(deadline)
		h.wantCalls("Resume", 1)
	})
}

// restoredWorking is a daemon start over a claim restored working on the current plugin, its
// process's shim not yet said hello to this daemon: the claim whose turn an earlier daemon saw
// start, which the start asks its agent about at the first hello (askFirst). It holds the
// fixture's pending task, its take's turn running, as change leaves it, and every take of it is
// driven by the test.
func restoredWorking(t *testing.T, change func(*Claim)) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.PluginContract = testTakeContract
	h.manualTakes()
	restored := fixture(StateWorking)
	restored.PluginContract = testTakeContract
	if change != nil {
		change(&restored)
	}
	h.restore(restored)
	return h
}

// A daemon start's own ask of a claim restored working (askFirst) serves only the first hello since
// that start, since the stream reads an answer only on the connection that asked. A later hello over
// the pending task asks once on its own connection and arms the reconnect deadline whole from
// itself, as every reconnect hello does (helloView, askView); the start's ask makes no second ask
// on it (askTurnOver), and no sweep asks that connection again (observe, row 31): the deadline
// bounds the ask. The negative control of the hello's ask and deadline is the commit under the
// change that made them, where the later hello let the start's ask stand for it: it asked through
// askTurnOver and armed no deadline, so a shim that went quiet after that hello was bounded by
// nothing but the stall backstop. That of the sweep is the head before row 31 (2da324b8), where
// each sweep asked the connection again through askTurnOver.
func TestTakeOfARestoredWorkingClaimIsAskedOnceAtALaterHelloUnderItsOwnDeadline(t *testing.T) {
	h := restoredWorking(t, nil)
	h.conn.FailGetState(errBoom)
	h.connect()
	h.reconnectLater()

	h.helloOf(true, true)

	if ask := h.conn.LastAsk(); ask != "ask-1" {
		t.Fatalf("the later hello's connection was asked up to %q, want the one ask the hello makes", ask)
	}
	h.wantArmedFromNow(TimerRegistration, deadline, "at the later hello")
	h.observe(runtime.Alive)
	if ask := h.conn.LastAsk(); ask != "ask-1" {
		t.Fatalf("the later hello's connection was asked up to %q after a sweep, want no ask but the hello's", ask)
	}
}

// Row 30, at a later hello: a claim restored working holds a pending task, its first hello's ask
// went unanswered, its connection closed, and the next hello armed the reconnect deadline whole at
// itself; a sweep passes, and the agent answers that hello's ask. The answer makes the view whole
// in the one place every answer does (answerView), which disarms the deadline, so at the
// deadline's time nothing is retired, charged or taken back, and the task's turn runs on. The
// negative control is the head before this change (2da324b8): the sweep asked the connection
// again through askTurnOver, whose answer made the view whole and left the deadline armed; the
// hello's own answer then found the view whole and changed nothing, and at the deadline the path
// of a shim that never said hello (retireNeverReady) retired the live worker in its turn, charged
// it a launch failure and took its task back, so the task ran twice.
func TestTakeOfARestoredWorkingClaimAnsweredAtALaterHelloIsNotRetiredAtItsDeadline(t *testing.T) {
	h := restoredWorking(t, nil)
	h.conn.FailGetState(errBoom)
	h.connect()
	h.reconnectLater()
	h.helloOf(true, true)
	h.observe(runtime.Alive)
	h.answerAsk(true)

	h.advance(deadline)

	h.wantCalls("Suspend", 0)
	h.wantCalls("Resume", 0)
	h.wantBudgets(Budgets{})
	h.wantCharges()
	h.wantState(StateWorking)
	if p := h.pending(); p.Number != 1 || !p.startRecorded() || p.Interrupted {
		t.Fatalf("the claim holds %+v, want task 1's take with its turn running", p)
	}
}

// Row 32, with rows 30 and 31: a daemon start restores a claim working holding a pending task
// (askFirst), here a take awaiting its turn behind the turn the earlier daemon saw start. The first
// hello's ask is the start's own (askTurnOver), and that hello arms the reconnect deadline whole at
// itself, as every hello that asks over a pending task does (helloView).
//
//   - Unanswered, the ask is not asked again at a sweep, and at the deadline the process is
//     relaunched, charged nothing, and the take taken back as a cut turn, so the relaunched worker
//     takes it again told its turn on it may have run. That deadline is the later of the hello's
//     whole bound and the daemon's start plus runtime.CompactTimeout, never the fixed one alone: a
//     claim stored working with its take accepted and its turn not started is the record a
//     compaction's start writes, and a handoff may still run over the take for that long with Oh
//     My Pi answering no ask behind it, so the boot protects the take to that fixed deadline
//     (departure 10 of dispatch://INFRA-2811's plan, handoffGuard), while a hello late enough
//     still gets its whole reconnect bound. Both orderings run. The negative control is the head
//     before this change (2da324b8), where the hello disarmed the start's deadline and armed none,
//     so the unanswered ask was bounded only by the sweep's repeat and the stall backstop.
//   - Answered at the hello, the answer disarms the deadline where it makes the view whole
//     (answerView), and at the deadline's time nothing is relaunched or charged. The hello's one
//     ask is made off its decision (askProtected), so the answer reaches the machine as the stream
//     posts it, behind the hello, and gives no task the turn it reports: a stored working state
//     cannot tell a parked take from a foreign turn (answerView). It passes at the head before this
//     change too, where no deadline was armed at the hello; with that deadline armed and
//     askTurnOver making the view whole without disarming it, the deadline would retire the live
//     worker as one whose shim never said hello.
func TestTakeOfARestoredWorkingClaimIsBoundedByTheReconnectDeadlineArmedAtItsFirstHello(t *testing.T) {
	awaiting := func(c *Claim) { c.Pending.TurnStarted, c.Pending.TakenText = false, c.Pending.Task }
	for _, tc := range []struct {
		name string
		// hello is how long after the daemon's start the first hello comes.
		hello time.Duration
	}{
		{"an unanswered ask, the handoff's deadline the later", deadline / 2},
		{"an unanswered ask, the hello's whole bound the later", runtime.CompactTimeout - deadline/2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := restoredWorking(t, awaiting)
			number := h.pending().Number
			until := h.clock.Now().Add(runtime.CompactTimeout)
			h.conn.FailGetState(errBoom)
			h.advance(tc.hello)
			h.connect()
			if bound := h.clock.Now().Add(deadline); bound.After(until) {
				until = bound
			}
			if due, ok := h.dueOf(TimerRegistration); !ok || !due.Equal(until) {
				t.Fatalf("at the first hello the registration timer is armed %t, due %s; want it due at the later of the hello's bound and the handoff's deadline, %s", ok, due, until)
			}
			// The hello's one ask is made off its decision (askProtected), so its failure reaches the
			// log a moment after the hello.
			const unanswered = "supervise: the agent could not say whether it is in a turn"
			for wait := time.Now().Add(5 * time.Second); len(h.logs.lines(unanswered)) == 0 && time.Now().Before(wait); {
				time.Sleep(time.Millisecond)
			}
			h.advance(deadline / 2)
			h.observe(runtime.Alive)
			if asks := h.logs.lines(unanswered); len(asks) != 1 {
				t.Fatalf("the asks the agent left unanswered are %q, want the first hello's alone", asks)
			}
			h.advance(until.Sub(h.clock.Now()) - time.Second)
			h.wantCalls("Resume", 0)

			h.advance(time.Second)

			h.wantCalls("Resume", 1)
			h.wantState(StateLaunching)
			h.wantBudgets(Budgets{})
			h.wantCharges()
			if cause := h.claim().PendingCause; cause != "" {
				t.Fatalf("the relaunch is pending cause %q, want none: it is charged nothing", cause)
			}
			h.wantTakenBackWithItsNote(number)
			h.wantTakenOnceMore(number, relaunchedAndTaken)
		})
	}
	t.Run("an answered ask", func(t *testing.T) {
		h := restoredWorking(t, awaiting)
		number := h.pending().Number
		h.conn.SetStreaming(true)
		h.connect()
		// The hello's one ask is made off the decision (askProtected), so its answer reaches the
		// machine as the stream posts it, behind the hello.
		h.answerAsk(true)
		h.wantUnarmed("after the answer to the first hello's ask", TimerRegistration)

		h.advance(deadline)

		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 0)
		h.wantBudgets(Budgets{})
		h.wantState(StateWorking)
		h.wantAwaitingItsTurn(number, "at the deadline's time")
	})
}

// Row 33: a close of a claim whose agent has said it is ready sets the reconnect deadline to the
// later of the one standing and the close plus the reconnect bound, never earlier, and a close
// over no pending task disarms it, as a ready over a closed view does (closeView, readyView,
// reconnectFromClose); before the agent is ready, a close leaves the registration deadline as it
// stands. TestACloseAfterAnUnansweredHelloArmsAWholeReconnectBoundFromTheClose and
// TestACloseOverNoTaskAfterAnUnansweredHelloRetiresNothing (reconnectclose_test.go) hold the first
// and third cases up to the close's deadline; these are the row's cases under c2, and the first
// goes on to the hello.
//
//   - A hello asks over the take and its ask goes unanswered, which arms the deadline at that
//     hello; the connection closes half a minute before it, and the next hello comes half a bound
//     after the close, past the first hello's deadline: the close armed a whole bound from itself,
//     that hello ends it, and nothing is retired or charged.
//   - A hello asks over a task no take was accepted for, and a take drops the task (its issue has
//     left the phase it was queued for) while that ask stands unanswered, so the claim holds no
//     task: the close disarms the deadline the hello armed, and nothing is retired at its time.
//   - The same with the take's task finished by its report over HTTP.
//   - A claim registered and not ready, holding a task, keeps the registration deadline it runs
//     under as it stood at the close, which then retires the process that never says ready.
//
// The negative control of the first three is the head before this change (9b493e61): the close
// kept a deadline standing (armReconnectUnlessArmed) and disarmed nothing over no task, so at the
// first hello's deadline the no-hello path retired the live worker and charged it, as never
// reconnected. The fourth passes there too, and pins that the rule reaches only a claim whose
// agent said it is ready.
func TestTakeOfAReadyClaimWhoseConnectionClosesHasAWholeReconnectBoundFromTheClose(t *testing.T) {
	t.Run("a hello within a whole bound of a close during an unanswered hello's ask", func(t *testing.T) {
		h := boundsHarness(t)
		number := h.takeOf("implement the plan")
		h.reconnectLater()
		h.conn.FailGetState(errBoom)
		h.helloOf(true, false)
		helloDue, _ := h.dueOf(TimerRegistration)
		h.advance(deadline - 30*time.Second)

		h.reconnectLater()

		h.wantArmedFromNow(TimerRegistration, deadline, "at the close")
		h.advance(deadline / 2)
		if !h.clock.Now().After(helloDue) {
			t.Fatalf("the next hello comes at %s, want it past the first hello's deadline, %s", h.clock.Now(), helloDue)
		}
		h.wantCalls("Suspend", 0)
		h.helloOf(true, false)
		h.wantArmedFromNow(TimerRegistration, deadline, "at the hello after the close")
		h.answerAsk(false)
		h.wantUnarmed("at the answer to that hello's ask", TimerRegistration)
		h.advance(deadline)

		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 0)
		h.wantBudgets(Budgets{})
		h.wantCharges()
		if cause := h.claim().PendingCause; cause != "" {
			t.Fatalf("the claim is pending cause %q, want none", cause)
		}
		h.wantAwaitingItsTurn(number, "a whole bound after the hello")
	})
	t.Run("a ready claim whose task a take dropped during an unanswered hello's ask", func(t *testing.T) {
		holds := true
		h := boundsHarness(t, func(d *Deps) {
			d.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return holds, nil }
		})
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 5})
		h.reconnectLater()
		h.conn.FailGetState(errBoom)
		h.helloOf(true, false)
		holds = false
		if answer := h.take(); answer.Outcome != TakeOutcomeDropped {
			t.Fatalf("take = %+v, want the task dropped", answer)
		}
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the claim holds %+v after the drop, want no task", p)
		}
		h.advance(deadline - 30*time.Second)

		h.reconnectLater()

		h.wantUnarmed("at the close over no task", TimerRegistration)
		h.advance(deadline)
		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 0)
		h.wantBudgets(Budgets{})
		h.wantState(StateReady)
	})
	t.Run("a ready claim whose take was finished over HTTP during an unanswered hello's ask", func(t *testing.T) {
		h := boundsHarness(t)
		number := h.takeOf("implement the plan")
		h.reconnectLater()
		h.conn.FailGetState(errBoom)
		h.helloOf(true, false)
		h.reportOf(1, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the claim holds %+v after the report, want task %d finished", p, number)
		}
		h.advance(deadline - 30*time.Second)

		h.reconnectLater()

		h.wantUnarmed("at the close over no task", TimerRegistration)
		h.advance(deadline)
		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 0)
		h.wantBudgets(Budgets{})
		h.wantCharges()
	})
	t.Run("a claim not ready keeps its registration deadline as it stood", func(t *testing.T) {
		h := waitsHarness(t, currentPlugin)
		h.manualTakes()
		h.reach(StateRegistered)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		due, armed := h.dueOf(TimerRegistration)
		if !armed {
			t.Fatal("the registered claim runs under no registration deadline")
		}
		h.advance(deadline / 2)

		h.reconnectLater()

		if at, ok := h.dueOf(TimerRegistration); !ok || !at.Equal(due) {
			t.Fatalf("after the close the registration deadline is armed %t, due %s; want it as it stood, due %s", ok, at, due)
		}
		h.advance(due.Sub(h.clock.Now()) - time.Second)
		h.wantCalls("Resume", 0)
		h.advance(time.Second)
		h.wantCalls("Resume", 1)
	})
}

// helloFailingOn is the shim's hello on the claim's connection, whose decision the store fails by
// refusing method while it is made; the store takes that method again after it.
func (h *harness) helloFailingOn(method string, hello StreamHello) {
	h.t.Helper()
	h.store.fail(method, errBoom)
	h.conns.Register(h.token, h.conn)
	err := h.m.Handle(h.ctx, hello)
	h.m.Wait()
	h.store.fail(method, nil)
	if err == nil {
		h.t.Fatalf("the hello's decision succeeded, want it failed on %s", method)
	}
}

// identifiedHello is the shim's hello at the claim's generation, saying its backlog is whole and
// carrying the pod's identity, so a claim with a secrets broker enrolls at it (ensureEnrolled).
func (h *harness) identifiedHello() StreamHello {
	return StreamHello{Claim: h.token, Generation: h.generation(), BacklogWhole: true, AgentSecrets: testIdentity}
}

// Row 34: a reconnect hello bounds the view it opens before any step of its decision that can
// fail. It reads whether a daemon start's own ask serves it (startAskServes) from the view as the
// hello found it, then marks the view asked and the hello seen and arms the reconnect deadline
// whole from itself (helloView), and only then reads the hello (readHello) and enrolls
// (ensureEnrolled). A decision that fails at one of those leaves the claim holding its task under
// that deadline, and no sweep asks the connection: at the deadline the process is relaunched,
// charged nothing, as after a hello whose ask went unanswered (endUnanswered), its take taken back
// as a cut turn, and nothing is charged as never reconnected. One case for each step the harness
// fails:
//
//   - the claim write of a fresh enrollment (ensureEnrolled), at the hello of a ready claim
//     (reconnected), which TestAHelloWhoseDecisionFailsOnTheStoreLeavesTheClaimBounded holds too,
//     and at the hello of a working one (reconnectedMidTurn);
//   - the claim write of a revocation (revokeUnheld), at the first hello after a daemon start of
//     a claim whose session another claim holds now;
//   - the delivery write of a put-back on the producer's word (bindRestoredAnchor, readHello), at
//     the first hello after a daemon start of a claim restored working, whose start's own ask
//     serves that hello (askFirst) and is never made.
//
// The negative control is the head before this change (9b493e61): each hello disarmed the
// deadline first, and its failed decision left the view cut with no deadline, so nothing bounded
// the take while the process lived.
func TestTakeOverAHelloWhoseDecisionFailsIsRelaunchedUnchargedAtTheHellosDeadline(t *testing.T) {
	wantRelaunchedUncharged := func(h *harness, number uint64) {
		h.t.Helper()
		h.wantArmedFromNow(TimerRegistration, deadline, "after the hello's failed decision")
		asked := h.conn.LastAsk()
		h.observe(runtime.Alive)
		if ask := h.conn.LastAsk(); ask != asked {
			h.t.Fatalf("a sweep asked the hello's connection up to %q, want no ask after %q", ask, asked)
		}
		h.advance(deadline - time.Second)
		h.wantCalls("Resume", 0)

		h.advance(time.Second)

		h.wantCalls("Resume", 1)
		h.wantBudgets(Budgets{})
		h.wantCharges()
		if cause := h.claim().PendingCause; cause != "" {
			h.t.Fatalf("the relaunch is pending cause %q, want none: it is charged nothing", cause)
		}
		if lines := h.logs.lines(viewUnansweredRelaunchLine); len(lines) != 1 {
			h.t.Fatalf("the relaunch logged %q, want the line of a hello no answer followed once", lines)
		}
		if lines := h.logs.lines("the shim never reconnected"); len(lines) != 0 {
			h.t.Fatalf("logged %q, want no retirement of a shim that never reconnected", lines)
		}
		if p := h.pending(); p.Number != number || p.taken() || !p.Interrupted {
			h.t.Fatalf("the claim holds %+v, want task %d taken back with the note of a cut turn", p, number)
		}
	}
	t.Run("a fresh enrollment's claim write, at a ready claim's hello", func(t *testing.T) {
		h := waitsHarness(t, currentPlugin)
		h.withSecrets()
		h.manualTakes()
		h.reach(StateReady)
		number := h.takeOf("implement the plan")
		h.reconnectLater()

		h.helloFailingOn("PutClaim", h.identifiedHello())

		wantRelaunchedUncharged(h, number)
	})
	t.Run("a fresh enrollment's claim write, at a working claim's hello", func(t *testing.T) {
		h := waitsHarness(t, currentPlugin)
		h.withSecrets()
		h.manualTakes()
		h.reach(StateWorking)
		number := h.pending().Number
		h.reconnectLater()
		h.conn.SetStreaming(true)

		h.helloFailingOn("PutClaim", h.identifiedHello())

		wantRelaunchedUncharged(h, number)
	})
	t.Run("a revocation's claim write, at the first hello after a daemon start", func(t *testing.T) {
		h := waitsHarness(t, currentPlugin)
		h.withSecrets()
		h.manualTakes()
		h.launch()
		h.helloWithIdentity()
		h.register()
		h.ready()
		if h.claim().Enrollment == nil {
			t.Fatal("the claim records no enrollment")
		}
		number := h.takeOf("implement the plan")
		h.store.setHold(sessionID(session), rootToken)
		h.restart()

		h.helloFailingOn("PutClaim", h.identifiedHello())

		if e := h.claim().Enrollment; e != nil {
			t.Fatalf("the claim records enrollment %+v, want it revoked before the claim write failed", *e)
		}
		// An operator gives the session back to this claim, so the relaunch at the deadline can
		// resume it: a hold of another claim's would refuse that launch, which is no part of the
		// deadline's path.
		h.store.setHold(sessionID(session), h.token)
		wantRelaunchedUncharged(h, number)
	})
	t.Run("a put-back's delivery write, at the first hello after a daemon start", func(t *testing.T) {
		h, number := takeAcrossARestart(t, givenRunRunning)
		h.reportHeldOf(2, 0, number)
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.conn.SetStreaming(true)

		h.helloFailingOn("PutDelivery", StreamHello{Claim: h.token, Generation: h.generation(), BacklogWhole: true})

		h.wantPutBackBy(number, putBacks, "held")
		wantRelaunchedUncharged(h, number)
	})
}

// Row 34 over a daemon start's own ask: the first hello since a daemon start of a claim restored
// ready on a stale plugin, holding a take whose relaunch waits for its agent's word on whether it
// is in a turn (askTurn), is the hello that ask serves (startAskServes), and it dates that ask
// (askedAt) where it marks its view asked and arms its deadline, before any step of its decision
// that can fail (helloView). Here the decision fails on the claim write of the fresh enrollment
// (ensureEnrolled), so the ask itself is never made, and the take's turn then starts on that
// connection: the turn frame makes the view whole and keeps the deadline, the bound of the daemon
// start's own ask, which only its answer ends (seeView). At that deadline the stale-plugin lane
// decides the relaunch, charged nothing (reconnectDeadline, refreshPlugin), and nothing is charged
// as never reconnected:
//
//   - with the take's turn running at the deadline, the lane holds the relaunch for that turn's
//     end and its report (holdStaleRelaunch), which finishes the task once;
//   - with the task finished over HTTP before the deadline, the claim holds no task, and the lane
//     relaunches it onto the current plugin, as it relaunches any stale claim that holds none
//     (row 29a).
//
// The negative control is the head before this change (26aa05dc), which dated the ask only after
// the steps that can fail (reconnected): the failed hello left askedAt zero, so the deadline read
// the whole view as that of a shim that never said hello, and retired the live worker with a
// launch failure charged and the cause never_reconnected, in both cases.
func TestTakeOfARestoredStaleClaimWhoseFirstHelloFailedIsLeftToTheStalePluginLaneAtItsDeadline(t *testing.T) {
	failedHelloThenTheTakesTurn := func(t *testing.T) (*harness, uint64) {
		t.Helper()
		h := restoreStaleTake(t, func(d *Deps) { d.Secrets = &fakeEnroller{now: d.Clock.Now} })
		number := h.pending().Number
		h.helloFailingOn("PutClaim", h.identifiedHello())
		h.wantArmedFromNow(TimerRegistration, deadline, "after the first hello's failed decision")
		h.must(StreamTurnStart{Claim: h.token})
		h.wantTakesTurnStanding(number, "once the take's turn started")
		h.wantArmedFromNow(TimerRegistration, deadline, "after the take's turn started")
		return h, number
	}
	wantLeftToTheLane := func(h *harness) {
		h.t.Helper()
		if budgets, cause := h.budgets(), h.claim().PendingCause; budgets != (Budgets{}) || cause != "" {
			h.t.Fatalf("the budgets are %+v and the claim is pending cause %q, want nothing charged", budgets, cause)
		}
		h.wantCharges()
		if lines := h.logs.lines("the shim never reconnected"); len(lines) != 0 {
			h.t.Fatalf("logged %q, want no retirement of a shim that never reconnected", lines)
		}
		if lines := h.logs.lines("within the reconnect deadline of its hello; the stale-plugin lane relaunches it, uncharged"); len(lines) != 1 {
			h.t.Fatalf("logged %q, want the stale-plugin lane's relaunch at the deadline of the hello once", lines)
		}
	}
	t.Run("the take's turn running at the deadline", func(t *testing.T) {
		h, number := failedHelloThenTheTakesTurn(t)
		h.advance(deadline - time.Second)
		h.wantCalls("Suspend", 0)

		h.advance(time.Second)

		wantLeftToTheLane(h)
		h.wantState(StateWorking)
		h.wantCalls("Suspend", 0)
		h.wantTakesTurnStanding(number, "at the deadline")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.mustReport(nil)
		h.wantFinishedOnce(finishedByReport)
		h.wantCalls("Resume", 1)
		h.wantBudgets(Budgets{})
	})
	t.Run("the task finished over HTTP before the deadline", func(t *testing.T) {
		h, _ := failedHelloThenTheTakesTurn(t)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.mustReport(nil)
		h.wantFinishedOnce(finishedByReport)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the claim holds %+v after the report, want no task", p)
		}
		h.advance(deadline - time.Second)
		h.wantCalls("Suspend", 0)

		h.advance(time.Second)

		wantLeftToTheLane(h)
		h.wantCalls("Resume", 1)
	})
}

// Row 34 before the hello's row: the first decision after a daemon start that restored a claim
// over a provider error's report whose take-back the store refused (ladderOwed) makes that
// take-back and the ladder's step (settle), before the fence and the row. Here that decision is the
// shim's hello, and the store refuses the write again: the take-back and the step stand in memory,
// their write owed to the store (putClaimAndPendingOwed), and the decision goes on to its row, so
// the hello marks its view asked and arms the reconnect deadline whole from itself (helloView). At
// that deadline the process is relaunched, charged nothing, as after a hello whose ask went
// unanswered, and nothing is charged as never reconnected. Once the store takes writes again, the
// claim's next decision writes the take-back and the step (repair).
//
// The negative control is the head before this change (26aa05dc), where settle returned the
// refused write: the hello's decision failed before its row, the view stayed that of a daemon
// start with no hello, and at the start's deadline the live worker was retired, charged a launch
// failure with the cause never_reconnected.
func TestTakeToTheLadderOwedAtADaemonStartIsMadeAtAFirstHelloWhoseWriteTheStoreRefuses(t *testing.T) {
	h := refusedTakeBack(t, 529, false, false)
	h.restart()
	number := h.pending().Number
	h.store.fail("PutClaim", errBoom)
	h.conns.Register(h.token, h.conn)
	err := h.m.Handle(h.ctx, StreamHello{Claim: h.token, Generation: h.generation(), BacklogWhole: true})
	h.m.Wait()
	h.store.fail("PutClaim", nil)
	if err != nil {
		t.Errorf("the hello's decision failed with %v, want it decided past the ladder's refused write", err)
	}
	h.m.mu.Lock()
	view, asked := h.m.connSeen, h.m.helloAsked
	h.m.mu.Unlock()
	if view != viewAsked || !asked {
		t.Errorf("after the hello the view is %d and helloAsked %t, want the view the hello asked (%d)", view, asked, viewAsked)
	}
	h.wantArmedFromNow(TimerRegistration, deadline, "after the hello")
	h.wantLadderOwedLine(true)
	if p := h.pending(); p.Number != number || p.taken() || p.ProviderErrorRetries != 1 || !h.m.waitsForProviderError(p.ID) {
		t.Fatalf("the claim holds %+v, want task %d taken back with one ladder step and its wait armed", p, number)
	}
	if stored, _ := h.store.delivery(h.token); stored.ProviderErrorRetries != 0 {
		t.Fatalf("the store holds %+v while it refused the write, want no ladder step in it", stored)
	}

	h.observe(runtime.Alive)

	stored := h.store.load(h.token)
	if p := stored.Pending; p == nil || p.taken() || p.ProviderErrorRetries != 1 || p.ProviderErrorNote == "" || stored.ProviderErrorRun != 1 {
		t.Fatalf("once the store takes writes again it holds a run of %d and %+v, want the take-back, one ladder step and a run of one written",
			stored.ProviderErrorRun, p)
	}
	h.advance(deadline - time.Second)
	h.wantCalls("Resume", 0)

	h.advance(time.Second)

	if budgets, cause := h.budgets(), h.claim().PendingCause; budgets != (Budgets{}) || cause != "" {
		t.Fatalf("at the hello's deadline the budgets are %+v and the claim is pending cause %q, want nothing charged", budgets, cause)
	}
	h.wantCharges()
	h.wantCalls("Resume", 1)
	if lines := h.logs.lines("the shim never reconnected"); len(lines) != 0 {
		t.Fatalf("logged %q, want no retirement of a shim that never reconnected", lines)
	}
	if lines := h.logs.lines(viewUnansweredRelaunchLine); len(lines) != 1 {
		t.Fatalf("the relaunch logged %q, want the line of a hello no answer followed once", lines)
	}
}
