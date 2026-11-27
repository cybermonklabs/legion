package supervise

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
// In every row the report clears the cause the death's relaunch left pending, in memory and in the
// store (dropOutlivedRelaunch): the relaunch it named never launched. Over an agent already up it
// sets the launch failures to 0, standing in for the ready of the relaunch it dropped; a process
// still coming up keeps its count, which its own ready then sets to 0
// (TestADroppedDeathRelaunchResetsTheLaunchFailuresAsItsReadyWould).
//
// The negative controls: judge's order as it was, reportedProcess run before the launchWaits
// guard and the wake left standing, fails every row, the claim still holding the relaunch; judge
// dropping only a relaunch the pacing holds fails the pause's row the same way; relaunchDeath
// marking no wake as the death's relaunch fails every row the same way; dropOutlivedRelaunch
// leaving the gate's stop posted fails the pause's row at that stop; dropOutlivedRelaunch leaving
// the cause pending fails every row at its check of it; dropOutlivedRelaunch without its reset
// fails every row of an agent already up at its count; and the reset made whatever the kept
// agent's state fails the row of a process still coming up at its count after the report.
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
	// It fails unless the claim held the death's one launch failure and died pending before it,
	// and unless the report dropped the relaunch and ended the death, with that cause cleared and
	// failures launch failures left, in the claim and in the store, its record told by no
	// delivery.
	report := func(t *testing.T, h *harness, failures int) {
		t.Helper()
		if c := h.claim(); c.Budgets.LaunchFailures != 1 || c.PendingCause != CauseDied {
			t.Fatalf("the death's relaunch left %d launch failures and %q pending, want its one charge and %q", c.Budgets.LaunchFailures,
				c.PendingCause, CauseDied)
		}
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
			if c.Budgets.LaunchFailures != failures || c.PendingCause != "" {
				t.Fatalf("%s holds %d launch failures and %q pending after the sweep found the process running, want %d and nothing pending",
					whose, c.Budgets.LaunchFailures, c.PendingCause, failures)
			}
		}
	}
	// running is report over a relaunch the pacing holds, and fails unless its place was given up.
	running := func(t *testing.T, h *harness, place *wakepace.Place, failures int) {
		t.Helper()
		report(t, h, failures)
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
			running(t, h, place, 0)
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
		report(t, h, 0)
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
	t.Run("a process still coming up, which keeps its count until its own ready", func(t *testing.T) {
		h := pacedHarness(t, wakeRate)
		h.reach(StateRegistered)
		queueAhead(h, 1)
		h.observe(runtime.Gone)
		place := parked(t, h)
		if h.armed(TimerRegistration) {
			t.Fatal("the park left the registration deadline of the process its relaunch replaces armed, want it disarmed")
		}
		running(t, h, place, 1)
		h.wantState(StateRegistered)
		if !h.armed(TimerRegistration) {
			t.Fatal("no registration deadline watches the process the dropped relaunch kept, want it armed again")
		}
		h.ready()
		for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
			if c.Budgets.LaunchFailures != 0 {
				t.Fatalf("%s holds %d launch failures after the kept agent's ready, want 0: its own ready resets them", whose, c.Budgets.LaunchFailures)
			}
		}
	})
}

// A report that finds the claim's process running drops only the relaunch the death it ends
// raised, which that raise marked on its wake (wake.deathRelaunch, relaunchDeath). A launch raised
// over the same death for a reason of its own — the stale-plugin refresh lane's relaunch
// (refreshPlugin) or a stale registration's (staleRegistration), over a death whose own relaunch
// the lifecycle recheck refused — is kept at its place, and its turn replaces the process onto
// this daemon's plugin: the report ends the death and nothing of what that launch was raised for,
// since a stale plugin's process stays stale.
//
// The negative control is judge's guard without the mark, reading the waiting launch and the
// charge alone: both rows fail, the claim holding no wake after the report.
func TestASweepReportThatFindsTheProcessRunningKeepsALaunchNoDeathRaised(t *testing.T) {
	// overARefusedRelaunch is a ready claim whose process the runtime reported gone: the death is
	// decided, its relaunch refused by the lifecycle recheck, so the claim stands over the process
	// with no wake, and the daemon speaks a plugin contract that process does not.
	overARefusedRelaunch := func(t *testing.T) *harness {
		t.Helper()
		h := pacedHarness(t, wakeRate)
		h.reach(StateReady)
		h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
		if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}}); err == nil {
			t.Fatal("the death's relaunch was taken, want the lifecycle recheck's refusal of it")
		}
		h.m.deps.Store = h.store
		if held := h.m.View().WakeWaiting; !h.claim().chargedDeath() || held != nil {
			t.Fatalf("the death left deathCharged=%v and wake %+v, want the death decided and no wake", h.claim().DeathCharged, held)
		}
		h.m.deps.PluginContract = refreshContract
		return h
	}
	// kept is the sweep's report of the process running over the launch of kind that waits: it
	// fails unless the report ends the death and keeps the launch, and that launch's turn replaces
	// the process onto this daemon's plugin.
	kept := func(t *testing.T, h *harness, kind WakeKind) {
		t.Helper()
		waiting := h.wantWaitingWake(kind)
		starts, stale := h.starts(h.token), h.locator()
		h.observe(runtime.Alive)
		if c := h.claim(); c.DeathCharged || c.judgedDeath() {
			t.Fatalf("the claim reads deathCharged=%v over the record %+v after the sweep found its process running, want the death over",
				c.DeathCharged, c.EndedWork)
		}
		if held := h.m.View().WakeWaiting; held == nil || held.Kind != kind {
			t.Fatalf("the claim holds wake %+v after the sweep found its process running, want the %s launch no death raised kept", held, kind)
		}
		h.advance(waiting.At.Sub(h.clock.Now()))
		h.wantStarts(h.token, starts+1)
		if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != stale {
			t.Fatalf("the kept launch's turn replaced %+v, want the process the sweep found running, %+v", resume.Previous, stale)
		}
		if got := h.claim().PluginRelaunchFor; got != refreshContract {
			t.Fatalf("the claim after the kept launch ran is marked for %d, want %d", got, refreshContract)
		}
	}
	t.Run("the stale-plugin refresh lane's relaunch", func(t *testing.T) {
		h := overARefusedRelaunch(t)
		h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
		kept(t, h, WakePluginRefresh)
	})
	t.Run("a stale registration's relaunch", func(t *testing.T) {
		h := overARefusedRelaunch(t)
		h.m.deps.PluginRelaunchable = true
		err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
			SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: staleContract})
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(staleContract, refreshContract) {
			t.Fatalf("the stale registration = %v, want the refusal naming %d and %d", err, staleContract, refreshContract)
		}
		kept(t, h, WakeLaunch)
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
		h.must(StreamPromptResult{Claim: h.token, DeliveryID: h.pending().ID, AgentInvoked: false, Error: busyWords, Outcome: runtime.OutcomeAgentBusy})
		sendsWhole(t, h, sentAgainTask+task, StreamTurnEnd{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		ran(t, h, false)
	})
}

// The drop of a death's waiting relaunch by a report that finds the process running clears only a
// cause that relaunch left pending (dropOutlivedRelaunch, Claim.PendingCause). A death of a ready
// claim holding nothing charged its relaunch's launch failure and recorded died pending, so the
// drop clears the cause, and sets the launch failures to 0 over the agent already up, as the
// dropped relaunch's ready would. The end of a start by Oh My Pi's own lookup that never
// registered is one the policy charges nothing (endOutcome, Charges): its relaunch recorded no
// cause, and the cause pending is the earlier death's that this start is the relaunch of, so the
// drop leaves it, and the start, still coming up, keeps that death's launch failure until its own
// ready.
//
// The negative controls: dropOutlivedRelaunch leaving the cause pending fails the first row (and
// every row of TestASweepReportThatFindsTheProcessRunningDropsTheWaitingRelaunchOfItsDeath), and
// dropOutlivedRelaunch clearing died pending whatever the policy says of the end fails the second;
// dropOutlivedRelaunch without its reset fails the first row at its count after the report, and
// the reset made whatever the kept agent's state fails the second.
func TestADroppedDeathRelaunchClearsOnlyTheCauseItsDeathLeftPending(t *testing.T) {
	// pending fails unless the claim and the store each count failures launch failures and hold
	// cause pending.
	pending := func(t *testing.T, h *harness, when string, failures int, cause FailureCause) {
		t.Helper()
		for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
			if c.Budgets.LaunchFailures != failures || c.PendingCause != cause {
				t.Fatalf("%s counts %d launch failures with %q pending %s, want %d and %q", whose, c.Budgets.LaunchFailures,
					c.PendingCause, when, failures, cause)
			}
		}
	}
	// running is the sweep's report of the process running while the death's relaunch waits,
	// which ends the death and drops the relaunch.
	running := func(t *testing.T, h *harness) {
		t.Helper()
		h.wantWaitingWake(WakeLaunch)
		h.observe(runtime.Alive)
		if held, c := h.m.View().WakeWaiting, h.claim(); held != nil || c.DeathCharged {
			t.Fatalf("the claim holds wake %+v and deathCharged=%v after the sweep found its process running, want the death over and its relaunch dropped",
				held, c.DeathCharged)
		}
	}
	t.Run("a death of a ready claim holding nothing", func(t *testing.T) {
		h := pacedHarness(t, wakeRate)
		h.reach(StateReady)
		queueAhead(h, 1)
		h.observe(runtime.Gone)
		if c := h.claim(); c.Budgets.LaunchFailures != 1 || c.PendingCause != CauseDied {
			t.Fatalf("the death's relaunch left %d launch failures and %q pending, want its one charge and %q", c.Budgets.LaunchFailures,
				c.PendingCause, CauseDied)
		}
		running(t, h)
		pending(t, h, "after the report", 0, "")
		h.wantState(StateReady)
	})
	t.Run("the end of a start by Oh My Pi's own lookup that never registered, which is charged nothing", func(t *testing.T) {
		c := byIDQueued(1)
		c.PendingCause = CauseDied
		r := newByIDRig(t, c, func(h *harness) {
			olderLauncher(h)
			h.pace = pacedDeps(wakeRate)
			h.pace(&h.deps)
		})
		r.startByAgentLookup()
		queueAhead(r.harness, 1)
		r.observe(runtime.Gone)
		if ends := r.records.prefixed(unchargedEndLine); len(ends) != 1 {
			t.Fatalf("the end was logged %d times by %q, want once: an end charged nothing", len(ends), unchargedEndLine)
		}
		pending(t, r.harness, "after the end", 1, CauseDied)
		running(t, r.harness)
		pending(t, r.harness, "after the report", 1, CauseDied)
	})
}

// A report that finds an agent already up running while the relaunch of the death it ends waits
// drops that relaunch and sets the launch failures to 0, standing in for the ready of the launch
// it dropped (dropOutlivedRelaunch,
// dispatch://INFRA-1117/comment/ef5d798e-5e76-4e5a-92ef-616545f4567c): had that relaunch run, its
// agent's ready would have set them to 0, where the agent kept in its place, already up, says no
// ready that resets anything (reready). So three false deaths of an idle claim that holds
// nothing, each overruled by the sweep while its relaunch waits behind another claim's wake, leave
// the claim idle over its one process with no launch failure, no death and no cause pending, in
// the claim and in the store, where each death would otherwise leave one more launch failure
// counted and the third would fail the claim at launch_failure_limit. The reset reads no raise, so
// a restart reaches it whether the boot's replay charges the death's launch failure or finds it
// counted where a write while the relaunch waited stored it beside died pending
// (deathLaunchFailureCharged); a ready from the process while the relaunch waits has reset the
// count already; and a launch failure left over from before the false death, which a running read
// kept (TestARunningVerdictBeforeTheBootsReplayGivesNothingBack), goes with the death's
// own, as the ready of the dropped relaunch would have taken both.
//
// The roll edge is the reviewer's trace at 6b60c510
// (dispatch://INFRA-1117/artifact/roll-edge-trace-md): a claim recording a session and no file
// holds n launch failures with died pending, as the earlier release stored them, and the fence
// refuses the registration of the process that release started, which marks its end charged
// nothing (markRollEdge). Its false death behind another claim's wave decides the death and parks
// an uncharged relaunch (relaunchAfterFailure); a restart loses the mark; the boot's replay reads
// the end as a charged death, finds its charge in died pending and charges nothing; the process
// registers again, refused, which marks its end charged nothing again; and the sweep finds it
// running. That process has sent no ready, and can send none: a ready needs a registration the
// fence took (StateRegistered), and that registration records the session's file (register), after
// which the end of the process is read as a death the policy charges (processOutcome), not the
// uncharged end the trace's death is. So the trace reaches only a kept agent still coming up, and
// the drop leaves the count at n, never below 0 in any write the store takes, and a second cycle
// of the same steps leaves it there; a kept agent that sent a ready is the other rows'.
//
// The negative controls: dropOutlivedRelaunch without its reset fails every row but the ready's
// and the roll edge's, at its count after the first report; the one-decrement give-back in its
// place, read off died pending, fails the leftover row at its count, 1 where 0, and the roll
// edge's at its first cycle, 0 where n; and the reset made whatever the kept agent's state fails
// the roll edge's at its first count after the report.
func TestADroppedDeathRelaunchResetsTheLaunchFailuresAsItsReadyWould(t *testing.T) {
	// counts fails unless the claim, and the store where stored is, count failures launch failures
	// and no death, with cause pending.
	counts := func(t *testing.T, h *harness, when string, stored bool, failures int, cause FailureCause) {
		t.Helper()
		of := map[string]Claim{"the claim": h.claim()}
		if stored {
			of["the store"] = h.store.load(h.token)
		}
		for whose, c := range of {
			if c.Budgets != (Budgets{LaunchFailures: failures}) || c.PendingCause != cause {
				t.Fatalf("%s counts %+v with %q pending %s, want %d launch failures, no death and %q pending", whose, c.Budgets,
					c.PendingCause, when, failures, cause)
			}
		}
	}
	// marked fails unless the claim holds the death's relaunch waiting in the pacing, marked as
	// the death's.
	marked := func(t *testing.T, h *harness) {
		t.Helper()
		h.wantWaitingWake(WakeLaunch)
		h.m.mu.Lock()
		death := h.m.wake.deathRelaunch
		h.m.mu.Unlock()
		if !death {
			t.Fatal("the death's waiting relaunch is not marked as the death's, want it marked")
		}
	}
	// overruled is the sweep's report of the process running, which fails unless it drops the
	// relaunch and leaves the claim over the process it recorded, the death over, nothing started.
	overruled := func(t *testing.T, h *harness) {
		t.Helper()
		alive, starts := h.locator(), h.starts(h.token)
		h.observe(runtime.Alive)
		if held := h.m.View().WakeWaiting; held != nil {
			t.Fatalf("the claim holds wake %+v after the sweep found its process running, want the death's relaunch dropped", held)
		}
		if c := h.claim(); c.Locator == nil || *c.Locator != alive || c.DeathCharged {
			t.Fatalf("the claim records %+v with deathCharged=%v after the report, want %+v kept and the death over", c.Locator, c.DeathCharged, alive)
		}
		h.wantStarts(h.token, starts)
	}
	for _, restart := range []bool{false, true} {
		name := "three false deaths of an idle claim that holds nothing"
		if restart {
			name += ", each with a restart before its report, whose boot's replay charges the relaunch again"
		}
		t.Run(name, func(t *testing.T) {
			h := pacedHarness(t, wakeRate)
			h.reach(StateIdle)
			for death := 1; death <= 3; death++ {
				queueAhead(h, 1)
				h.observe(runtime.Gone)
				counts(t, h, fmt.Sprintf("after death %d", death), false, 1, CauseDied)
				if restart {
					restartBehindAWave(t, h)
					h.must(RecordedDeath{Claim: h.token, Generation: h.claim().Generation})
					counts(t, h, fmt.Sprintf("after the boot's replay of death %d", death), false, 1, CauseDied)
				}
				marked(t, h)
				overruled(t, h)
				h.wantState(StateIdle)
				counts(t, h, fmt.Sprintf("after the report of death %d", death), true, 0, "")
				h.advance(time.Minute)
			}
			h.wantStarts(h.token, 1)
		})
	}
	t.Run("a false death whose launch failure a task's write stored while its relaunch waited, a restart, and the boot's replay, which charges nothing",
		func(t *testing.T) {
			h := pacedHarness(t, wakeRate)
			h.reach(StateIdle)
			queueAhead(h, 1)
			h.observe(runtime.Gone)
			h.must(RequestDeliver{Claim: h.token, Task: "the phase's task", ID: "outbox:7", Row: 7})
			counts(t, h, "after the task's write", true, 1, CauseDied)
			restartBehindAWave(t, h)
			h.must(RecordedDeath{Claim: h.token, Generation: h.claim().Generation})
			counts(t, h, "after the boot's replay", false, 1, CauseDied)
			marked(t, h)
			overruled(t, h)
			counts(t, h, "after the report", true, 0, "")
		})
	t.Run("a ready from the process, still coming up, while its relaunch waits", func(t *testing.T) {
		h := pacedHarness(t, wakeRate)
		h.reach(StateRegistered)
		queueAhead(h, 1)
		h.observe(runtime.Gone)
		counts(t, h, "after the death", false, 1, CauseDied)
		h.ready()
		counts(t, h, "after the ready", true, 0, "")
		marked(t, h)
		overruled(t, h)
		h.wantState(StateReady)
		counts(t, h, "after the report", true, 0, "")
	})
	t.Run("a launch failure left over from an earlier death a running read ended, then a false death", func(t *testing.T) {
		h := pacedHarness(t, wakeRate)
		h.reach(StateIdle)
		// The earlier death's relaunch is refused at its turn by the lifecycle recheck, so its
		// charge stays with no wake to drop, and a read finds the process running.
		queueAhead(h, 1)
		store := &unansweredCheckLaunchStore{memStore: h.store}
		h.m.deps.Store = store
		h.observe(runtime.Gone)
		waiting := h.wantWaitingWake(WakeLaunch)
		store.failing = true
		h.advance(waiting.At.Sub(h.clock.Now()))
		h.m.deps.Store = h.store
		if held := h.m.View().WakeWaiting; held != nil || !h.claim().DeathCharged {
			t.Fatalf("the refused turn left wake %+v and deathCharged=%v, want no wake over the death decided", held, h.claim().DeathCharged)
		}
		readRunning(t, h)
		counts(t, h, "after the running read", true, 1, CauseDied)
		queueAhead(h, 1)
		h.observe(runtime.Gone)
		counts(t, h, "after the false death", false, 2, CauseDied)
		marked(t, h)
		overruled(t, h)
		h.wantState(StateIdle)
		counts(t, h, "after the report", true, 0, "")
	})
	t.Run("the roll edge: an earlier release's process the fence refuses, its uncharged false death, a restart and the boot's replay, twice",
		func(t *testing.T) {
			const n = 1
			c := byIDClaim(StateLaunching, 3)
			c.Budgets.LaunchFailures, c.PendingCause = n, CauseDied
			r := newByIDRig(t, c, func(h *harness) {
				h.pace = pacedDeps(wakeRate)
				h.pace(&h.deps)
			})
			var negative []Budgets
			r.store.onPut = func(c Claim) {
				if c.Budgets.LaunchFailures < 0 {
					negative = append(negative, c.Budgets)
				}
			}
			// refused is the registration of the earlier release's process on its older contract,
			// which the fence refuses and which marks the end of that process charged nothing.
			refused := func(t *testing.T) {
				t.Helper()
				if err := r.registerAs(otherSession, otherSessionFile, r.olderContract()); !errors.Is(err, claim.SameAgentRefusal) {
					t.Fatalf("the registration on contract %d = %v, want the same-agent refusal", r.olderContract(), err)
				}
			}
			r.connect()
			refused(t)
			for cycle := 1; cycle <= 2; cycle++ {
				queueAhead(r.harness, 1)
				r.observe(runtime.Gone)
				if s := r.store.load(r.token); !s.DeathCharged || s.Budgets != (Budgets{LaunchFailures: n}) || s.PendingCause != CauseDied {
					t.Fatalf("cycle %d: the store holds %+v with %q pending and deathCharged=%v after the uncharged death, want the death decided over %d launch failures and died pending",
						cycle, s.Budgets, s.PendingCause, s.DeathCharged, n)
				}
				restartBehindAWave(t, r.harness)
				r.must(RecordedDeath{Claim: r.token, Generation: r.claim().Generation})
				counts(t, r.harness, fmt.Sprintf("after the boot's replay of cycle %d", cycle), false, n, CauseDied)
				marked(t, r.harness)
				refused(t)
				overruled(t, r.harness)
				counts(t, r.harness, fmt.Sprintf("after the report of cycle %d", cycle), true, n, CauseDied)
			}
			if len(negative) > 0 {
				t.Fatalf("the claim was written counting %+v, want no write below 0: the store's check refuses it", negative)
			}
		})
}

// A running verdict, a read at a caller's request (ReadProcess) or the sweep's report, that comes
// after a restart and before the boot's replay of a decided death gives nothing back
// (dispatch://INFRA-1117/comment/3859c597-2267-417b-a57d-be9738fdfebc).
// The verdict ends the death (reportedProcess), so the replay finds no death decided and raises no
// relaunch (relaunchRecordedDeath); no wake holds the relaunch the restart lost, so no drop stands
// in for its ready (dropOutlivedRelaunch). The launch failure a task's write stored while that
// relaunch waited stays counted, with died pending, in the claim and in the store, until the next
// ready clears both, and nothing starts.
//
// It pins behaviour that dispatch://INFRA-1117 left as it found it, so it has no negative control
// of that change. A regression would have to change relaunchRecordedDeath's first case, which
// returns over a claim whose running verdict cleared Claim.DeathCharged, or write the count where
// no wake is dropped: a budget write in reportedProcess, ReadProcess or relaunchRecordedDeath,
// which TestOutcomeSourceHoldsThePolicyAtItsSites refuses unless budgetResets lists it.
func TestARunningVerdictBeforeTheBootsReplayGivesNothingBack(t *testing.T) {
	// counts fails unless the claim and the store each count one launch failure and no death,
	// with died pending and no death decided.
	counts := func(t *testing.T, h *harness, when string) {
		t.Helper()
		for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
			if c.Budgets != (Budgets{LaunchFailures: 1}) || c.PendingCause != CauseDied || c.DeathCharged {
				t.Fatalf("%s counts %+v with %q pending and deathCharged=%v %s, want main's one launch failure, no death, %q pending and the death over",
					whose, c.Budgets, c.PendingCause, c.DeathCharged, when, CauseDied)
			}
		}
	}
	for _, verdict := range []struct {
		name string
		run  func(t *testing.T, h *harness)
	}{
		{"a read at a caller's request", readRunning},
		{"the sweep's report", func(_ *testing.T, h *harness) { h.observe(runtime.Alive) }},
	} {
		t.Run(verdict.name+" finding the process running before the boot's replay", func(t *testing.T) {
			h := pacedHarness(t, wakeRate)
			h.reach(StateIdle)
			queueAhead(h, 1)
			h.observe(runtime.Gone)
			h.must(RequestDeliver{Claim: h.token, Task: "the phase's task", ID: "outbox:7", Row: 7})
			if s := h.store.load(h.token); s.Budgets != (Budgets{LaunchFailures: 1}) || s.PendingCause != CauseDied || !s.DeathCharged {
				t.Fatalf("the store holds %+v with %q pending and deathCharged=%v before the restart, want the death decided and its relaunch's launch failure stored",
					s.Budgets, s.PendingCause, s.DeathCharged)
			}
			restartBehindAWave(t, h)
			verdict.run(t, h)
			counts(t, h, "after the running verdict")
			starts := h.starts(h.token)
			h.must(RecordedDeath{Claim: h.token, Generation: h.claim().Generation})
			if held := h.m.View().WakeWaiting; held != nil {
				t.Fatalf("the boot's replay left wake %+v, want none: the running verdict ended the death", held)
			}
			h.wantStarts(h.token, starts)
			counts(t, h, "after the boot's replay")
		})
	}
}

// The boot's replay of a recorded end from outside the pod raises that end's launch as the end
// itself raised it, through launch itself (relaunchRecordedDeath, launchAfter), and never as a
// death's relaunch (relaunchDeath): the wake it parks behind the restart's wave is not marked as
// one (wake.deathRelaunch), since no death raised it. A sweep's report of the process running
// leaves that launch at its place, and its turn replaces the process.
//
// The negative control is that branch raised through relaunchDeath again: the wake is marked as a
// death's relaunch, and the row fails at the mark. The report keeps the launch under the control
// too, since judge reads no charged death over an end decided from outside the pod (chargedDeath).
func TestTheBootsReplayOfAnEndFromOutsideThePodMarksNoDeathsRelaunch(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	queueAhead(h, 1)
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
		EndReading: runtime.PodEndNotOurs}})
	if stored := h.store.load(h.token); !stored.DeathCharged || !stored.decidedFromOutside() {
		t.Fatalf("the store holds deathCharged=%v over the record %+v, want the end decided from outside the pod", stored.DeathCharged,
			stored.EndedWork)
	}
	restartBehindAWave(t, h)
	h.must(RecordedDeath{Claim: h.token, Generation: h.claim().Generation})
	h.wantWaitingWake(WakeLaunch)
	h.m.mu.Lock()
	marked := h.m.wake.deathRelaunch
	h.m.mu.Unlock()
	if marked {
		t.Fatal("the boot's replay of the end from outside the pod parked a wake marked as a death's relaunch, want no mark: no death raised it")
	}
	starts, ended := h.starts(h.token), h.locator()
	h.observe(runtime.Alive)
	if held := h.m.View().WakeWaiting; held == nil || held.Kind != WakeLaunch {
		t.Fatalf("the claim holds wake %+v after the sweep found its process running, want the replay's launch kept", held)
	}
	h.advanceToTurn(h.m)
	h.wantStarts(h.token, starts+1)
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != ended {
		t.Fatalf("the replay's launch replaced %+v, want the process of the end from outside the pod, %+v", resume.Previous, ended)
	}
}

// A death's relaunch that runs at once and then waits at its issue pod's gate behind another
// role's load (askPod) is marked as the death's relaunch (wake.deathRelaunch): the park is a wake
// the raise put on the machine (relaunchDeath), and it is that relaunch waiting, for its pod's
// start rather than its turn. No report drops it. The launch let the dead process go before it
// parked, and the death's charge with it (launchNow, letGo), so the sweep's report that finds that
// process running is dropped as stale (fence) and decides nothing: the start stays parked, and the
// grant starts it.
//
// The negative control is relaunchDeath marking no wake: the row fails at the mark.
func TestADeathsRelaunchParkedAtItsPodsGateIsMarkedAndNoReportDropsIt(t *testing.T) {
	h := podHarness(t, "")
	first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)
	h.runs(second)
	dead := *second.m.View().Claim.Locator
	h.mustPostTo(first.m, RequestResume{Claim: first.token, By: SuspendedByOperator})
	h.wantStarts(first.token, 1)
	h.parks(second, RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now()}}, 1)
	// parked is whether the second's machine holds a start parked at the gate, and whether that
	// wake is marked as the death's relaunch.
	parked := func() (held, marked bool) {
		second.m.mu.Lock()
		defer second.m.mu.Unlock()
		w := second.m.wake
		return w != nil && w.park != nil, w != nil && w.deathRelaunch
	}
	if held, marked := parked(); !held || !marked {
		t.Fatalf("the death's relaunch is parked at the gate %v and marked as the death's relaunch %v, want both", held, marked)
	}
	if c := second.m.View().Claim; c.Locator != nil || c.DeathCharged {
		t.Fatalf("the claim records %+v with deathCharged=%v while its relaunch waits at the gate, want the dead process let go with the death's charge",
			c.Locator, c.DeathCharged)
	}
	const staleLine = "supervise: dropped a stale event"
	stale := len(h.podRecords(staleLine, second.token))
	if err := h.postPromptly(second, RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Alive, At: h.clock.Now()}}); err != nil {
		t.Fatalf("the sweep's report of the process the relaunch let go answered %v, want no failure", err)
	}
	if got := len(h.podRecords(staleLine, second.token)); got != stale+1 {
		t.Fatalf("the second's machine dropped %d stale events after the sweep's report, want %d: the report is of a process the launch let go",
			got, stale+1)
	}
	if held, _ := parked(); !held {
		t.Fatal("the start no longer waits at the gate after the sweep's report, want it kept: no report drops a launch that let its process go")
	}
	h.loadEnds(first)
	h.wantGrantedStart(second, 2)
}
