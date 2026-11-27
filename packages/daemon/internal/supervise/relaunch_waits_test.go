package supervise

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// sendsWhole fails unless the process's next prompt, after do, is want whole.
func sendsWhole(t *testing.T, h *harness, want string, do ...Event) {
	t.Helper()
	sent := len(h.prompts())
	for _, ev := range do {
		h.must(ev)
	}
	if got := h.wantPrompts(sent + 1)[sent].Message; got != want {
		t.Fatalf("the running process was sent\n%q\nwant\n%q", got, want)
	}
}

// A sweep report that finds the claim's process running while the relaunch of a death died
// decided of it still waits for its turn, in the wake pacing or behind the pause's gate, ends
// that death, as a read does (reportedProcess), and the waiting relaunch goes with it (judge,
// dropOutlivedRelaunch; dispatch://INFRA-1117/comment/80daf46d-7ffd-4020-b51f-63fedd02182e): the
// wake is dropped, its place in the pacing or the gate's own stop of the claim is given up, and
// the process is watched again (watchAgain), so the daemon replaces no live process and nothing
// tells the death. Where the death took the task back, the task goes to the process again behind
// the one sentence of a task sent again, no relaunch starts once the turn the dropped place
// would have taken has passed, or once the pause has ended, and a turn that runs the task to its
// result ends its sends, so the next task is its text alone: on the daemon that judged the death,
// after a restart whose boot's replay parked the relaunch again, and over a relaunch the pause's
// gate held. Those rows check the whole text of each prompt the process gets after the report. A
// process still coming up gets back the registration deadline the park took from it. Where the
// death put an earlier launch's untold record aside, TestAFalseDeathErasesNoEarlierEndStillUntold's
// rows whose relaunch waits hold that the report puts that record back, untold, in its own words.
//
// The negative controls: judge's order as it was, reportedProcess run before the launchWaits
// guard and the wake left standing, fails every row, the claim still holding the relaunch; judge
// dropping only a relaunch the pacing holds fails the pause's row the same way; and
// dropOutlivedRelaunch leaving the gate's stop posted fails the pause's row at that stop.
func TestASweepReportThatFindsTheProcessRunningDropsTheWaitingRelaunchOfItsDeath(t *testing.T) {
	const task, next = "implement the plan", "the next task"
	// parked is the place in the pacing the death's relaunch waits at.
	parked := func(t *testing.T, h *harness) *wakepace.Place {
		t.Helper()
		h.wantWaitingWake(WakeLaunch)
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return h.m.wake.place
	}
	// report is the sweep's report of the process running, the agent still in any turn it is in.
	// It fails unless the report dropped the relaunch and ended the death in the claim and in the
	// store, its record told by no delivery.
	report := func(t *testing.T, h *harness) {
		t.Helper()
		h.conn.SetStreaming(true)
		h.observe(runtime.Alive)
		h.conn.SetStreaming(false)
		h.m.mu.Lock()
		held := h.m.wake
		h.m.mu.Unlock()
		if held != nil {
			t.Fatalf("the claim holds a %s wake (pause %d) after the sweep found its process running, want the death's relaunch dropped",
				held.kind, held.pause)
		}
		for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
			if e := c.EndedWork; c.DeathCharged || e.Died || e.Tells() || e.ToldBy != "" {
				t.Fatalf("%s holds deathCharged=%v over the record %+v, want the death over and its record told by no delivery",
					whose, c.DeathCharged, e)
			}
		}
	}
	// running is report over a relaunch the pacing holds, and fails unless its place was given up.
	running := func(t *testing.T, h *harness, place *wakepace.Place) {
		t.Helper()
		report(t, h)
		if _, out := place.Let(); out || !place.Estimate().IsZero() {
			t.Fatal("the dropped relaunch still holds its place in the pacing, want the place given up")
		}
	}
	// sentAgain fails unless the stored record names the pending task sent again, unmarked.
	sentAgain := func(t *testing.T, h *harness) {
		t.Helper()
		if p, e := h.pending(), h.store.load(h.token).EndedWork; e.SentAgain != p.ID || p.Interrupted || p.OutOfMemory {
			t.Fatalf("the report left the task %+v under the stored record %+v, want it named the task sent again, unmarked", p, e)
		}
	}
	for _, restart := range []bool{false, true} {
		name := "the death's take-back landed"
		if restart {
			name += "; a restart, whose boot's replay parks the relaunch again"
		}
		t.Run(name, func(t *testing.T) {
			h := pacedHarness(t, wakeRate)
			h.reach(StateWorking)
			queueAhead(h, 1)
			h.observe(runtime.Gone)
			if restart {
				restartBehindAWave(t, h)
				h.must(RecordedDeath{Claim: h.token, Generation: h.claim().Generation})
			}
			place := parked(t, h)
			starts, due := h.starts(h.token), place.Estimate()
			running(t, h, place)
			sentAgain(t, h)
			h.advance(due.Sub(h.clock.Now()) + wakeInterval)
			h.wantStarts(h.token, starts)
			h.wantState(StateWorking)
			sendsWhole(t, h, sentAgainTask+task, StreamTurnEnd{Claim: h.token})
			sendsWhole(t, h, next, StreamTurnStart{Claim: h.token}, StreamTurnEnd{Claim: h.token}, RequestDeliver{Claim: h.token, Task: next})
		})
	}
	t.Run("the death's take-back landed; the pause's gate holds the relaunch, and the pause ends", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate))
		h.reach(StateWorking)
		flag.set(7)
		h.observe(runtime.Gone)
		h.m.mu.Lock()
		gated := h.m.wake != nil && h.m.wake.pause == 7
		h.m.mu.Unlock()
		if !gated || !h.armed(TimerPause) {
			t.Fatal("the death's relaunch is not held by the pause's gate with the gate's stop posted, want both")
		}
		starts := h.starts(h.token)
		report(t, h)
		if h.armed(TimerPause) {
			t.Fatal("the gate's stop of the claim is still posted, want it given up with the dropped relaunch")
		}
		sentAgain(t, h)
		flag.set(0)
		h.must(ResumePause{Claim: h.token, Pause: 7})
		h.advance(testProbe)
		h.wantStarts(h.token, starts)
		h.wantState(StateWorking)
		stops.wantStops(t)
		sendsWhole(t, h, sentAgainTask+task, StreamTurnEnd{Claim: h.token})
		sendsWhole(t, h, next, StreamTurnStart{Claim: h.token}, StreamTurnEnd{Claim: h.token}, RequestDeliver{Claim: h.token, Task: next})
	})
	t.Run("a process still coming up", func(t *testing.T) {
		h := pacedHarness(t, wakeRate)
		h.reach(StateRegistered)
		queueAhead(h, 1)
		h.observe(runtime.Gone)
		place := parked(t, h)
		if h.armed(TimerRegistration) {
			t.Fatal("the park left the registration deadline of the process its relaunch replaces armed, want it disarmed")
		}
		running(t, h, place)
		h.wantState(StateRegistered)
		if !h.armed(TimerRegistration) {
			t.Fatal("no registration deadline watches the process the dropped relaunch kept, want it armed again")
		}
	})
}

// A relaunch the pause's gate held that a running report dropped (dropOutlivedRelaunch) takes the
// gate's own stop of the claim with it, so the live claim is the pause's own stop's to end: the
// request the daemon's pause_stop row sends, and sends again while the claim holds it (By
// released, the pause's id, no deadline). That request stops the claim through the pause's stop
// transaction: an idle claim at once, and a claim in a turn at that turn's end, whether the stop
// came after the report or was held before it, since the drop leaves a held stop standing. The
// claim holds no task, so the stop owes the role no launch: none is owed for the relaunch the
// report dropped, and no launch starts while the pause stands. (A claim that still holds the task
// a death took back is owed a launch for that task at the resume, decision 6: holdsTask.)
//
// The negative controls: dropOutlivedRelaunch leaving the relaunch on the machine fails every row,
// the stop owing the role that launch; and dropOutlivedRelaunch dropping a held suspension with
// the relaunch fails the row whose stop was held before the report, the claim left running.
func TestAPausesOwnStopEndsTheLiveClaimWhoseGateHeldRelaunchARunningReportDropped(t *testing.T) {
	// gated stands the claim idle, or in a turn of its own over no task, while the project is
	// paused: its process is reported gone, and the pause's gate holds the death's relaunch.
	gated := func(t *testing.T, inTurn bool) (*harness, *stopTransaction) {
		t.Helper()
		h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate))
		h.reach(StateIdle)
		h.reportActivity()
		if inTurn {
			h.must(StreamTurnStart{Claim: h.token})
		}
		flag.set(7)
		h.observe(runtime.Gone)
		h.m.mu.Lock()
		held := h.m.wake != nil && h.m.wake.pause == 7
		h.m.mu.Unlock()
		if !held || !h.armed(TimerPause) || !h.claim().DeathCharged || h.claim().Pending != nil {
			t.Fatalf("the death left the claim holding %+v, want its relaunch held by the pause's gate with that gate's stop posted, the death charged and no task held",
				h.claim())
		}
		return h, stops
	}
	// running is the sweep's report of the process running, which ends the death and drops the
	// relaunch with the gate's stop.
	running := func(t *testing.T, h *harness) {
		t.Helper()
		h.conn.SetStreaming(true)
		h.observe(runtime.Alive)
		h.conn.SetStreaming(false)
		if h.claim().DeathCharged || h.armed(TimerPause) {
			t.Fatal("the report left the death charged or the gate's stop posted, want both gone with the dropped relaunch")
		}
	}
	// held is the pause's own stop meeting the claim in its turn: held for the turn's end.
	held := func(t *testing.T, h *harness) {
		t.Helper()
		if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the pause's stop of the claim in its turn answered %v, want it held for the turn's end", err)
		}
	}
	// stopped fails unless the claim is suspended for the pause through its stop transaction,
	// owing no launch, and nothing starts once the probe interval has passed.
	stopped := func(t *testing.T, h *harness, stops *stopTransaction, starts int) {
		t.Helper()
		h.wantState(StateSuspended)
		if by := h.claim().SuspendedBy; by != SuspendedByReleased {
			t.Fatalf("the claim is suspended by %q, want released", by)
		}
		stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: false}, landed: true})
		h.advance(testProbe)
		h.wantStarts(h.token, starts)
	}
	t.Run("idle: the pause's stop after the report stops the claim at once", func(t *testing.T) {
		h, stops := gated(t, false)
		starts := h.starts(h.token)
		running(t, h)
		h.must(pauseStop(h.token, 7))
		stopped(t, h, stops, starts)
	})
	t.Run("in a turn: the pause's stop after the report stops the claim at the turn's end", func(t *testing.T) {
		h, stops := gated(t, true)
		starts := h.starts(h.token)
		running(t, h)
		held(t, h)
		h.wantState(StateWorking)
		stops.wantStops(t)
		h.must(StreamTurnEnd{Claim: h.token})
		stopped(t, h, stops, starts)
	})
	t.Run("in a turn: the pause's stop held before the report stops the claim at the turn's end", func(t *testing.T) {
		h, stops := gated(t, true)
		starts := h.starts(h.token)
		held(t, h)
		running(t, h)
		if pause, holds := h.heldPause(); !holds || pause != 7 {
			t.Fatalf("the report left held pause %d (held %v), want the pause's stop still held for the turn", pause, holds)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		stopped(t, h, stops, starts)
	})
}

// A task a false death took back goes to the process a read found running behind the one
// sentence of a task sent again (EndedWork.SentAgain), and it keeps that sentence until a turn
// runs it to its result, the process is let go or the task retires
// (dispatch://INFRA-1117/comment/71ee0bb6-4a7e-4e7c-839e-43bf45039938). A provider error that
// ends the turn of the task sent again takes the task back under a new id (takeBackPending), and
// the mark moves with the id, so the ladder's send carries the sentence, then the provider's. A
// send that rewrites the task under a new id for news of the approved design the record no
// longer reads as it did (currentNews, rewritePending) moves the mark with the id too, so the
// rewritten task carries the sentence. A second false death before the task's turn keeps the
// mark: in the turn the process was in, on the record that death writes (recordEnd); in no turn,
// cutting off nothing, on the record of the first death it types (retype), the agent then
// answering the task's prompt busy, which takes the task back under a new id; and through the
// read that ends it, which took nothing back (reportedProcess). Each row checks the whole text of
// each prompt the process gets after the first read, and that the result of the turn that runs
// the task ends the mark, in the claim and in the store, while the task is still the claim's until
// that turn ends, so a restart between the two reads no mark back; and the next task is its text
// alone. A newer task that takes the place of the task sent again while the provider error's
// ladder holds it retires that task with no result to end the mark, and the retirement ends it.
//
// The negative controls: takeBackPending leaving the mark on the old id fails the provider
// error's rows, the newer task's and the row in no turn; rewritePending leaving it there fails
// the news row; recordEnd writing its record without the mark fails the row in the turn, and
// recordEnd clearing the mark, as it did, fails both rows of a second false death; retype
// clearing the mark fails the row in no turn; reportedProcess naming only a task its own death
// took back fails both rows of a second false death; promptResulted leaving the mark at the
// turn's result fails every row that runs the task to its result, and promptResulted clearing it
// without writing it fails, at the store, each of those rows whose result ends no run of provider
// errors, which writes the claim anyway; and retirePending leaving the mark fails the newer
// task's row.
func TestATaskSentAgainKeepsItsSentenceUntilATurnRunsItToItsResult(t *testing.T) {
	const task, next = "implement the plan", "the next task"
	// diesAgain is a second death of the process, read neither way, whose relaunch the lifecycle
	// recheck refuses, so the claim stands over the process it judged dead.
	diesAgain := func(t *testing.T, h *harness, ended runtime.Locator) {
		t.Helper()
		h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
		if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}}); err == nil {
			t.Fatal("the second death's relaunch was taken, want the lifecycle recheck's refusal of it")
		}
		h.m.deps.Store = h.store
		if c := h.claim(); !c.DeathCharged || !c.judgedDeath() {
			t.Fatalf("the second death left deathCharged=%v over the record %+v, want it decided", c.DeathCharged, c.EndedWork)
		}
	}
	// named fails unless the claim and the store each hold a task and name sent again the id
	// want gives for it: the task's own, or "" for none.
	named := func(t *testing.T, h *harness, after string, want func(p Delivery) string) {
		t.Helper()
		for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
			if c.Pending == nil {
				t.Fatalf("%s holds no task after %s, want the task still held", whose, after)
			}
			if got, w := c.EndedWork.SentAgain, want(*c.Pending); got != w {
				t.Fatalf("%s names %q sent again over the task %s after %s, want %q", whose, got, c.Pending.ID, after, w)
			}
		}
	}
	itsOwn := func(p Delivery) string { return p.ID }
	none := func(Delivery) string { return "" }
	// ran is the result and then the end of the turn of the task sent again, a restart between
	// the two where restart is, and fails unless the result ended the mark in the claim and in the
	// store, the task still held, and the next task goes as its text alone.
	ran := func(t *testing.T, h *harness, restart bool) {
		t.Helper()
		h.must(StreamPromptResult{Claim: h.token, DeliveryID: h.pending().ID, AgentInvoked: true})
		named(t, h, "the result of the turn that ran it", none)
		if restart {
			h.restart()
			named(t, h, "a restart after that result", none)
		}
		sendsWhole(t, h, next, StreamTurnEnd{Claim: h.token}, RequestDeliver{Claim: h.token, Task: next})
		named(t, h, "the next task", none)
	}
	// errored is a claim whose process a read found running over the death that took its task
	// back, the task sent again and its turn ended in a provider error, the ladder holding it.
	errored := func(t *testing.T) *harness {
		t.Helper()
		h, _ := workingOverTheProcessItsRefusedRelaunchKept(t)
		readRunning(t, h)
		sendsWhole(t, h, sentAgainTask+task, StreamTurnEnd{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
		named(t, h, "the provider error's take-back", itsOwn)
		return h
	}
	for _, restart := range []bool{false, true} {
		name := "a provider error ends the turn of the task sent again"
		if restart {
			name += "; a restart between the next turn's result and its end"
		}
		t.Run(name, func(t *testing.T) {
			h := errored(t)
			const provider = "Your previous turn on this task stopped because of a provider error (HTTP 529). Your work did not cause it. " +
				"The daemon sends you the task again. Read what that turn already did in your workspace and on the issue's branch. " +
				"Then continue the task.\n\n"
			if got, want := h.climbs(len(h.prompts()), promptRetryFirst), sentAgainTask+provider+task; got != want {
				t.Fatalf("the ladder sent the running process\n%q\nwant\n%q", got, want)
			}
			ran(t, h, restart)
		})
	}
	t.Run("a newer task takes the place of the task sent again on the provider error's ladder", func(t *testing.T) {
		h := errored(t)
		sendsWhole(t, h, next, RequestDeliver{Claim: h.token, Task: next})
		named(t, h, "the newer task took the place of the one sent again", none)
	})
	t.Run("design news rewrites the task sent again before its turn", func(t *testing.T) {
		const narrowed = `{"task":"Review the change. Issue: LEGION-209. Phase: reviewing.",` +
			`"items":[{"issue":"LEGION-209","audience":"holder","step":"spec","artifact":"artifact-spec","from":3,"to":5}]}`
		const text = "Review the change. Issue: LEGION-209. Phase: reviewing.\n\nThe approved design of LEGION-208 changed for LEGION-209."
		answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
		h := pacedHarness(t, wakeRate, func(d *Deps) { d.DesignNews = answer.answer })
		h.reach(StateReady)
		h.must(newsStart("outbox:41", 41))
		h.must(StreamTurnStart{Claim: h.token})
		// The death takes the task back from its turn, and the lifecycle recheck refuses its
		// relaunch at its turn, so the claim stands over the process the read then finds running.
		store := &unansweredCheckLaunchStore{memStore: h.store}
		h.m.deps.Store = store
		h.observe(runtime.Gone)
		waiting := h.wantWaitingWake(WakeLaunch)
		store.failing = true
		h.advance(waiting.At.Sub(h.clock.Now()))
		store.failing = false
		readRunning(t, h)
		was := h.pending().ID
		named(t, h, "the read", itsOwn)
		// The record moves before the task's next send: the item that send renders changed.
		answer.task, answer.news = text, []byte(narrowed)
		sendsWhole(t, h, sentAgainTask+text, StreamTurnEnd{Claim: h.token})
		if h.pending().ID == was {
			t.Fatalf("the rewrite kept the id %s, want the new one the changed news gives the task", was)
		}
		named(t, h, "the rewrite", itsOwn)
		h.must(StreamTurnStart{Claim: h.token})
		ran(t, h, false)
	})
	t.Run("a second false death in the turn the process was in", func(t *testing.T) {
		h, ended := workingOverTheProcessItsRefusedRelaunchKept(t)
		readRunning(t, h)
		diesAgain(t, h, ended)
		readRunning(t, h)
		sendsWhole(t, h, sentAgainTask+task, StreamTurnEnd{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		ran(t, h, false)
	})
	t.Run("a second false death in no turn, which cuts off nothing", func(t *testing.T) {
		h, ended := idleAfterARunningRead(t)
		diesAgain(t, h, ended)
		if e := h.claim().EndedWork; !e.Died || !e.Tells() || e.Turn || e.TaskTurn {
			t.Fatalf("the second death left the record %+v, want the first death's record typed as this death's, cutting off nothing", e)
		}
		readRunning(t, h)
		// The agent answers the prompt of the task sent again busy, in a turn of its own, whose
		// end sends the task again under the new id the busy answer's take-back gave it.
		h.must(StreamPromptResult{Claim: h.token, DeliveryID: h.pending().ID, AgentInvoked: false, Error: busyAnswer})
		sendsWhole(t, h, sentAgainTask+task, StreamTurnEnd{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		ran(t, h, false)
	})
}
