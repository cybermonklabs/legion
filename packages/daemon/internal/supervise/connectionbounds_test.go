package supervise

// The bounds and the connection (decision 8 of dispatch://INFRA-2811, its state table). Three
// bounds judge the worker by the frames its connection carries: R5's second wait (TimerTurnStart),
// the wait for a take's report (TimerReport), with nothing held as with work held, and R5's first
// wait over a value the worker reported seeing (TimerTake). Each runs only while the claim's
// connection shows the machine every frame of its turns: a close disarms all three, none fires
// while no connection stands, and each is armed again whole once the agent answers the ask a
// reconnect hello makes, or a turn start or end frame arrives on the new connection. Each test is
// one row of the plan's table, named so the check that row names selects it (c2's Take, c5's
// Charge, c3's Relaunch); the row's number heads its comment. The rows of each bound are in a file
// of their own: R5's first wait (connectionbounds_firstwait_test.go), its second wait
// (connectionbounds_secondwait_test.go), the wait for a take's report
// (connectionbounds_report_test.go) and the reconnect deadline
// (connectionbounds_reconnect_test.go).
// This file holds the helpers they share, the rows of all three bounds at once, the take-backs of
// a take awaiting its turn over a view that is not whole, and the stale-plugin relaunch.

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// longReconnect sets the reconnect deadline (the registration deadline's bound, Timeouts.Boot ×
// RegistrationIntervals) past both of R5's bounds, for a row that lets a whole bound pass while no
// connection stands: at the daemon's defaults that deadline comes first, and relaunches the worker.
func longReconnect(d *Deps) { d.Timeouts.Boot = turnStartBound }

// boundsHarness is a claim on the current plugin with R5's two bounds set, ready, every take of it
// driven by the test, and whatever else set changes of its deps.
func boundsHarness(t *testing.T, set ...func(*Deps)) *harness {
	t.Helper()
	h := waitsHarness(t, append([]func(*Deps){currentPlugin}, set...)...)
	h.manualTakes()
	h.reach(StateReady)
	return h
}

// helloOf is the shim's hello on the claim's connection after a close, saying its backlog is
// whole or not, its agent in a turn as streaming says. What the hello asks is answered when the
// test delivers that answer (answerAsk), behind whatever frames the shim replays first.
func (h *harness) helloOf(whole, streaming bool) {
	h.t.Helper()
	h.conn.SetStreaming(streaming)
	h.conns.Register(h.token, h.conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation(), BacklogWhole: whole})
}

// answerAsk is the agent's answer to the last ask of the claim's connection whether it is in a
// turn, as the worker stream delivers it: behind every frame the shim sent before it.
func (h *harness) answerAsk(streaming bool) {
	h.t.Helper()
	h.must(StreamTurnAnswered{Claim: h.token, Streaming: streaming, Ask: h.conn.LastAsk()})
}

// dueOf is when timer kind fires, and whether it is armed.
func (h *harness) dueOf(kind TimerKind) (time.Time, bool) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	a, ok := h.m.timers[kind]
	return a.due, ok
}

// wantArmedFromNow holds that timer kind is armed whole from this moment: bound from now.
func (h *harness) wantArmedFromNow(kind TimerKind, bound time.Duration, when string) {
	h.t.Helper()
	due, ok := h.dueOf(kind)
	if want := h.clock.Now().Add(bound); !ok || !due.Equal(want) {
		h.t.Fatalf("%s the %s timer is armed %t, due %s; want it armed whole from now, due %s", when, kind, ok, due, want)
	}
}

// wantUnarmed holds that no timer of kinds is armed.
func (h *harness) wantUnarmed(when string, kinds ...TimerKind) {
	h.t.Helper()
	for _, kind := range kinds {
		if due, ok := h.dueOf(kind); ok {
			h.t.Fatalf("%s the %s timer is armed, due %s; want it unarmed", when, kind, due)
		}
	}
}

// wantAwaitingItsTurn holds that the take of task number stands, awaiting its turn: nothing took
// it back.
func (h *harness) wantAwaitingItsTurn(number uint64, when string) {
	h.t.Helper()
	if p := h.pending(); p.Number != number || !p.awaitsStart() || p.Interrupted {
		h.t.Fatalf("%s the claim holds %+v, want task %d's take standing, awaiting its turn", when, p, number)
	}
}

// wantRunOnce holds that task number was taken once and finished once, by its report, and that
// nothing of it is left to take: one accepted take, one finish, and no value of it in the bucket.
func (h *harness) wantRunOnce(number uint64) {
	h.t.Helper()
	takes := 0
	for _, took := range h.prompts() {
		if took.Number == number {
			takes++
		}
	}
	if takes != 1 {
		h.t.Fatalf("task %d was taken %d times (%+v), want once", number, takes, h.prompts())
	}
	if p := h.claim().Pending; p != nil && p.Number == number {
		h.t.Fatalf("the claim still holds %+v, want task %d finished", p, number)
	}
	h.wantFinishedOnce(finishedByReport)
	wantAttr(h.t, h.records.of(finishedLine)[0], "task", strconv.FormatUint(number, 10))
	if value, held := h.values.value(h.token); held && value.Number == number {
		h.t.Fatalf("the bucket still holds %+v, want task %d's value deleted", value, number)
	}
}

// Row 14: after a daemon start, a worker's first act that reaches the daemon over HTTP before its
// shim's hello arms none of the three bounds; the hello and the agent's answer arm them. The
// negative control is the commit under this change, where any first act armed them.
func TestTakeOrReportBeforeTheHelloAfterADaemonStartArmsNoBound(t *testing.T) {
	t.Run("a take", func(t *testing.T) {
		h := boundsHarness(t)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		h.restart()

		h.mustTake()

		h.wantUnarmed("after the take before the hello", TimerTurnStart, TimerTake, TimerReport)
		h.connect()
		h.wantUnarmed("at the hello", TimerTurnStart)
		h.answerAsk(false)
		h.wantArmedFromNow(TimerTurnStart, turnStartBound, "at the answer")
	})
	t.Run("a seen report", func(t *testing.T) {
		h := boundsHarness(t)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		h.restart()

		h.seen()

		h.wantUnarmed("after the seen report before the hello", TimerTake)
		h.connect()
		h.wantUnarmed("at the hello", TimerTake)
		h.answerAsk(false)
		h.wantArmedFromNow(TimerTake, takeBound, "at the answer")
	})
	t.Run("a turn's report", func(t *testing.T) {
		h := boundsHarness(t)
		number := h.takeOf("implement the plan")
		h.restart()

		h.reportOf(1, 0)

		h.wantUnarmed("after the report before the hello", TimerTurnStart, TimerReport)
		h.connect()
		h.answerAsk(false)
		h.wantArmedFromNow(TimerTurnStart, turnStartBound, "at the answer")
		h.wantAwaitingItsTurn(number, "at the answer")
	})
}

// Row 24: after a daemon start, a claim restored launching, shim-connected or registered whose
// agent says it is ready over HTTP before its shim's first hello to this daemon. Every claim a
// daemon start restores with a process has no connection the daemon reads until that hello
// (viewNoHello), so the take accepted then arms none of the three bounds; the hello and the
// agent's answer arm them. The negative control is the commit under this change, where such a
// claim started with its view whole, so the take armed R5's second wait over no connection.
func TestTakeOfAClaimRestoredBeforeItsReadyArmsNoBoundBeforeItsHello(t *testing.T) {
	for _, restored := range []ClaimState{StateLaunching, StateShimConnected, StateRegistered} {
		t.Run(string(restored), func(t *testing.T) {
			h := waitsHarness(t, currentPlugin)
			h.manualTakes()
			h.reach(restored)
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
			h.restart()
			if restored != StateRegistered {
				h.register()
			}
			h.ready()

			h.mustTake()

			h.wantUnarmed("after the take before the hello", TimerTurnStart, TimerTake, TimerReport)
			h.connect()
			h.wantUnarmed("at the hello", TimerTurnStart)
			h.answerAsk(false)
			h.wantArmedFromNow(TimerTurnStart, turnStartBound, "at the answer")
		})
	}
}

// retiredByTheOperator is the operator's stop of the claim, which retires it (retire).
func retiredByTheOperator(t *testing.T, h *harness) {
	t.Helper()
	h.must(RequestStop{Claim: h.token})
	h.wantState(StateRetired)
}

// retriedAfterRetirement is the operator's retry of the retired claim, the process it relaunches
// brought up to ready and its take accepted.
func retriedAfterRetirement(h *harness) {
	h.t.Helper()
	h.must(RequestOperatorRetry{Claim: h.token})
	relaunchedAndTaken(h)
}

// relaunchedAndTaken is the process a relaunch started brought up to ready, its take accepted.
func relaunchedAndTaken(h *harness) {
	h.t.Helper()
	h.relaunched()
	h.mustTake()
}

// compactedByTheOperator is the operator's compaction of the claim's session (compactStart), run
// to its end (compactEnded). It keeps the process, and it takes back no take awaiting its turn:
// it is Oh My Pi's handoff, which aborts no turn (departure 10 of dispatch://INFRA-2811's plan).
func compactedByTheOperator(t *testing.T, h *harness) {
	t.Helper()
	if err := h.m.Compact(h.ctx, testCompactInstructions); err != nil {
		t.Fatalf("the compaction answered %v", err)
	}
	h.m.Wait()
}

// Row 26: a take awaiting its turn while the claim's connection does not show every frame of its
// turns — no hello since a close or a daemon start, or no answer since the hello — taken back by a
// let-go that is no reconnect deadline: a retirement, a held suspension's deadline, a death, the
// one the close's own probe finds among them, or a relaunch of a process still running for a stale
// address. Its turn may have run unseen, its start frame lost with the connection, so it goes back
// as a turn cut off, and the next process takes it told its turn on it may have run, as row 8
// takes it back. A stale plugin's relaunch waits for a whole view (row 29) and runs over a cut one
// only at the reconnect deadline, whose take-back
// TestAStaleRelaunchBehindABacklogCutTakesTheTakeBackAsACutTurn holds. The operator's compaction
// takes no take back (departure 10): it is Oh My Pi's handoff, which aborts no turn
// (TestTakeKeptByACompactionOverACutViewIsGivenTheTurnTheAnswerReports). The negative control is
// the commit under this change, where each let-go gave the take back unstarted with no note: only
// the reconnect deadline marked such a take (turnUnseen), the close probed its process before it
// cut the view, and a relaunch's take-back, made before its let-go records anything, read the mark
// alone (interrupted).
func TestTakeAwaitingItsTurnIsTakenBackAsACutTurnWhileTheViewIsCut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cut   func(h *harness)
		letGo func(t *testing.T, h *harness)
		next  func(h *harness)
	}{
		{name: "a retirement after a close", cut: (*harness).reconnectLater, letGo: retiredByTheOperator, next: retriedAfterRetirement},
		{name: "a retirement after a daemon start, before the hello", cut: (*harness).restart, letGo: retiredByTheOperator, next: retriedAfterRetirement},
		{name: "a retirement after a hello no answer followed", cut: func(h *harness) {
			h.reconnectLater()
			h.helloOf(true, false)
		}, letGo: retiredByTheOperator, next: retriedAfterRetirement},
		{name: "a death the close's own probe finds", cut: func(*harness) {}, letGo: func(t *testing.T, h *harness) {
			h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
			h.reconnectLater()
			h.wantState(StateLaunching)
		}, next: relaunchedAndTaken},
		{name: "a death after a daemon start, before the hello", cut: (*harness).restart, letGo: func(t *testing.T, h *harness) {
			h.observe(runtime.Gone)
			h.wantState(StateLaunching)
		}, next: relaunchedAndTaken},
		{name: "a stale address's relaunch after a close", cut: (*harness).reconnectLater, letGo: func(t *testing.T, h *harness) {
			h.observe(runtime.StaleAddress)
			h.wantState(StateLaunching)
		}, next: relaunchedAndTaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := boundsHarness(t)
			number := h.takeOf("implement the plan")
			tc.cut(h)

			tc.letGo(t, h)

			h.wantTakenBackWithItsNote(number)
			h.wantTakenOnceMore(number, tc.next)
		})
	}
	t.Run("a held suspension's deadline after a close", func(t *testing.T) {
		h := boundsHarness(t)
		number := h.takeOf("implement the plan")
		h.hostsRun()
		if err := h.handle(operatorSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the suspension answered %v, want it held for the report of the take's turn", err)
		}
		h.reportOf(1, 0)
		h.wantAwaitingItsTurn(number, "after the given run's report")
		h.reconnectLater()

		h.advance(testStop)

		h.wantState(StateSuspended)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, func(h *harness) {
			h.must(RequestResume{Claim: h.token})
			relaunchedAndTaken(h)
		})
	})
}

// heldStaleRelaunch is a stale-plugin relaunch of the claim held for the end of a turn of the
// host's own, and run at that end (holdStaleRelaunch, afterTurn).
func heldStaleRelaunch(t *testing.T, h *harness) {
	t.Helper()
	staleToTheDaemon(h)
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
	h.wantCalls("Resume", 0)
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantState(StateLaunching)
}

// secondWaitGivesBackACutTurn is R5's second wait over a take marked as one whose turn may have
// run unseen (blindTurnEnded, Machine.turnUnseen), with the agent's answer making the view whole:
// the bound passes with no turn started, the worker is charged, and the take goes back as a cut
// turn with its note (turnStartBoundOver, takeBack), the process kept.
func secondWaitGivesBackACutTurn(t *testing.T, h *harness, number uint64) {
	t.Helper()
	h.reconnectLater()
	h.helloOf(true, false)
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.answerAsk(false)
	h.wantAwaitingItsTurn(number, "after the answer")
	h.advance(turnStartBound)
	h.wantCharges(WaitTurnStart)
	if p := h.pending(); p.Number != number || p.taken() || !p.Interrupted {
		t.Fatalf("the second wait left %+v, want task %d taken back with the note of a cut turn", p, number)
	}
}

// Row 35: R5's second wait took back a take marked as one whose turn may have run unseen, and
// kept the process (secondWaitGivesBackACutTurn). A let-go of that process then comes before the
// next take — the reconnect deadline of a hello no answer followed, a death, a retirement or a
// held stale-plugin relaunch. Its record says the task's turn was cut off (EndedWork.TaskTurn),
// as it says for a take-back the let-go's own decision makes (interrupted), so the next process
// takes the task told its turn on it may have run. The claim ran a turn first, so the let-go
// writes its record. The negative control is the take-back with no mark for that record
// (Machine.cutTurnOf, takeBackInMemory): the record says the agent was in no turn, and its
// sentence stands in place of the cut turn's note, so the next take carries none.
func TestTakeTheSecondWaitGaveBackAsACutTurnKeepsItsNoteThroughALaterLetGo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		letGo func(t *testing.T, h *harness)
		next  func(h *harness)
	}{
		{"the reconnect deadline of a hello no answer followed", func(t *testing.T, h *harness) {
			h.reconnectLater()
			h.helloOf(true, false)
			h.advance(deadline)
			h.wantState(StateLaunching)
		}, relaunchedAndTaken},
		{"a death", func(t *testing.T, h *harness) {
			h.observe(runtime.Gone)
			h.wantState(StateLaunching)
		}, relaunchedAndTaken},
		{"a retirement", retiredByTheOperator, retriedAfterRetirement},
		{"a held stale-plugin relaunch", heldStaleRelaunch, relaunchedAndTaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := boundsHarness(t)
			h.takeRunsItsOwnTurn("the first task", 1, false)
			number := h.takeOf("implement the plan")
			secondWaitGivesBackACutTurn(t, h, number)

			tc.letGo(t, h)

			if ended := h.claim().EndedWork; !ended.TaskTurn {
				t.Fatalf("the let-go recorded %+v, want the task's turn recorded as cut off", ended)
			}
			h.wantTakenOnceMore(number, tc.next)
		})
	}
}

// Row 36: R5's second wait took back a take marked as one whose turn may have run unseen, and
// kept the process (secondWaitGivesBackACutTurn); the worker takes the task again. A death before
// that take's turn starts cuts off no turn of the task: the worker ran nothing of that take, and
// the view showed every frame of its turns, so the let-go's record says the task's turn was not
// cut off (EndedWork.TaskTurn). The mark the second wait's take-back left (Machine.cutTurnOf) ends
// with the acceptance of the task's next take (accept). The claim ran a turn first, so the let-go
// writes its record. The negative control is the head before this change (3399439b), where only
// letGo cleared the mark, and the record said the task's turn was cut off for a take that never
// ran.
func TestTakeAcceptedAgainAfterTheSecondWaitGaveItBackAsACutTurnIsInNoTurnCutOffAtALaterDeath(t *testing.T) {
	h := boundsHarness(t)
	h.takeRunsItsOwnTurn("the first task", 1, false)
	number := h.takeOf("implement the plan")
	secondWaitGivesBackACutTurn(t, h, number)
	h.mustTake()
	if p := h.pending(); p.Number != number || !p.awaitsStart() {
		t.Fatalf("the take again left %+v, want task %d's take awaiting its turn", p, number)
	}

	h.observe(runtime.Gone)

	h.wantState(StateLaunching)
	ended := h.claim().EndedWork
	if ended.Cause == "" {
		t.Fatalf("the let-go recorded %+v, want a record of the process's end", ended)
	}
	if ended.TaskTurn {
		t.Fatalf("the let-go recorded %+v, want no turn of the task recorded as cut off", ended)
	}
}

// Row 37: R5's second wait sends a take awaiting its turn, marked as one whose turn may have run
// unseen, back as a cut turn, and keeps the process (secondWaitGivesBackACutTurn). The daemon then
// restarts before any let-go, and a let-go of the process comes before the task's next take: the
// restored claim's reconnect deadline, a death, or a held stale-plugin relaunch. The let-go's
// record says the task's turn was cut off (EndedWork.TaskTurn), as it says with no restart
// between, so the next process takes the task told its turn on it may have run. The claim ran a
// turn first, so the let-go writes its record. The negative control is the head 4ce1b85d80, where
// the cut-turn mark (Machine.cutTurnOf) is memory only: the restored machine records no cut turn,
// the record says the agent was in no turn, and the next take carries no note.
func TestTakeTheCutTurnMarkSurvivesADaemonRestartBeforeALaterLetGo(t *testing.T) {
	takeBacks := []struct {
		name     string
		takeBack func(t *testing.T, h *harness, number uint64)
	}{
		{"the second wait", secondWaitGivesBackACutTurn},
	}
	letGos := []struct {
		name  string
		letGo func(h *harness)
	}{
		{"the reconnect deadline of the restored claim", func(h *harness) { h.advance(deadline) }},
		{"a death", func(h *harness) { h.observe(runtime.Gone) }},
		{"a held stale-plugin relaunch", func(h *harness) {
			h.connect()
			h.answerAsk(false)
			heldStaleRelaunch(h.t, h)
		}},
	}
	for _, back := range takeBacks {
		t.Run(back.name, func(t *testing.T) {
			for _, tc := range letGos {
				t.Run(tc.name, func(t *testing.T) {
					h := boundsHarness(t)
					h.takeRunsItsOwnTurn("the first task", 1, false)
					number := h.takeOf("implement the plan")
					back.takeBack(t, h, number)
					if p := h.pending(); p.Number != number || p.taken() || !p.Interrupted {
						t.Fatalf("%s left %+v, want task %d taken back with the note of a cut turn", back.name, p, number)
					}
					h.restart()

					tc.letGo(h)

					h.wantState(StateLaunching)
					if ended := h.claim().EndedWork; !ended.TaskTurn {
						t.Fatalf("the let-go recorded %+v, want the task's turn recorded as cut off", ended)
					}
					h.wantTakenOnceMore(number, relaunchedAndTaken)
				})
			}
		})
	}
}

// Row 28: a take awaiting its turn, its turn's start frame lost in the socket's send buffer as the
// connection broke. The shim keeps only a frame whose write failed, so its hello says the backlog
// is whole, and the replay carries that turn's end alone, a turn this daemon never saw begin
// (blindTurnEnded); the agent then answers that it is in no turn, which makes the view whole. A
// let-go before that turn's report — a retirement or a death — takes the take back as a cut turn,
// and the next process takes it told its turn on it may have run: the blind end over a take
// awaiting its turn marked it as one whose turn may have run unseen (Machine.turnUnseen). The
// negative control is the commit under this change, where the blind end marked nothing, and the
// let-go over the whole view gave the take back unstarted with no note.
//
// A migrated worker's take awaiting its turn, on a plugin below TaskTakeContract (waitNone), is
// marked as a current one's is (markTurnUnseen: one test at every site that marks), so a death
// after the blind end takes it back as a cut turn too. Its negative control is the head before
// this change (2da324b8), where the blind end marked only a take in waitTake, and the death gave
// the migrated take back unstarted with no note.
func TestTakeAwaitingItsTurnBehindAnEndNeverSeenBegunIsTakenBackAsACutTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		letGo func(t *testing.T, h *harness)
		next  func(h *harness)
	}{
		{"a retirement", retiredByTheOperator, retriedAfterRetirement},
		{"a death", func(t *testing.T, h *harness) {
			h.observe(runtime.Gone)
			h.wantState(StateLaunching)
		}, relaunchedAndTaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := boundsHarness(t)
			number := h.takeOf("implement the plan")
			h.reconnectLater()
			h.helloOf(true, false)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.answerAsk(false)
			h.wantAwaitingItsTurn(number, "after the answer")

			tc.letGo(t, h)

			h.wantTakenBackWithItsNote(number)
			h.wantTakenOnceMore(number, tc.next)
		})
	}
	t.Run("a migrated worker's take, a death", func(t *testing.T) {
		h := restoreMigrated(t, false)
		number := h.pending().Number
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.answerAsk(false)
		h.wantAwaitingItsTurn(number, "after the answer")

		h.observe(runtime.Gone)
		h.wantState(StateLaunching)

		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, (*harness).relaunchedTakes)
	})
}

// Row 28's mark is the take's, not the claim's: the next accepted take clears it (accept). A take
// marked by an end never seen begun runs its own turn after it, and its report finishes the task;
// the next task's take then awaits its turn over a whole view, and a death comes before that turn
// starts. The let-go's record says no turn of the task was cut off (EndedWork.TaskTurn), and the
// next task goes back unstarted, with no note: the worker ran nothing of its take. The negative
// control is accept with no clear of the mark (setTurnUnseen): the first task's mark stands over
// the next task's take, and the death takes it back as a cut turn.
func TestTakeOfTheNextTaskInheritsNoMarkOfAnEndNeverSeenBegun(t *testing.T) {
	h := boundsHarness(t)
	first := h.takeOf("the first task")
	h.reconnectLater()
	h.helloOf(true, false)
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.answerAsk(false)
	h.wantAwaitingItsTurn(first, "after the answer")
	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.reportOf(2, first)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v after the report of task %d's turn, want it finished", p, first)
	}
	second := h.takeOf("the second task")
	h.wantAwaitingItsTurn(second, "after the second task's take")

	h.observe(runtime.Gone)

	h.wantState(StateLaunching)
	ended := h.claim().EndedWork
	if ended.Cause == "" {
		t.Fatalf("the let-go recorded %+v, want a record of the process's end", ended)
	}
	if ended.TaskTurn {
		t.Fatalf("the let-go recorded %+v, want no turn of the task recorded as cut off", ended)
	}
	if p := h.pending(); p.Number != second || p.taken() || p.Interrupted {
		t.Fatalf("the death left %+v, want task %d given back unstarted, with no note of a cut turn", p, second)
	}
}

// Row 28's mark says nothing once the take's own turn has started (turnMayHaveRunUnseen), and the
// record of a let-go clears it then (recordEnd). A take marked by an end never seen begun runs its
// own turn after it, its start and end frames seen, and the process dies before that turn's report
// comes. No turn was running, and the turn of the take ran whole on a connection that showed every
// frame of it, so the let-go's record says no turn of the task was cut off (EndedWork.TaskTurn).
// The negative control is recordEnd with no clear of the mark (setTurnUnseen): the mark of the end
// never seen begun stands, and the record says the task's turn was cut off.
func TestTakeWhoseOwnTurnRanAfterAnEndNeverSeenBegunIsInNoTurnCutOffAtADeath(t *testing.T) {
	h := boundsHarness(t)
	h.takeRunsItsOwnTurn("the first task", 1, false)
	number := h.takeOf("implement the plan")
	h.reconnectLater()
	h.helloOf(true, false)
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.answerAsk(false)
	h.wantAwaitingItsTurn(number, "after the answer")
	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if p := h.pending(); p.Number != number || !p.startRecorded() {
		t.Fatalf("the take's own turn left %+v, want task %d's turn run, its report still to come", p, number)
	}

	h.observe(runtime.Gone)

	h.wantState(StateLaunching)
	ended := h.claim().EndedWork
	if ended.Cause == "" {
		t.Fatalf("the let-go recorded %+v, want a record of the process's end", ended)
	}
	if ended.TaskTurn {
		t.Fatalf("the let-go recorded %+v, want no turn of the task recorded as cut off", ended)
	}
}

// staleToTheDaemon makes the claim's recorded plugin contract an earlier one than this daemon's,
// which can relaunch onto its own: the stale-plugin lane's case (refreshPlugin).
func staleToTheDaemon(h *harness) {
	h.deps.PluginContract, h.m.deps.PluginContract, h.m.deps.PluginRelaunchable = testTakeContract+1, testTakeContract+1, true
}

// Row 29: a claim on a stale plugin holding a take, whose connection closed since the agent's last
// answer: a sweep's post relaunches nothing and logs that the relaunch waits for the view
// (refreshPlugin), and a post after a hello no answer followed does the same. The first post after
// the agent's answer on the new connection decides it; with neither answer nor turn frame, the
// reconnect deadline's own path runs at it, and takes the take back as a cut turn. The negative
// control is the commit under this change, where the sweep relaunched the process over the cut
// view, so a take whose own turn may have been running unseen was taken back and run again.
func TestStalePluginRelaunchOfAClaimHoldingATakeWaitsForAWholeView(t *testing.T) {
	postedOverACut := func(t *testing.T) (*harness, uint64) {
		t.Helper()
		h := boundsHarness(t)
		number := h.takeOf("implement the plan")
		h.reconnectLater()
		staleToTheDaemon(h)

		h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

		h.wantCalls("Resume", 0)
		if lines := h.logs.lines(relaunchWaitsForViewLine, "trigger=sweep"); len(lines) != 1 {
			t.Fatalf("logged %q, want the relaunch's wait for the view once", lines)
		}
		h.wantAwaitingItsTurn(number, "after the sweep over the cut view")
		return h, number
	}
	t.Run("the agent's answer on the next connection, then a sweep", func(t *testing.T) {
		h, number := postedOverACut(t)
		h.helloOf(true, false)
		h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
		h.wantCalls("Resume", 0)
		h.answerAsk(false)

		h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

		h.wantCalls("Resume", 1)
		if p := h.pending(); p.Number != number || p.taken() || p.Interrupted {
			t.Fatalf("the relaunch over the whole view left %+v, want task %d given back unstarted", p, number)
		}
	})
	t.Run("no hello by the reconnect deadline", func(t *testing.T) {
		h, number := postedOverACut(t)

		h.advance(deadline)

		h.wantCalls("Resume", 1)
		if cause := h.claim().PendingCause; cause != CauseNeverReconnected {
			t.Fatalf("the relaunch is pending cause %q, want %q", cause, CauseNeverReconnected)
		}
		h.wantTakenBackWithItsNote(number)
	})
}

// Row 29a: a claim on a stale plugin holding no take, whose connection closed: a sweep's post
// relaunches it onto the current plugin, as before, since only a take the daemon accepted can have
// a turn running unseen. Two cases: idle, holding no pending task; and ready, holding a pending task
// no take holds, which waits for the process the relaunch starts. Both pass on the commit under this
// change too; they pin that the guard of the view reads a take, not a pending task.
func TestStalePluginRelaunchOfAClaimHoldingNoTakeRunsOverAClosedConnection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim func(t *testing.T) *harness
	}{
		{"idle, holding no pending task", func(t *testing.T) *harness {
			h := waitsHarness(t, currentPlugin)
			h.reach(StateIdle)
			if p := h.claim().Pending; p != nil {
				t.Fatalf("the idle claim holds %+v, want no pending task", p)
			}
			return h
		}},
		{"ready, holding a pending task no take holds", func(t *testing.T) *harness {
			h := boundsHarness(t)
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
			return h
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.claim(t)
			h.reconnectLater()
			staleToTheDaemon(h)

			h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

			h.wantCalls("Resume", 1)
			if lines := h.logs.lines(relaunchWaitsForViewLine); len(lines) != 0 {
				t.Fatalf("logged %q, want no wait for the view over a claim holding no take", lines)
			}
			if p := h.claim().Pending; p != nil && (p.taken() || p.Interrupted) {
				t.Fatalf("the relaunch left %+v, want the untaken task waiting for the process it starts", p)
			}
		})
	}
}

// handoffAnswerLost is the stream's failure of a handoff whose connection closed before Oh My Pi
// answered it: no answer, so the handoff may still run in the session.
var handoffAnswerLost = errors.New("worker-stream: handoff: worker stream closed before it was answered")

// shortTurnStart is a second wait configured far shorter than a handoff can take.
func shortTurnStart(d *Deps) { d.Timeouts.TurnStart = 30 * time.Second }

// Row H4 of departure 10: the take awaits its turn when the handoff is asked for, and the
// handoff's answer is lost with the connection: the handoff may still run in the session, up to
// runtime.CompactTimeout from the ask, and Oh My Pi answers no ask whether it is in a turn until it
// is over, since the handoff and get_state share its serial queue. So the take is protected until
// that one fixed deadline: the reconnect deadline of the close and of the next hello is the later
// of its usual bound and that deadline, the hello's ask stays pending on its own connection past
// the RPC timeout, and the second wait, once the answer makes the view whole, cannot pass before
// the deadline either. Before it nothing charges, relaunches or offers the task again, and the
// late answer, the take's turn and its report run the task once. With no answer and no frame by
// the deadline the ordinary recovery runs: the process is relaunched, charged nothing, and the
// take goes back as a cut turn. A claim the handoff does not protect keeps its usual deadline. A
// later connection's hello cancels the first connection's ask.
func TestChargeNoneForATakeAcrossACompactionLongerThanTheSecondBound(t *testing.T) {
	protected := func(t *testing.T) (*harness, uint64, time.Time) {
		t.Helper()
		h := boundsHarness(t, shortTurnStart)
		number := h.takeOf("implement the plan")
		until := h.clock.Now().Add(runtime.CompactTimeout)
		h.conn.FailCompact(handoffAnswerLost)
		if err := h.m.Compact(h.ctx, testCompactInstructions); !errors.Is(err, handoffAnswerLost) {
			t.Fatalf("compact answered %v, want the lost answer", err)
		}
		h.m.Wait()
		h.wantAwaitingItsTurn(number, "after the handoff's answer was lost")
		h.reconnectLater()
		t.Cleanup(h.conn.HoldGetState())
		h.helloOf(true, false)
		waitStatesHeld(t, h.conn, 1, "the hello's one ask on the new connection")
		return h, number, until
	}
	t.Run("the answer comes past the usual bounds and within the handoff's", func(t *testing.T) {
		h, number, until := protected(t)
		writes := len(h.values.written())

		h.advance(deadline + time.Second)

		h.wantCharges()
		h.wantCalls("Resume", 0)
		h.wantAwaitingItsTurn(number, "past the usual reconnect deadline")
		if got := len(h.values.written()); got != writes {
			t.Fatalf("%d values were written before the handoff's deadline, want none", got-writes)
		}
		if held := h.conn.StatesHeld(); held != 1 {
			t.Fatalf("%d asks wait past the RPC timeout, want the hello's one still pending", held)
		}
		h.answerAsk(false)
		if due, ok := h.dueOf(TimerTurnStart); !ok || !due.Equal(until) {
			t.Fatalf("the second wait is armed %t, due %s; want it due at the handoff's deadline, %s", ok, due, until)
		}
		h.advance(30 * time.Second)
		h.wantCharges()
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(h.worker.sequence+1, number)
		h.wantRunOnce(number)
		h.wantCharges()
	})
	t.Run("a close of the hello's connection cancels its ask", func(t *testing.T) {
		h, number, _ := protected(t)
		first := h.conn
		h.reconnectLater()
		waitStatesHeld(t, first, 0, "no ask on the closed connection")
		h.wantAwaitingItsTurn(number, "after the close")
	})
	t.Run("no answer and no frame by the handoff's deadline", func(t *testing.T) {
		h, number, until := protected(t)

		h.advance(until.Sub(h.clock.Now()))

		h.wantCalls("Resume", 1)
		h.wantCharges()
		h.wantTakenBackWithItsNote(number)
	})
	t.Run("a claim the handoff does not protect keeps its usual deadline", func(t *testing.T) {
		h := boundsHarness(t, shortTurnStart)
		number := h.takeOf("implement the plan")
		h.reconnectLater()
		h.helloOf(true, false)

		h.advance(deadline)

		h.wantCalls("Resume", 1)
		h.wantTakenBackWithItsNote(number)
	})
}

// waitStatesHeld waits until want asks wait at conn's gate (fake.Conn.HoldGetState): an ask the
// machine makes on a goroutine of its own reaches the connection a moment after its decision.
func waitStatesHeld(t *testing.T, conn *fake.Conn, want int, what string) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for conn.StatesHeld() != want && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if held := conn.StatesHeld(); held != want {
		t.Fatalf("%d asks wait at the connection, want %d: %s", held, want, what)
	}
}
