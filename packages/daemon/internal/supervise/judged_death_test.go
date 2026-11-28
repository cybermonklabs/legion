package supervise

// What becomes of a death died judged when a later read or report of the same process meets it:
// a read that finds the process running ends it, and a later report over a death died decided
// raises the relaunch the decision raised and leaves the decision's record as it stands.

import (
	"errors"
	"reflect"
	"slices"
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
// read that found its process running: the read marks the record of the death told, the turn the
// process was in ends, the task the death took back is sent again as a task sent again and
// acknowledged, no turn starts on it, and the process's activity report says nothing runs. So
// the next end of the process cuts off nothing, and the judgement it is types the record the
// cleared death left (unjudgedRecord, retype) rather than recording afresh (EndedWork.Outlived).
func idleAfterARunningRead(t *testing.T) (*harness, runtime.Locator) {
	t.Helper()
	h, ended := workingOverTheProcessItsRefusedRelaunchKept(t)
	readRunning(t, h)
	h.must(StreamTurnEnd{Claim: h.token})
	h.reportActivity()
	c := h.claim()
	if c.State != StateIdle || c.Pending == nil || !c.Pending.ConfirmedAt.IsZero() || !c.EndedWork.Outlived || c.EndedWork.Tells() {
		t.Fatalf("the claim is %s holding %+v over the record %+v, want it idle with its task sent and unconfirmed and the cleared death's record told",
			c.State, c.Pending, c.EndedWork)
	}
	return h, ended
}

// A judgement of an end that cuts off nothing, after a read that found the process running,
// types the record the cleared death left under its own cause and type (unjudgedRecord), as
// the record of that end: a displacement types it displaced's (markDisplaced), so, its relaunch
// refused and the claim written, a restart and a report read neither way find the displacement
// standing (recordedDisplaced), which charges nothing; a death types it died's under that death's
// own cause (died); and an end from outside the pod types it as one from outside the pod under
// its cause (markFromOutside). Each end ended nothing worth telling, so the record stays told by
// no delivery (retype), and each relaunched role is sent its task alone, as with no false death
// before the end (the rows of
// TestAnEndThatCutsOffNothingAfterARunningReadTellsWhatItWouldWithNoFalseDeath compare the two).
//
// The negative controls: markDisplaced leaving such a record as it stands fails the first row, the
// store holding the cleared death's record untyped; died typing it without its own cause fails
// the second, the record still the first death's; and markFromOutside typing it without its own
// cause fails the third, the record still the first death's.
func TestAnEndThatCutsOffNothingAfterARunningReadTypesTheRecordUnderItsOwnCause(t *testing.T) {
	relaunch := func(t *testing.T, h *harness) {
		t.Helper()
		if h.m.View().WakeWaiting != nil {
			h.advanceToTurn(h.m)
		}
		task := h.pending().Task
		sent := len(h.prompts())
		h.relaunched()
		if told := h.wantPrompts(sent + 1)[sent].Message; told != task {
			t.Fatalf("the relaunched role was sent %q, want its task %q alone", told, task)
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
		if got := h.store.load(h.token).EndedWork; !got.Displaced || got.Died || got.Cause != endDisplaced || got.Death != "" || got.Tells() || got.ToldBy != "" {
			t.Fatalf("the store holds the record %+v, want displaced's under %q, told by no delivery", got, endDisplaced)
		}
		restartBehindAWave(t, h)
		charged, dying := h.budgets(), len(h.logs.lines(diedLine, "claim="+string(h.token)))
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}})
		if lines := len(h.logs.lines(diedLine, "claim="+string(h.token))); lines != dying || h.budgets() != charged {
			t.Fatalf("the report read neither way reached died, %d lines after %d, budgets %+v after %+v, want the displacement again, charged nothing",
				lines, dying, h.budgets(), charged)
		}
		relaunch(t, h)
	})
	t.Run("a death", func(t *testing.T) {
		h, ended := idleAfterARunningRead(t)
		death := runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}
		h.must(RuntimeObservation{Observation: death})
		if got := h.claim().EndedWork; !got.Died || got.Cause != endDied(death) || got.FromOutside || got.Displaced {
			t.Fatalf("the claim records %+v, want died's record under %q", got, endDied(death))
		}
		relaunch(t, h)
	})
	t.Run("an end from outside the pod", func(t *testing.T) {
		h, ended := idleAfterARunningRead(t)
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}})
		if got := h.claim().EndedWork; !got.FromOutside || got.Died || got.Cause != endFromOutside || got.Death != "" {
			t.Fatalf("the claim records %+v, want a record of an end from outside the pod under %q", got, endFromOutside)
		}
		relaunch(t, h)
	})
}

// A read that finds running a process died judged dead ends that death, and the record keeps no
// turn of it: no end cut that turn off. The read drops the record's turn (EndedWork.Turn,
// TaskTurn) with the charge, Died and the take-back's mark, and takes the marks the death's
// take-back left on the task off it (Delivery.Interrupted, OutOfMemory; reportedProcess). A later
// end of the process records only what it cut off itself: where it cuts off the turn then in
// flight, the role is told that turn was interrupted, under that end's cause. The row checks the
// whole of what the role is told: the relaunched task's text and the record's sentence alone
// (RelaunchSentence), and the task sent again to the running process, which tells no end
// (TestARunningReadTellsNothingOfTheDeathItEnded). A later end that cuts off nothing is the rows'
// of TestAnEndThatCutsOffNothingAfterARunningReadTellsWhatItWouldWithNoFalseDeath.
//
// The negative controls: reportedProcess keeping the death's turn on the record fails the row at
// its check of the record after the read; and reportedProcess keeping the take-back's marks on
// the task fails it at its check of the task after the read.
func TestARunningReadLeavesNoTurnOfTheDeathItEnded(t *testing.T) {
	const task = "implement the plan"
	const lost = "No result of that work will come to you. Your transcript does not say so. "
	const outside = "its pod was ended from outside it"
	relaunched := func(t *testing.T, h *harness) string {
		t.Helper()
		if h.m.View().WakeWaiting != nil {
			h.advanceToTurn(h.m)
		}
		sent := len(h.prompts())
		h.relaunched()
		return h.wantPrompts(sent + 1)[sent].Message
	}
	t.Run("a later end that cuts off the turn in flight", func(t *testing.T) {
		h, ended := workingOverTheProcessItsRefusedRelaunchKept(t)
		readRunning(t, h)
		if got, p := h.claim().EndedWork, h.pending(); got.Turn || got.TaskTurn || p.Interrupted || p.OutOfMemory {
			t.Fatalf("after the read the record is %+v over the task %+v, want neither saying a turn was cut off", got, p)
		}
		sent := len(h.prompts())
		h.must(StreamTurnEnd{Claim: h.token})
		if again, want := h.wantPrompts(sent + 1)[sent].Message, sentAgainTask+task; again != want {
			t.Fatalf("the running process was sent\n%q\nwant\n%q", again, want)
		}
		h.must(StreamTurnStart{Claim: h.token})
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}})
		cutOff := "Your tool calls in flight were never answered. The daemon has no report of the background work of that turn. " +
			"So the subagents and background jobs of that turn are gone. " +
			"Every other subagent and background job that you started in that process is gone too. " + lost
		if got, want := RelaunchSentence(h.claim().EndedWork), "Your previous process ended because "+outside+". The turn you were in was interrupted. "+
			cutOff+"Before you start any of that work again, check what it already did."; got != want {
			t.Fatalf("the record's sentence is\n%q\nwant\n%q", got, want)
		}
		if got, want := relaunched(t, h), "Your previous turn on this task was interrupted. Your process ended because "+outside+". "+
			cutOff+"Before you do any of that work again, check what it already did. Then continue the task.\n\n"+task; got != want {
			t.Fatalf("the relaunched role was sent\n%q\nwant\n%q", got, want)
		}
	})
}

// A read that finds the process running leaves no trace of the false death it ends in what a
// later end tells (dispatch://INFRA-1117/comment/e75ec747-1c70-44b9-86c9-750fa8309a59, corrected
// by dispatch://INFRA-1117/comment/d5fc3b9c-3425-4f4e-9920-fb8f476d0e37). The record the read
// leaves is as none: it is told by no delivery and holds no fact of that death, no turn, no
// task's turn, no background work and no report (reportedProcess). So a later end that cuts off
// nothing, whichever of the three judgements that type that record judges it (retype: died,
// markDisplaced, markFromOutside), tells exactly what the same end tells with no false death
// before it: a role holding its task, which a turn of the agent's own kept waiting, is sent that
// task alone, and a role holding none is sent nothing and is not woken for it. The exception,
// which the list in the EndedWork doc names, is a death the runtime read a plain cause of (the
// memory limit or an eviction) of a role holding its task: recordEnd keeps it whatever it cut off
// (kept), and the role is told it, with no false death as with one; this row's death has no
// plain cause. Each row runs its steps twice, the first time on a harness of its own without
// the false death and the read, and compares every prompt the two claims were sent, whole. The
// false death comes in the agent's own turn, whose activity report names a subagent, so its
// record names the turn, the subagent and a report of the turn; and it takes nothing back, so
// the task stands the same in both harnesses.
//
// The negative controls: reportedProcess keeping the death's background work and report on the
// record (EndedWork.Background, Kinds, Reported) fails every row at its check of the record after
// the read; and retype untelling the record a running verdict outlived, as it did, fails every
// row at its comparison, the role holding a task told the later end in front of it and the role
// holding none sent a delivery of the daemon's own.
func TestAnEndThatCutsOffNothingAfterARunningReadTellsWhatItWouldWithNoFalseDeath(t *testing.T) {
	const task = "implement the plan"
	// sends walks a paced claim through the row's steps and answers the harness and every prompt
	// its claim was sent: ready, a turn of the agent's own whose activity report names a subagent,
	// and the task delivered in that turn where withTask is set; where falseDeath is set, a death
	// of the process whose relaunch the lifecycle recheck refuses at its turn, and a read that
	// finds the process running; then the turn's end, an activity report that says nothing runs,
	// the later end, and the relaunch's turn and ready.
	sends := func(t *testing.T, withTask, falseDeath bool, end runtime.Observation) (*harness, []string) {
		t.Helper()
		h := pacedHarness(t, wakeRate)
		h.reach(StateReady)
		h.must(StreamTurnStart{Claim: h.token})
		if withTask {
			h.must(RequestDeliver{Claim: h.token, Task: task})
		}
		h.reportActivity("subagent")
		if falseDeath {
			store := &unansweredCheckLaunchStore{memStore: h.store}
			h.m.deps.Store = store
			h.observe(runtime.Gone)
			waiting := h.wantWaitingWake(WakeLaunch)
			store.failing = true
			h.advance(waiting.At.Sub(h.clock.Now()))
			store.failing = false
			if e := h.claim().EndedWork; !e.Died || !e.Turn || e.TaskTurn || !e.Background || len(e.Kinds) == 0 || !e.Reported {
				t.Fatalf("premise: the false death recorded %+v, want died's record of the turn, its subagent and its report", e)
			}
			readRunning(t, h)
			for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
				if e := c.EndedWork; e.Tells() || e.Turn || e.TaskTurn || e.Background || len(e.Kinds) > 0 || e.Reported {
					t.Fatalf("%s holds the record %+v after the read, want it told and holding no fact of the death the read ended", whose, e)
				}
			}
		}
		h.must(StreamTurnEnd{Claim: h.token})
		h.reportActivity()
		end.Locator, end.At = h.locator(), h.clock.Now()
		h.must(RuntimeObservation{Observation: end})
		if h.m.View().WakeWaiting != nil {
			h.advanceToTurn(h.m)
		}
		h.relaunched()
		var sent []string
		for _, p := range h.prompts() {
			sent = append(sent, p.Message)
		}
		return h, sent
	}
	for _, end := range []struct {
		name        string
		observation runtime.Observation
	}{
		{"a death", runtime.Observation{Kind: runtime.NotRecordedProcess}},
		{"a displacement", runtime.Observation{Kind: runtime.Gone, Displaced: true}},
		{"an end from outside the pod", runtime.Observation{Kind: runtime.Gone, EndReading: runtime.PodEndNotOurs}},
	} {
		for _, withTask := range []bool{true, false} {
			name := end.name + ", a role holding its task"
			if !withTask {
				name = end.name + ", a role holding no task"
			}
			t.Run(name, func(t *testing.T) {
				base, want := sends(t, withTask, false, end.observation)
				h, got := sends(t, withTask, true, end.observation)
				if withTask && len(want) != 2 || !withTask && (len(want) != 0 || base.claim().Pending != nil) {
					t.Fatalf("premise: with no false death the role was sent %q and holds %+v, want its task at the turn's end and at the relaunch's ready where it holds one, and nothing where it holds none",
						want, base.claim().Pending)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("after the false death and the read the role was sent\n%q\nwant what the same steps send with no false death\n%q", got, want)
				}
				if !withTask && h.claim().Pending != nil {
					t.Fatalf("the role holding no task holds %+v after the relaunch, want it woken for nothing", *h.claim().Pending)
				}
			})
		}
	}
}

// A read that finds running a process died judged dead ends the record of that death: the end
// never came, so the read marks the record told, by no delivery (EndedWork.Outlived, ToldBy
// empty), and nothing tells it: no prompt to that process, no relaunch task and no architect's
// catch-up, which reads Tells (internal/daemon's ready). Where the death's take-back never
// landed, the task is still confirmed to the process, and the next task it is sent is its text
// alone. Where it landed, the task goes to the process again behind the one sentence of a task
// sent again (sentAgainTask, EndedWork.SentAgain), on this daemon and after a restart, which
// reads the record from the store; its turn confirms it, which ends its sends under that id, so
// the next task, of another id, is its text alone. Each row checks the whole text of each prompt
// the process gets after the read.
//
// The negative controls: reportedProcess leaving the record untold fails each row, the prompt
// after the read telling the process that it died; and Delivery.message giving no sentence to the
// task sent again fails the rows where the take-back landed, that task sent bare.
func TestARunningReadTellsNothingOfTheDeathItEnded(t *testing.T) {
	const task, next = "implement the plan", "the next task"
	// told fails unless the record of the death is told by no delivery, in memory and stored.
	told := func(t *testing.T, h *harness) {
		t.Helper()
		for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
			if e := c.EndedWork; e.Tells() || e.ToldBy != "" || !e.Outlived {
				t.Fatalf("%s holds the record %+v after the read, want the death's record told by no delivery", whose, e)
			}
		}
	}
	// sends fails unless the process's next prompt, after do, is want whole.
	sends := func(t *testing.T, h *harness, want string, do ...Event) {
		t.Helper()
		sent := len(h.prompts())
		for _, ev := range do {
			h.must(ev)
		}
		if got := h.wantPrompts(sent + 1)[sent].Message; got != want {
			t.Fatalf("the running process was sent\n%q\nwant\n%q", got, want)
		}
	}
	t.Run("its take-back never landed", func(t *testing.T) {
		h, _ := judgedOverARefusedTakeBack(t)
		readRunning(t, h)
		if p := h.pending(); p.ConfirmedAt.IsZero() || h.claim().EndedWork.SentAgain != "" {
			t.Fatalf("the read left the task %+v under the record %+v, want it confirmed and sent again never", p, h.claim().EndedWork)
		}
		sends(t, h, next, StreamTurnEnd{Claim: h.token}, RequestDeliver{Claim: h.token, Task: next})
		told(t, h)
	})
	for _, restart := range []bool{false, true} {
		name := "its take-back landed"
		if restart {
			name += "; a restart before the task goes again"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := workingOverTheProcessItsRefusedRelaunchKept(t)
			readRunning(t, h)
			if p, e := h.pending(), h.store.load(h.token).EndedWork; e.SentAgain != p.ID || p.Interrupted || p.OutOfMemory {
				t.Fatalf("the read left the task %+v under the stored record %+v, want it named the task sent again, unmarked", p, e)
			}
			if restart {
				h.restart()
			}
			sends(t, h, sentAgainTask+task, StreamTurnEnd{Claim: h.token})
			sends(t, h, next, StreamTurnStart{Claim: h.token}, StreamTurnEnd{Claim: h.token}, RequestDeliver{Claim: h.token, Task: next})
			told(t, h)
		})
	}
}

// A read that finds the process running after a death whose take-back the store refused leaves
// the task and the record as that death found them. An earlier end took the task back, marking it
// interrupted by a pod that ran out of memory; the relaunch's turn confirmed it and its prompt's
// result came, so that end's record is told. A death died judged then records the task's turn as
// cut off (EndedWork.TaskTurn, recordGoneEnd) before it tries the take-back, which the store
// refuses, so the task stays confirmed under its id, the earlier end's marks still on it. The
// death's record replaces the earlier end's told record and keeps it whole (EndedWork.Earlier),
// naming no task it took back (EndedWork.TookBack). The read reads the take-back by that id
// (reportedProcess): this death took nothing back, so no task is marked sent again
// (EndedWork.SentAgain), the task keeps its id, its confirmation and its marks, and the earlier
// end's record is back as it stood, told, in the claim and in the store.
//
// The negative controls: reportedProcess reading the take-back of a record that keeps an earlier
// one by the record's turn and the task's mark alone, the id aside, fails the row, the task named
// sent again and its marks gone; and recordEnd keeping an earlier record only where it still
// tells fails the row at its check of the record the death left.
func TestARunningReadAfterARefusedTakeBackLeavesTheTaskAsTheDeathFoundIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), OutOfMemory: true}})
	h.relaunched()
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamPromptResult{Claim: h.token, DeliveryID: h.pending().ID, AgentInvoked: true})
	before, earlier := h.pending(), h.claim().EndedWork
	if !before.Interrupted || !before.OutOfMemory || before.ConfirmedAt.IsZero() || earlier.Tells() {
		t.Fatalf("premise: the relaunch holds the task %+v under the record %+v, want it confirmed with the earlier end's marks, that end told",
			before, earlier)
	}
	h.store.fail("PutDelivery", errStoreRefused)
	if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}}); err == nil {
		t.Fatal("the take-back was written, want the store's refusal of it")
	}
	h.store.fail("PutDelivery", nil)
	if e, p := h.claim().EndedWork, h.pending(); !e.Died || !e.TaskTurn || e.Earlier == nil || !reflect.DeepEqual(*e.Earlier, earlier) ||
		e.TookBack != "" || p.ID != before.ID || p.ConfirmedAt.IsZero() {
		t.Fatalf("the death left the record %+v over the task %+v, want died's record of the task's turn keeping the earlier end's told record %+v whole, the task still confirmed under %s",
			e, p, earlier, before.ID)
	}
	readRunning(t, h)
	for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
		if p := c.Pending; p == nil || c.EndedWork.SentAgain != "" || p.ID != before.ID || p.ConfirmedAt.IsZero() ||
			p.Interrupted != before.Interrupted || p.OutOfMemory != before.OutOfMemory {
			t.Fatalf("%s holds the task %+v under the record %+v after the read, want %s still confirmed with the marks it held, interrupted=%v outOfMemory=%v, and sent again never",
				whose, p, c.EndedWork, before.ID, before.Interrupted, before.OutOfMemory)
		}
		if !reflect.DeepEqual(c.EndedWork, earlier) {
			t.Fatalf("%s holds the record %+v after the read, want the earlier end's put back as it stood, told: %+v", whose, c.EndedWork, earlier)
		}
	}
}

// A read that finds the process running after a death whose take-back the store refused leaves
// the marks of an earlier end the claim no longer records on the task. A death of the claim's
// process takes its task back, marked interrupted by a pod that ran out of memory, and its
// relaunch is refused; the store takes the delivery and refuses the claim, so the death's record
// and charge are lost at the restart that follows, and the claim stands over the same process
// holding the marked task and no record. The turn the process was in ends, the task goes to it
// again, and the next turn confirms it, marks and all. A second death, judged afresh, records the
// task's turn as cut off and keeps no earlier record, there being none (recordEnd), and the store
// refuses its take-back, so the task stays confirmed under its id with the first death's marks.
// The read reads the take-back of a record that keeps none by the task's confirmation as well as
// by the record's turn and the task's mark (reportedProcess): this death took nothing back, so no
// task is marked sent again (EndedWork.SentAgain), and the task keeps its id, its confirmation
// and its marks, in the claim and in the store.
//
// The negative control is reportedProcess reading that take-back by the record's turn and the
// task's mark alone: the row fails, the task named sent again and its marks gone.
func TestARunningReadAfterARefusedTakeBackKeepsTheMarksOfAnEndTheStoreLost(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	refused := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	h.m.deps.Store = refused
	h.store.fail("PutClaim", errStoreRefused)
	if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
		OutOfMemory: true}}); err == nil {
		t.Fatal("the death's claim write was taken, want the store's refusal of it")
	}
	h.store.fail("PutClaim", nil)
	h.restart()
	if c := h.claim(); c.EndedWork.Cause != "" || c.DeathCharged || c.Pending == nil || !c.Pending.Interrupted || !c.Pending.OutOfMemory ||
		!c.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("premise: the restart restored the record %+v, charged=%v, over the task %+v, want no record and no charge over the task the death took back, marked",
			c.EndedWork, c.DeathCharged, c.Pending)
	}
	h.must(StreamTurnEnd{Claim: h.token})
	h.must(StreamTurnStart{Claim: h.token})
	before := h.pending()
	if before.ConfirmedAt.IsZero() || !before.Interrupted || !before.OutOfMemory {
		t.Fatalf("premise: the next turn holds the task %+v, want it confirmed with the first death's marks", before)
	}
	h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	h.store.fail("PutDelivery", errStoreRefused)
	if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}}); err == nil {
		t.Fatal("the second death's take-back was written, want the store's refusal of it")
	}
	h.store.fail("PutDelivery", nil)
	if e, p := h.claim().EndedWork, h.pending(); !e.Died || !e.TaskTurn || e.Earlier != nil || p.ID != before.ID || p.ConfirmedAt.IsZero() {
		t.Fatalf("premise: the second death left the record %+v over the task %+v, want died's record of the task's turn, keeping no earlier one, the task still confirmed under %s",
			e, p, before.ID)
	}
	readRunning(t, h)
	for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
		if p := c.Pending; p == nil || c.EndedWork.SentAgain != "" || p.ID != before.ID || p.ConfirmedAt.IsZero() || !p.Interrupted || !p.OutOfMemory {
			t.Fatalf("%s holds the task %+v under the record %+v after the read, want %s still confirmed with the first death's marks, and sent again never",
				whose, p, c.EndedWork, before.ID)
		}
	}
}

// A task taken back from a turn goes again behind the sentence of the end that cut that turn off,
// and a false death that a read found running names no end there: every prompt after the read is
// what the same steps send with no false death
// (dispatch://INFRA-1117/comment/dbf49ff6-0883-4b79-8379-d306e3323b57), but for the sentence of a
// task sent again where the death's take-back landed, and the stale result of the prompt that
// take-back replaced (dispatch://INFRA-1117/comment/6e64dc44-8791-46de-9b01-fa305a750309). An OOM
// kill of launch G-1 cuts off its task's turn, in which a subagent ran, and its take-back marks
// the task interrupted and out of memory. The turn of relaunch G confirms the task, which tells
// G-1's record, and the daemon restarts before the prompt's result. A death of G then comes, its
// relaunch refused by the lifecycle recheck: its record replaces G-1's told record and keeps it
// whole (EndedWork.Earlier, recordEnd), and a read that finds G running puts that record back as
// it stood (reportedProcess). The result of the prompt G ran comes with a provider error, or as
// not invoked, after the read or before it.
//
// Where the store refuses the death's take-back, the result takes the task back with its marks,
// and takes back what the task's confirmation told (takeBackPending, moveTelling), G-1's record
// among them where the death keeps it, so G-1's record tells again; the turn's end sends the task
// again, behind that record's sentence, once the retry's wait has passed. Every prompt the two
// claims were sent after the restart is compared whole: the one retry, behind G-1's interruption
// under G-1's cause, its subagent and the out-of-memory note G-1's death gives, and the note of
// the provider error where there was one.
//
// Where the take-back lands, it replaces the prompt G was running, so that prompt's result is
// stale (fence) and no retry comes of it. The read puts G-1's marks back on the task
// (EndedWork.TookBackEarlierTask), and the turn's end sends the task again to G behind G-1's end,
// told in its record's words with its out-of-memory note, and then the sentence of a task sent
// again (Delivery.message). That one prompt, with the sentence of a task sent again
// (sentAgainTask) taken out, is compared whole with the retry of the run with no false death,
// less the provider-error note that retry carries (Delivery.ProviderErrorNote, read off that run's
// task); and that sentence stands right in front of the task. Each row runs with G-1's memory
// limit recorded and without it, its run with no false death first, on a harness of its own.
//
// The negative controls: recordEnd keeping an earlier record only where it still tells fails
// every row. The false death's record replaces G-1's told record and keeps nothing, so the read
// leaves it outlived, under the false death's cause and told by no delivery. Where the store
// refused the take-back and the result comes after the read, the retry names the false death as
// the end that interrupted the task (interruptedTask); where it comes before, the read takes the
// result's take-back for the death's, and the retry goes behind the sentence of a task sent again
// with no word of G-1's end; and where the take-back landed, the read takes G-1's marks off the
// task, which fails the row at its check of the task after the read. moveTelling leaving the
// telling of the record a death keeps as it stood fails the refused rows whose result comes
// before the read: the read puts G-1's record back told, and the retry names no subagent and says
// the daemon has no report of that turn. Delivery.message telling no end in front of a task sent
// again whose record is told, as it did, fails every landed row at its comparison: the task goes
// behind the sentence of a task sent again alone.
func TestARetryAfterARunningReadNamesTheEndItWouldWithNoFalseDeath(t *testing.T) {
	const oomDetail = "the pod's roles container was OOMKilled"
	// sends walks a claim through the row's steps. It answers the cause G-1's record holds, every
	// prompt the claim was sent after the restart, and the task the claim holds at the end.
	sends := func(t *testing.T, cause runtime.DeathCause, invoked, falseDeath, landed, resultFirst bool) (string, []string, Delivery) {
		t.Helper()
		h := newHarness(t)
		h.reach(StateWorking)
		h.reportActivity("subagent")
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
			Detail: oomDetail, OutOfMemory: true, Cause: cause}})
		earlier := h.claim().EndedWork.Cause
		h.relaunched()
		h.must(StreamTurnStart{Claim: h.token})
		if p, e := h.pending(), h.claim().EndedWork; !p.Interrupted || !p.OutOfMemory || p.ConfirmedAt.IsZero() || e.Tells() {
			t.Fatalf("premise: G's turn holds the task %+v under the record %+v, want it confirmed with G-1's marks and G-1's record told", p, e)
		}
		h.restart()
		// ran is the prompt G ran, whose result names it.
		sent, ran := len(h.prompts()), h.pending().ID
		result := func() {
			r := StreamPromptResult{Claim: h.token, DeliveryID: ran}
			if invoked {
				r.AgentInvoked, r.Error, r.HTTPStatus = true, "overloaded_error", 529
			}
			h.must(r)
		}
		if falseDeath {
			store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
			h.m.deps.Store = store
			if !landed {
				h.store.fail("PutDelivery", errStoreRefused)
			}
			err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
				Detail: "a false death"}})
			h.store.fail("PutDelivery", nil)
			store.failing = false
			if p := h.pending(); landed && (p.ID == ran || !p.ConfirmedAt.IsZero()) || !landed && (err == nil || p.ID != ran) {
				t.Fatalf("premise: the false death answered %v and left the task %+v, want its take-back landed=%v", err, p, landed)
			}
			if resultFirst {
				result()
			}
			readRunning(t, h)
			switch p, e := h.pending(), h.claim().EndedWork; {
			case landed && (!p.Interrupted || !p.OutOfMemory || !p.ConfirmedAt.IsZero() || e.SentAgain != p.ID || e.Cause != earlier || e.Tells()):
				t.Fatalf("after the read G holds the task %+v under the record %+v, want it sent again with G-1's marks under G-1's told record", p, e)
			case !landed && !resultFirst && (!p.Interrupted || !p.OutOfMemory || p.ConfirmedAt.IsZero()):
				t.Fatalf("premise: after the read G holds the task %+v, want it confirmed with G-1's marks", p)
			}
		}
		if !falseDeath || !resultFirst {
			result()
		}
		h.must(StreamTurnEnd{Claim: h.token})
		h.advance(promptRetryMax)
		var got []string
		for _, p := range h.prompts()[sent:] {
			got = append(got, p.Message)
		}
		return earlier, got, h.pending()
	}
	for _, limit := range []struct {
		name  string
		cause runtime.DeathCause
		note  string
	}{
		{"G-1's memory limit recorded", runtime.DeathCause{Kind: runtime.DeathMemoryLimit, Limit: 8 << 30}, outOfMemoryGuidance},
		{"no memory limit recorded", runtime.DeathCause{}, outOfMemoryTask},
	} {
		for _, landed := range []bool{false, true} {
			for _, invoked := range []bool{true, false} {
				for _, resultFirst := range []bool{false, true} {
					name := limit.name + ", the take-back refused"
					if landed {
						name = limit.name + ", the take-back landed"
					}
					if invoked {
						name += ", a provider error"
					} else {
						name += ", a result not invoked"
					}
					if resultFirst {
						name += ", before the read"
					} else {
						name += ", after the read"
					}
					t.Run(name, func(t *testing.T) {
						earlier, want, held := sends(t, limit.cause, invoked, false, landed, resultFirst)
						_, got, _ := sends(t, limit.cause, invoked, true, landed, resultFirst)
						if len(want) != 1 || !strings.HasPrefix(want[0], "Your previous turn on this task was interrupted. Your process ended because "+earlier+". ") ||
							!strings.Contains(want[0], saysSubagentStopped) || !strings.Contains(want[0], limit.note) ||
							strings.Contains(want[0], outOfMemoryStatement) != (limit.note == outOfMemoryTask) ||
							(held.ProviderErrorNote != "") != invoked || !strings.HasSuffix(want[0], held.Task) {
							t.Fatalf("premise: with no false death the role was sent %q holding %+v, want one retry of its task behind G-1's interruption under %q, its subagent, the note %q and a provider-error note where the result was a provider error",
								want, held, earlier, limit.note)
						}
						if !landed {
							if !slices.Equal(got, want) {
								t.Fatalf("after the false death and the read the role was sent\n%q\nwant what the same steps send with no false death\n%q", got, want)
							}
							return
						}
						// The retry of the run with no false death, less the note of the provider error
						// of the prompt the landed take-back replaced.
						retry := want[0]
						if held.ProviderErrorNote != "" {
							retry = strings.Replace(retry, held.ProviderErrorNote+"\n\n", "", 1)
						}
						if len(got) != 1 || strings.Count(got[0], sentAgainTask) != 1 || !strings.HasSuffix(got[0], sentAgainTask+held.Task) ||
							strings.Replace(got[0], sentAgainTask, "", 1) != retry {
							t.Fatalf("after the false death, its take-back landed, and the read the role was sent\n%q\nwant, with the sentence of a task sent again in front of the task, what the same steps send with no false death less the provider-error note of the replaced prompt\n%q",
								got, retry)
						}
					})
				}
			}
		}
	}
}

// A death that stands tells nothing of the told record it keeps. The claim holds an older
// let-go's record, told already (olderToldClaim), and a death in its task's turn replaces it and
// keeps it whole for a read that finds the process running (EndedWork.Earlier, recordEnd). No
// read comes, so the death stands: the record the relaunch's ready reads for the architect's
// catch-up (Tells, RelaunchSentence) is the death's alone, and the ready sends the task behind the
// death's own sentence; the relaunch's turn tells the death's record, and after a restart the
// restored claim and the store hold that record told, so nothing tells either record again; and
// a later death of the relaunched process, which keeps the first death's record in its place and
// drops the older one, sends the next relaunch the task behind its own sentence. Every prompt from
// the first death on names the cause of the end it tells and never the older let-go's
// (olderCause), and says once that its turn was interrupted.
func TestADeathThatStandsTellsNothingOfTheToldRecordItKeeps(t *testing.T) {
	h := newHarnessOf(t, olderToldClaim())
	h.reach(StateWorking)
	from := len(h.prompts())
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
		Detail: "the death that stands"}})
	first := h.claim().EndedWork
	if first.Earlier == nil || first.Earlier.Cause != olderCause || !first.Earlier.Told || !first.Tells() {
		t.Fatalf("premise: the death left the record %+v, want its own, untold, keeping the older told record whole", first)
	}
	h.relaunched()
	if e := h.claim().EndedWork; !e.Tells() || e.Cause != first.Cause || strings.Contains(RelaunchSentence(e), olderCause) {
		t.Fatalf("at the relaunch's ready the claim holds the record %+v, telling %q, want the death's own, which tells nothing of the older record",
			e, RelaunchSentence(e))
	}
	h.must(StreamTurnStart{Claim: h.token})
	h.restart()
	for whose, c := range map[string]Claim{"the restored claim": h.claim(), "the store": h.store.load(h.token)} {
		if e := c.EndedWork; e.Tells() || e.Cause != first.Cause || e.Earlier == nil || !e.Earlier.Told {
			t.Fatalf("after the relaunch's turn and a restart %s holds the record %+v, want the death's own, told, keeping the older record told",
				whose, e)
		}
	}
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
		Detail: "a later death"}})
	later := h.claim().EndedWork
	if later.Earlier == nil || later.Earlier.Cause != first.Cause || later.Earlier.Earlier != nil {
		t.Fatalf("the later death left the record %+v, want its own keeping the first death's, which keeps nothing", later)
	}
	h.relaunched()
	sent := h.prompts()[from:]
	if len(sent) != 2 {
		t.Fatalf("the claim was sent %q after the first death, want its task at each relaunch's ready", sent)
	}
	for i, cause := range []string{first.Cause, later.Cause} {
		if p := sent[i].Message; !strings.HasPrefix(p, "Your previous turn on this task was interrupted. Your process ended because "+cause+". ") ||
			strings.Contains(p, olderCause) || strings.Count(p, "interrupted") != 1 {
			t.Fatalf("relaunch %d was sent %q, want the task behind the sentence of %q alone, naming nothing of %q", i+1, p, cause, olderCause)
		}
	}
}

// A false death erases nothing. An OOM kill cuts off launch G-1's task turn, with or without a
// recorded memory limit. The relaunch G is sent the task behind that record's sentence. Before
// the telling is certain, G is wrongly judged dead with a turn cut off, so that death's record
// joins the earlier one (joined), which it keeps whole (EndedWork.Earlier), in the claim, in the
// store and in a copy of the claim handed out of the machine (copyClaim). Either its relaunch is
// refused and a read finds G running, or its relaunch still waits for its turn in the wake pacing
// and the sweep's report finds G running, which drops that relaunch (judge,
// dropOutlivedRelaunch); each on the daemon that judged the death or after a restart, whose
// boot's replay parks the waiting relaunch again. The read or the report puts the earlier record
// back, untold, and the death goes with all it added (reportedProcess). In the first row the turn
// is the agent's own, which its busy answer to the task said it was in (recordBusyTurn), so the
// task was never confirmed and the death took nothing back: the turn's end sends G the task again
// behind G-1's end, told in its own words as the end that interrupted the task's turn, and the
// note of the pod's memory, the marks G-1's take-back left on the task. In the second the turn is
// the task's own, whose confirmation of the prompt that carried the sentence the death takes back
// with the task (untellInFlight, interrupted), naming the id it gives the task
// (EndedWork.TookBack). The read restores the task's earlier interruption and memory marks, so
// that prompt reads the same as in the first row, with the sent-again sentence after the memory
// guidance. In both, the turn that confirms that prompt tells the record, and the next task goes
// as its text alone.
//
// Marking the joined record told whole fails every row: the prompt no longer carries G-1's
// record. Reading a joined record's take-back by its turn and the task's marks fails the
// busy-turn rows: a task the false death never took back gets the sent-again sentence.
// Skipping the restoration of the earlier marks fails the task-turn rows: the prompt loses
// its task-specific interruption and memory guidance. Restoring the memory mark from the death
// kind instead of the task fails the rows with no recorded memory limit. judge leaving the
// waiting relaunch as it stood fails every row whose relaunch waits: the claim still holds it.
func TestAFalseDeathErasesNoEarlierEndStillUntold(t *testing.T) {
	const next = "the next task"
	const oomDetail = "the pod's roles container was OOMKilled"
	const memory = "the pod's roles container, which runs every role of this issue, was killed because it used more than its " +
		`8 GiB memory limit. The runtime's detail: "gone: ` + oomDetail + `"`
	const noLimit = `the process died: "gone: ` + oomDetail + `"`
	const cut = "Your tool calls in flight were never answered. The daemon has no report of the background work of that turn. " +
		"So the subagents and background jobs of that turn are gone. " +
		"Every other subagent and background job that you started in that process is gone too. " +
		"No result of that work will come to you. Your transcript does not say so. "
	const interrupted = "Your previous turn on this task was interrupted. Your process ended because "
	const instruction = "Before you do any of that work again, check what it already did. Then continue the task.\n\n"
	const afterEarlierEnd = interrupted + memory + ". " + cut + instruction + outOfMemoryGuidance
	taskTurn := func(h *harness, _ fake.Prompt) { h.must(StreamTurnStart{Claim: h.token}) }
	for _, row := range []struct {
		name string
		// turn puts G in a turn over the ready's prompt, which is its to answer.
		turn func(h *harness, ready fake.Prompt)
		// tookBack is whether the false death takes the task back, and want the whole text the
		// task goes to G in after the read.
		noLimit  bool
		tookBack bool
		want     string
	}{
		{name: "a turn the agent was in when its task came",
			turn: func(h *harness, ready fake.Prompt) {
				h.must(StreamPromptResult{Claim: h.token, DeliveryID: ready.DeliveryID, AgentInvoked: false, Error: busyWords, Outcome: runtime.OutcomeAgentBusy})
				h.must(StreamTurnStart{Claim: h.token})
			},
			want: afterEarlierEnd + owedTask},
		{name: "the task's own turn, its prompt's result still to come", tookBack: true,
			turn: taskTurn,
			want: afterEarlierEnd + sentAgainTask + owedTask},
		{name: "the task's own turn after an OOM kill with no recorded limit", tookBack: true, noLimit: true,
			turn: taskTurn,
			want: interrupted + noLimit + ". " + cut + instruction + outOfMemoryTask + sentAgainTask + owedTask},
	} {
		for _, v := range []struct{ waits, restart bool }{{}, {restart: true}, {waits: true}, {waits: true, restart: true}} {
			name := row.name
			if v.waits {
				name += "; the sweep's report while the relaunch waits"
			}
			if v.restart {
				name += "; a restart before the read"
			}
			t.Run(name, func(t *testing.T) {
				h := pacedHarness(t, wakeRate)
				h.reach(StateWorking)
				cause := runtime.DeathCause{Kind: runtime.DeathMemoryLimit, Limit: 8 << 30}
				wantCause := memory
				if row.noLimit {
					cause, wantCause = runtime.DeathCause{}, noLimit
				}
				h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
					Detail: oomDetail, OutOfMemory: true, Cause: cause}})
				earlier := h.claim().EndedWork
				if !earlier.Tells() || earlier.Cause != wantCause || earlier.Death != cause.Kind {
					t.Fatalf("G-1's death left %+v, want cause %q and kind %q", earlier, wantCause, cause.Kind)
				}
				h.advanceToTurn(h.m)
				sent := len(h.prompts())
				h.relaunched()
				ready := h.wantPrompts(sent + 1)[sent]
				row.turn(h, ready)

				store := &unansweredCheckLaunchStore{memStore: h.store}
				h.m.deps.Store = store
				h.observe(runtime.Gone)
				waiting := h.wantWaitingWake(WakeLaunch)
				if !v.waits {
					store.failing = true
					h.advance(waiting.At.Sub(h.clock.Now()))
					store.failing = false
				}
				handed := h.claim()
				for whose, c := range map[string]Claim{"the claim": handed, "the store": h.store.load(h.token)} {
					if e := c.EndedWork; !e.Died || e.Generation != c.Generation || !e.Turn || !e.Tells() || e.Earlier == nil ||
						!reflect.DeepEqual(*e.Earlier, earlier) || (e.TookBack == c.Pending.ID) != row.tookBack ||
						e.TookBackEarlierTask != row.tookBack || e.TookBackEarlierOutOfMemory != row.tookBack {
						t.Fatalf("%s holds the record %+v over the task %+v after the false death, want it joined over G-1's, which it keeps whole, naming the task where it took it back",
							whose, e, *c.Pending)
					}
				}
				handed.EndedWork.Earlier.Cause = "a copy's"
				if kept := h.claim().EndedWork.Earlier; kept.Cause != wantCause {
					t.Fatalf("a copy of the claim shares G-1's record with the machine, which now holds %+v", *kept)
				}
				if v.restart {
					h.restart()
					if v.waits {
						firstAhead(h.deps.WakePace.Pacer, 1)
						h.must(RecordedDeath{Claim: h.token, Generation: h.claim().Generation})
						h.wantWaitingWake(WakeLaunch)
					}
				}
				if v.waits {
					// G is still in its turn, so the sweep's question of a restored claim or of a
					// busy answer's turn finds it streaming, and the turn's own end sends the task.
					h.conn.SetStreaming(true)
					h.observe(runtime.Alive)
					h.conn.SetStreaming(false)
					if held := h.m.View().WakeWaiting; held != nil {
						t.Fatalf("the claim still holds wake %+v after the sweep found G running, want the false death's relaunch dropped", *held)
					}
				} else {
					readRunning(t, h)
				}
				sent = len(h.prompts())
				h.must(StreamTurnEnd{Claim: h.token})
				got := h.wantPrompts(sent + 1)[sent]
				if got.Message != row.want {
					t.Fatalf("after the read G was sent\n%q\nwant\n%q", got.Message, row.want)
				}
				for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
					want := earlier
					if row.tookBack {
						want.SentAgain = got.DeliveryID
					}
					if !reflect.DeepEqual(c.EndedWork, want) {
						t.Fatalf("%s holds the record %+v after the read, want G-1's put back, untold: %+v", whose, c.EndedWork, want)
					}
					if p := c.Pending; p == nil || !p.Interrupted || !p.OutOfMemory {
						t.Fatalf("%s holds task %+v, want G-1's interruption and memory marks", whose, p)
					}
				}

				h.must(StreamTurnStart{Claim: h.token})
				if e := h.claim().EndedWork; e.Tells() || e.ToldBy != got.DeliveryID {
					t.Fatalf("the turn of the task left the record %+v, want it told by %s", e, got.DeliveryID)
				}
				sent = len(h.prompts())
				h.must(StreamTurnEnd{Claim: h.token})
				h.must(RequestDeliver{Claim: h.token, Task: next})
				if again := h.wantPrompts(sent + 1)[sent].Message; again != next {
					t.Fatalf("G's next task went as %q, want its text alone", again)
				}
			})
		}
	}
}
