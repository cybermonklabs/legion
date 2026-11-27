package supervise

// What becomes of a death died judged when a later read or report of the same process meets it:
// a read that finds the process running ends it, and a later report over a death died decided
// raises the relaunch the decision raised and leaves the decision's record as it stands.

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// judgedOverARefusedTakeBack is a claim in a turn of its confirmed task whose process the
// runtime reported gone, read neither way, with the store refusing the take-back: died recorded
// the death it judged (EndedWork.Died) and returned before deciding it, so the claim stands
// working over the dead process, the task still confirmed, charged nothing. Another claim's wake
// waits ahead of any relaunch, so a relaunch raised later parks where its queue says.
func judgedOverARefusedTakeBack(t *testing.T) (*harness, runtime.Locator) {
	t.Helper()
	h := pacedHarness(t, wakeRate)
	h.reach(StateWorking)
	queueAhead(h, 1)
	ended := h.locator()
	h.store.fail("PutDelivery", errStoreRefused)
	if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}}); err == nil {
		t.Fatal("the take-back was written, want the store's refusal of it")
	}
	h.store.fail("PutDelivery", nil)
	if c := h.claim(); !c.judgedDeath() || c.DeathCharged {
		t.Fatalf("the claim reads deathCharged=%v over the record %+v, want a death judged and not decided", c.DeathCharged, c.EndedWork)
	}
	return h, ended
}

// readRunning reads the claim's process at a caller's request and finds it running.
func readRunning(t *testing.T, h *harness) {
	t.Helper()
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Alive})
	if got := h.m.ReadProcess(h.ctx); got != ProcessRuns {
		t.Fatalf("the read answered %q, want %q", got, ProcessRuns)
	}
}

// restartBehindAWave restarts the daemon, and another role's place in the first queue stands
// for the restart's wave, as in the walk of every order, so a relaunch raised after it waits its
// turn in its queue.
func restartBehindAWave(t *testing.T, h *harness) {
	t.Helper()
	h.restart()
	firstAhead(h.deps.WakePace.Pacer, 1)
}

// A read that finds the claim's process running ends a death died judged of it: the process
// lives, so the record's judgement of that death (EndedWork.Died) goes with the charge
// (Claim.DeathCharged), in the claim and in the store, the one write carrying both
// (reportedProcess; ReadProcess, judge). A later end of the process read as from outside the pod
// is then judged afresh: continued charged nothing beyond what the death charged, its record of
// the turn it cut off made afresh as one from outside the pod under that end's cause (recordEnd,
// EndedWork.Outlived), its relaunch in the second queue, the next process told that its pod was
// ended from outside it. That holds after a restart between the read and the end, as the read's
// write carried the clear; for the sweep's report as for a read; and for a death died judged over
// a take-back the store refused as for one it decided. Left standing, the judgement sends that end
// to died (judgedDeath), which charges it as a second death of a process that ran on. A death
// died judges after the read is a death of its own, recorded afresh, so an end read as from
// outside the pod after it goes to died as that death's later report, and is charged as that one
// death.
//
// The negative control is reportedProcess leaving the record's judgement standing (Died): every
// row fails, the claim and the store still holding the death after the read. How a later end is
// recorded after the read is held by TestAnEndAfterARunningReadRecordsTheTurnItCutsOff, where it
// cuts off a turn, and TestAnEndThatCutsOffNothingAfterARunningReadTypesTheRecordUnderItsOwnCause,
// where it cuts off none.
func TestAReadThatFindsTheProcessRunningEndsItsDeathSoALaterEndFromOutsideIsJudgedAfresh(t *testing.T) {
	for _, row := range []struct {
		name  string
		stand func(t *testing.T) (*harness, runtime.Locator)
		// read is the verdict that finds the process running, and between what comes after it
		// and before the end read as outside.
		read, between func(t *testing.T, h *harness)
		// death is whether the end read as outside is the later report of a death died judged
		// after the read, charged as that death; every other row's is continued uncharged.
		death bool
	}{
		{name: "a death decided, its relaunch refused at its turn; a read finds the process running",
			stand: workingOverTheProcessItsRefusedRelaunchKept, read: readRunning},
		{name: "a death decided, its relaunch refused at its turn; a read finds the process running; a restart",
			stand: workingOverTheProcessItsRefusedRelaunchKept, read: readRunning,
			between: restartBehindAWave},
		{name: "a death decided, its relaunch refused at its turn; the sweep finds the process running; a restart",
			stand: workingOverTheProcessItsRefusedRelaunchKept, read: func(t *testing.T, h *harness) { h.observe(runtime.Alive) },
			between: restartBehindAWave},
		{name: "a death judged, its take-back refused; a read finds the process running",
			stand: judgedOverARefusedTakeBack, read: readRunning},
		{name: "a death judged, its take-back refused; a read finds the process running; a report read neither way, its take-back refused",
			stand: judgedOverARefusedTakeBack, read: readRunning, death: true,
			between: func(t *testing.T, h *harness) {
				h.store.fail("PutDelivery", errStoreRefused)
				if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.NotRecordedProcess, At: h.clock.Now()}}); err == nil {
					t.Fatal("the take-back was written, want the store's refusal of it")
				}
				h.store.fail("PutDelivery", nil)
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, ended := row.stand(t)
			charged := h.budgets()
			row.read(t, h)
			if c, stored := h.claim(), h.store.load(h.token); c.DeathCharged || c.judgedDeath() || stored.DeathCharged || stored.judgedDeath() {
				t.Fatalf("after the running verdict the claim reads deathCharged=%v over %+v, and the store deathCharged=%v over %+v; want the death over in both",
					c.DeathCharged, c.EndedWork, stored.DeathCharged, stored.EndedWork)
			}
			if row.between != nil {
				row.between(t, h)
			}

			h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}})

			c := h.claim()
			if c.ProcessGone && h.armed(TimerTurn) {
				t.Errorf("TimerTurn is armed over the process %s reported gone, want it disarmed", ended.Incarnation)
			}
			waiting, queue, told := h.wantWaitingWake(WakeLaunch), wakepace.Second, endFromOutside+"."
			if row.death {
				queue, told = wakepace.First, "the process died"
				if want := (Budgets{LaunchFailures: 1, Deaths: 1}); c.Budgets != want {
					t.Fatalf("budgets %+v, want %+v: the one death died judged after the read", c.Budgets, want)
				}
				if !c.judgedDeath() || c.recordedFromOutside() || !strings.HasPrefix(c.EndedWork.Cause, told) {
					t.Fatalf("the claim reads deathCharged=%v over the record %+v, want the death died judged after the read", c.DeathCharged, c.EndedWork)
				}
			} else {
				if c.Budgets != charged {
					t.Fatalf("budgets %+v, want %+v: an end from outside the pod charges nothing", c.Budgets, charged)
				}
				if e := c.EndedWork; !c.decidedFromOutside() || e.Died || e.Cause != endFromOutside || e.Death != "" {
					t.Fatalf("the claim reads deathCharged=%v over the record %+v, want the end decided from outside the pod under %q", c.DeathCharged, e, endFromOutside)
				}
			}
			if place := h.m.view.Load().place; place == nil || place.Queue() != queue {
				t.Fatalf("the relaunch %+v waits at %+v, want a place in queue %v", waiting, place, queue)
			}

			h.advanceToTurn(h.m)
			sent := len(h.prompts())
			h.relaunched()
			got := h.wantPrompts(sent + 1)[sent].Message
			if !strings.Contains(got, "ended because "+told) || !row.death && strings.Contains(got, "the process died") {
				t.Fatalf("the relaunched role was sent %q, want it told %q", got, told)
			}
		})
	}
}

// A death died decided first stays that death whatever a later report of the same process reads
// of the activity the claim's agent reported. A claim idle with nothing running, as its activity
// report says, has its death decided with no record: it held nothing (recordEnd). After a
// restart, a report read as outside goes to died as a later report of that death (judgedDeath),
// and nothing records the end over an end already decided, neither died nor the let-go of the
// relaunch it raises, whether that relaunch parks behind the restart's wave or runs in the
// report's own decision: the decision's finding that the end ended nothing worth recording
// stands, though the store now refuses the read of that activity, in the second row through the
// relaunch's let-go and its ready. Recorded again, the end would read as a process that ran a
// turn and left no report of it, a record that tells, and the relaunched role, which held
// nothing, would be woken to be told its process died (queueRelaunch). So the claim holds no
// record of the ended launch, its death still died's, its relaunch in the first queue
// (deathRetyped), and it is charged that death's launch failure alone, which died's decision
// stored uncharged and the restart's first raise charges, with no death for a claim that held
// nothing (chargeDeath); the relaunched role is sent nothing.
//
// The negative control is recordEnd recording over an end the claim holds decided: died's own
// report after the restart then writes a record of the ended launch that tells, in both rows.
func TestALaterReadOfADeathDecidedFirstWritesNoRecordOfAnEndFromOutsideThePod(t *testing.T) {
	for _, row := range []struct {
		name string
		// atOnce is whether the pacer has credit after the restart, so the relaunch runs in the
		// report's decision, with the activity read still refused, rather than parking.
		atOnce bool
	}{
		{name: "the relaunch parked behind the restart's wave"},
		{name: "the relaunch at once, the activity read refused through its ready", atOnce: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := pacedHarness(t, wakeRate)
			h.reach(StateIdle)
			h.reportActivity()
			queueAhead(h, 1)
			ended, read := h.locator(), h.generation()
			h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}})
			if c := h.claim(); !c.chargedDeath() || c.EndedWork.Generation == read {
				t.Fatalf("died left deathCharged=%v and the record %+v, want the death decided and nothing recorded of a launch that ended no work",
					c.DeathCharged, c.EndedWork)
			}

			h.restart()
			if !row.atOnce {
				firstAhead(h.deps.WakePace.Pacer, 1)
			}
			h.store.fail("ClaimActivity", errStoreRefused)
			sent := len(h.prompts())
			h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}})
			if row.atOnce {
				h.wantState(StateLaunching)
			} else {
				h.wantWaitingWake(WakeLaunch)
			}
			if wrong := deathRetyped(h, read); wrong != "" {
				t.Fatal(wrong)
			}
			for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
				if got := c.EndedWork; got.Generation == read || got.Tells() || c.Pending != nil {
					t.Fatalf("%s holds the record %+v and the delivery %+v, want no record of the ended launch and nothing to tell", whose, got, c.Pending)
				}
			}
			h.wantBudgets(Budgets{LaunchFailures: 1})

			if !row.atOnce {
				h.store.fail("ClaimActivity", nil)
				h.advanceToTurn(h.m)
			}
			h.relaunched()
			h.wantPrompts(sent)
			h.wantBudgets(Budgets{})
		})
	}
}

// A death died judged and then ended by a read that found the process running leaves its record
// of the launch outlived (EndedWork.Outlived), and the next end of the same launch, which here
// cuts off the turn of the task the death's take-back left confirmed, is recorded afresh under
// its own cause, type and kind, joined with that record while it still tells (recordEnd). A
// displacement's record is displaced's (EndedWork.Displaced): the role is told that its pod was
// replaced, and, the displacement's take-back refused and the claim written, a restart and a
// report read neither way find the displacement standing (recordedDisplaced), which charges
// nothing and relaunches the claim in the second queue. A later death's is died's, under that
// death's cause and kind, so the role is told the plain words of what killed its process
// (dispatch://INFRA-1117 R9) rather than the words of the death that was over.
//
// The negative controls: a record joined over the outlived one keeping that one's cause and kind
// (joined) fails both rows, the store holding the displacement under the death's cause and the
// later death's record under the first death's; and judge sending a report read neither way
// over displaced's record to died fails the first, the report after the restart charged a death
// and a launch failure.
func TestAJudgementAfterARunningReadTypesTheRecordUnderItsOwnCause(t *testing.T) {
	oom := runtime.Observation{Kind: runtime.Gone, Detail: "the pod's roles container was OOMKilled",
		Cause: runtime.DeathCause{Kind: runtime.DeathMemoryLimit, Limit: 8 << 30}}
	t.Run("a displacement, its take-back refused, a restart, then a report read neither way", func(t *testing.T) {
		h, ended := judgedOverARefusedTakeBack(t)
		readRunning(t, h)
		h.store.fail("PutDelivery", errStoreRefused)
		if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), Displaced: true}}); err == nil {
			t.Fatal("the take-back was written, want the store's refusal of it")
		}
		h.store.fail("PutDelivery", nil)
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Uncertain, At: h.clock.Now()}})
		if got := h.store.load(h.token).EndedWork; !got.Displaced || got.Died || got.Cause != endDisplaced || got.Death != "" {
			t.Fatalf("the store holds the record %+v, want displaced's under %q", got, endDisplaced)
		}

		restartBehindAWave(t, h)
		dying := len(h.logs.lines(diedLine, "claim="+string(h.token)))
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}})
		h.wantBudgets(Budgets{})
		if lines := len(h.logs.lines(diedLine, "claim="+string(h.token))); lines != dying {
			t.Fatalf("the report read neither way reached died, %d lines after %d, want it taken as the displacement", lines, dying)
		}
		h.wantWaitingWake(WakeLaunch)
		if place := h.m.view.Load().place; place == nil || place.Queue() != wakepace.Second {
			t.Fatalf("the relaunch waits at %+v, want a place in the second queue", place)
		}
		h.advanceToTurn(h.m)
		sent := len(h.prompts())
		h.relaunched()
		h.wantBudgets(Budgets{})
		got := h.wantPrompts(sent + 1)[sent].Message
		if !strings.Contains(got, "ended because "+endDisplaced+".") || strings.Contains(got, "the process died") {
			t.Fatalf("the relaunched role was sent %q, want it told the replacement", got)
		}
	})
	t.Run("a later death of the process with a plain cause", func(t *testing.T) {
		h, ended := judgedOverARefusedTakeBack(t)
		readRunning(t, h)
		death := oom
		death.Locator, death.At = ended, h.clock.Now()
		h.must(RuntimeObservation{Observation: death})
		if got := h.claim().EndedWork; !got.Died || got.Cause != endDied(death) || got.Death != runtime.DeathMemoryLimit {
			t.Fatalf("the claim records %+v, want the later death's record under %q", got, endDied(death))
		}
		h.advanceToTurn(h.m)
		sent := len(h.prompts())
		h.relaunched()
		if got := h.wantPrompts(sent + 1)[sent].Message; !strings.Contains(got, "8 GiB memory limit") {
			t.Fatalf("the relaunched role was sent %q, want the plain words of the death that killed its process", got)
		}
	})
}

// A death the runtime read a plain cause of, of a claim holding a task, leaves a record that
// tells in the death's own words (endDied, dispatch://INFRA-1117 R9). The relaunched process,
// still starting, is then displaced or its pod ended from outside it, which cuts off nothing, so
// that end leaves its typed record of its launch over the earlier one still untold
// (leaveRecord): the types route the reports of the new launch, and the record keeps the death's
// cause and kind, so the next relaunch tells the role what killed its process in the death's
// plain sentence (plainCause: the 8 GiB memory limit), rather than an end that ended nothing; the
// task is sent again not interrupted, so no out-of-memory note goes in front of it.
//
// The negative control is leaveRecord giving such a record the later end's cause: the role is
// then told that its pod was replaced or ended from outside it, and never what killed it.
func TestAnEndThatCutOffNothingKeepsTheWordsOfAnEarlierDeathStillUntold(t *testing.T) {
	for _, tc := range []struct {
		name        string
		end         runtime.Observation
		displaced   bool
		fromOutside bool
	}{
		{name: "a displacement", end: runtime.Observation{Kind: runtime.Gone, Displaced: true}, displaced: true},
		{name: "an end from outside the pod", end: runtime.Observation{Kind: runtime.Gone, EndReading: runtime.PodEndNotOurs}, fromOutside: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: owedTask})
			death := runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
				Detail: "the pod's roles container was OOMKilled", Cause: runtime.DeathCause{Kind: runtime.DeathMemoryLimit, Limit: 8 << 30}}
			h.must(RuntimeObservation{Observation: death})
			h.wantState(StateLaunching)
			if got := h.claim().EndedWork; !got.Tells() || got.Cause != endDied(death) {
				t.Fatalf("the death left the record %+v, want one that tells under %q", got, endDied(death))
			}

			end, launched := tc.end, h.generation()
			end.Locator, end.At = h.locator(), h.clock.Now()
			h.must(RuntimeObservation{Observation: end})
			got := h.claim().EndedWork
			if got.Generation != launched || got.Displaced != tc.displaced || got.FromOutside != tc.fromOutside ||
				got.Cause != endDied(death) || got.Death != runtime.DeathMemoryLimit || !got.Tells() {
				t.Fatalf("the end that cut off nothing left the record %+v, want its own type over the death's untold record and words", got)
			}
			sent := len(h.prompts())
			h.relaunched()
			told := h.wantPrompts(sent + 1)[sent].Message
			if !strings.Contains(told, "8 GiB memory limit") || strings.Contains(told, endDisplaced) || strings.Contains(told, endFromOutside) {
				t.Fatalf("the relaunched role was sent %q, want the plain words of the death that killed its process", told)
			}
			if !strings.HasSuffix(told, owedTask) {
				t.Fatalf("the relaunched role was sent %q, want the task %q", told, owedTask)
			}
		})
	}
}

// A death died decided and told — its task taken back, sent again and confirmed by a turn — and
// then ended by a read that found the process running leaves its record of the launch told and
// outlived (EndedWork.Outlived). The process ran on, so a later end of it that cuts off the turn
// it is in is that end's own to record and to tell: recordEnd records it afresh, with its own
// turn and task, untold, under its own cause and type, as for a first end of the launch, and the
// relaunched role is told that end and sent its task again.
//
// The negative control is recordEnd keeping a record of this launch that a running verdict
// outlived: the later end then records nothing, the told record stands, and the role is sent its
// task with no sentence of the end that cut its turn off.
func TestAnEndAfterARunningReadRecordsTheTurnItCutsOff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		end   runtime.Observation
		cause string
	}{
		{name: "a death", end: runtime.Observation{Kind: runtime.NotRecordedProcess}, cause: "the process died"},
		{name: "a displacement", end: runtime.Observation{Kind: runtime.Gone, Displaced: true}, cause: endDisplaced},
		{name: "an end from outside the pod", end: runtime.Observation{Kind: runtime.Gone, EndReading: runtime.PodEndNotOurs}, cause: endFromOutside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ended := workingOverTheProcessItsRefusedRelaunchKept(t)
			readRunning(t, h)
			h.must(StreamTurnEnd{Claim: h.token})
			h.must(StreamTurnStart{Claim: h.token})
			h.must(StreamTurnEnd{Claim: h.token})
			const task = "the next task"
			h.must(RequestDeliver{Claim: h.token, Task: task})
			h.must(StreamTurnStart{Claim: h.token})
			if got, p := h.claim().EndedWork, h.pending(); !got.Told || got.Tells() || p.Task != task || p.ConfirmedAt.IsZero() {
				t.Fatalf("the death's record is %+v over the task %+v, want it told by the task sent again and a new task's turn running", got, p)
			}
			end := tc.end
			end.Locator, end.At = ended, h.clock.Now()
			h.must(RuntimeObservation{Observation: end})
			if got := h.claim().EndedWork; !got.Tells() || !got.Turn || !got.TaskTurn || !strings.HasPrefix(got.Cause, tc.cause) {
				t.Fatalf("the end left the record %+v, want its own record of the turn it cut off, untold, under %q", got, tc.cause)
			}
			if h.m.View().WakeWaiting != nil {
				h.advanceToTurn(h.m)
			}
			sent := len(h.prompts())
			h.relaunched()
			told := h.wantPrompts(sent + 1)[sent].Message
			if !strings.Contains(told, "ended because "+tc.cause) || !strings.HasSuffix(told, task) {
				t.Fatalf("the relaunched role was sent %q, want it told the end that cut its turn off, %q, and its task", told, tc.cause)
			}
		})
	}
}

// A read that finds the process running ends the death's take-back with its judgement
// (reportedProcess, Machine.cutTurnOf). The death took the task back from the turn the process
// was in, and the mark of that take-back names the task under the id the take-back gave it; the
// process ran on, so that task, sent again and confirmed by its next turn, is in no turn the
// death cut off. A suspension held for that turn runs at its end (suspendHeldAtEnd), whose
// let-go cuts off nothing: the turn is over, its result in, and the activity report says nothing
// runs. So the suspension records nothing, the death's record stays told, and the resumed role
// is told nothing.
//
// The negative control is reportedProcess leaving the mark: the suspension's let-go then records
// the task's turn as cut off (EndedWork.TaskTurn), and the resumed role is told that the turn it
// was in was interrupted.
func TestASuspensionAtTheEndOfATurnAfterARunningReadRecordsNoTurnCutOff(t *testing.T) {
	h, _ := workingOverTheProcessItsRefusedRelaunchKept(t)
	readRunning(t, h)
	h.must(StreamTurnEnd{Claim: h.token})
	h.must(StreamTurnStart{Claim: h.token})
	p := h.pending()
	if p.ConfirmedAt.IsZero() || !h.claim().EndedWork.Told {
		t.Fatalf("the task sent again is %+v under the record %+v, want it confirmed and the death's record told", p, h.claim().EndedWork)
	}
	h.reportActivity()
	h.must(StreamPromptResult{Claim: h.token, DeliveryID: p.ID, AgentInvoked: true})
	if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the suspension answered %v, want it held for the turn", err)
	}
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantState(StateSuspended)
	if got := h.claim().EndedWork; got.Tells() {
		t.Fatalf("the suspension at the turn's end left the record %+v telling, want nothing told of a turn no end cut off", got)
	}
	sent := len(h.prompts())
	h.must(RequestResume{Claim: h.token, By: SuspendedByOperator})
	if h.m.View().WakeWaiting != nil {
		h.advanceToTurn(h.m)
	}
	h.relaunched()
	h.wantPrompts(sent)
}

// idleAfterARunningRead is the claim workingOverTheProcessItsRefusedRelaunchKept leaves, after a
// read that found its process running: the turn the process was in ends, the task the death took
// back is sent again with the death's record and acknowledged, no turn starts on it, and the
// process's activity report says nothing runs. So the next end of the process cuts off nothing,
// and the judgement it is types the record the cleared death left (unjudgedRecord) rather than
// recording afresh (EndedWork.Outlived).
func idleAfterARunningRead(t *testing.T) (*harness, runtime.Locator) {
	t.Helper()
	h, ended := workingOverTheProcessItsRefusedRelaunchKept(t)
	readRunning(t, h)
	h.must(StreamTurnEnd{Claim: h.token})
	h.reportActivity()
	c := h.claim()
	if c.State != StateIdle || c.Pending == nil || !c.Pending.ConfirmedAt.IsZero() || !c.EndedWork.Outlived || !c.EndedWork.Tells() {
		t.Fatalf("the claim is %s holding %+v over the record %+v, want it idle with its task sent and unconfirmed and the cleared death's record untold",
			c.State, c.Pending, c.EndedWork)
	}
	return h, ended
}

// A judgement of an end that cuts off nothing, after a read that found the process running,
// types the record the cleared death left under its own cause and type (unjudgedRecord), as
// the record of that end: a displacement types it displaced's (markDisplaced), so the role is
// told that its pod was replaced, and, its relaunch refused and the claim written, a restart and
// a report read neither way find the displacement standing (recordedDisplaced), which charges
// nothing; a death types it died's under that death's own cause (died); and an end from outside
// the pod types it as one from outside the pod under its cause (markFromOutside). Each relaunched
// role is told the end that judged its process last, with its task.
//
// The negative controls: markDisplaced leaving such a record as it stands fails the first row, the
// store holding the cleared death's record untyped; died typing it without its own cause fails
// the second, the record still the first death's; and markFromOutside typing it without its own
// cause fails the third, the record still the first death's.
func TestAnEndThatCutsOffNothingAfterARunningReadTypesTheRecordUnderItsOwnCause(t *testing.T) {
	relaunch := func(t *testing.T, h *harness, cause string) {
		t.Helper()
		if h.m.View().WakeWaiting != nil {
			h.advanceToTurn(h.m)
		}
		task := h.pending().Task
		sent := len(h.prompts())
		h.relaunched()
		told := h.wantPrompts(sent + 1)[sent].Message
		if !strings.Contains(told, "ended because "+cause) || !strings.HasSuffix(told, task) {
			t.Fatalf("the relaunched role was sent %q, want it told %q and its task", told, cause)
		}
	}
	t.Run("a displacement, its relaunch refused, a restart, then a report read neither way", func(t *testing.T) {
		h, ended := idleAfterARunningRead(t)
		store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
		h.m.deps.Store = store
		if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), Displaced: true}}); err == nil {
			t.Fatal("the relaunch was taken, want the lifecycle recheck's refusal of it")
		}
		store.failing = false
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Uncertain, At: h.clock.Now()}})
		if got := h.store.load(h.token).EndedWork; !got.Displaced || got.Died || got.Cause != endDisplaced || got.Death != "" || !got.Tells() {
			t.Fatalf("the store holds the record %+v, want displaced's under %q, still to be told", got, endDisplaced)
		}
		restartBehindAWave(t, h)
		charged, dying := h.budgets(), len(h.logs.lines(diedLine, "claim="+string(h.token)))
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}})
		if lines := len(h.logs.lines(diedLine, "claim="+string(h.token))); lines != dying || h.budgets() != charged {
			t.Fatalf("the report read neither way reached died, %d lines after %d, budgets %+v after %+v, want the displacement again, charged nothing",
				lines, dying, h.budgets(), charged)
		}
		relaunch(t, h, endDisplaced+".")
	})
	t.Run("a death", func(t *testing.T) {
		h, ended := idleAfterARunningRead(t)
		death := runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}
		h.must(RuntimeObservation{Observation: death})
		if got := h.claim().EndedWork; !got.Died || got.Cause != endDied(death) || got.FromOutside || got.Displaced {
			t.Fatalf("the claim records %+v, want died's record under %q", got, endDied(death))
		}
		relaunch(t, h, endDied(death))
	})
	t.Run("an end from outside the pod", func(t *testing.T) {
		h, ended := idleAfterARunningRead(t)
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}})
		if got := h.claim().EndedWork; !got.FromOutside || got.Died || got.Cause != endFromOutside || got.Death != "" {
			t.Fatalf("the claim records %+v, want a record of an end from outside the pod under %q", got, endFromOutside)
		}
		relaunch(t, h, endFromOutside+".")
	})
}
