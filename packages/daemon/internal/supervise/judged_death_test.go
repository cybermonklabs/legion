package supervise

// What becomes of a death died judged when a later read or report of the same process meets it:
// a read that finds the process running ends it, and a later report over a death died decided
// raises the relaunch the decision raised and leaves the decision's record as it stands.

import (
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
// is then judged afresh: continued charged nothing beyond what the death charged, its record
// typed as one from outside the pod under that end's cause (markFromOutside), its relaunch in
// the second queue, the next process told that its pod was ended from outside it. That holds
// after a restart between the read and the end, as the read's write carried the clear; for the
// sweep's report as for a read; and for a death died judged over a take-back the store refused
// as for one it decided. Left standing, the judgement sends that end to died (judgedDeath),
// which charges it as a second death of a process that ran on. A death died judges after the
// read is a death of its own: died types the record standing for the launch as its judgement
// again, so an end read as from outside the pod after it goes to died as that death's later
// report, and is charged as that one death.
//
// The negative controls: reportedProcess leaving the record's judgement standing fails every row,
// the claim and the store still holding the death after the read; died leaving the standing
// record of the launch untyped fails the last row, the end read as outside continued uncharged
// over the death died judged after the read; and markFromOutside keeping died's cause on the
// record it types fails the first four rows, the end decided from outside the pod under the
// sentence of a death.
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
// and died records nothing over an end already decided: the decision's finding that the end
// ended nothing worth recording stands, though the store now refuses the read of that activity.
// Recorded again, the end would read as a process that ran a turn and left no report of it, a
// record that tells, and the relaunched role, which held nothing, would be woken to be told its
// process died (queueRelaunch). So the claim holds no record of the ended launch, its death still
// died's, its relaunch in the first queue (deathRetyped), and it is charged that death's launch
// failure alone, which died's decision stored uncharged and the restart's first raise charges,
// with no death for a claim that held nothing (chargeDeath). The store answers that read again by
// the relaunch's turn, whose let-go reads the activity anew (letGo), and the relaunched role is
// sent nothing.
//
// The negative control is died recording the end over an end the claim holds decided on entry:
// the report then writes a record of the ended launch that tells.
func TestALaterReadOfADeathDecidedFirstWritesNoRecordOfAnEndFromOutsideThePod(t *testing.T) {
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
	firstAhead(h.deps.WakePace.Pacer, 1)
	h.store.fail("ClaimActivity", errStoreRefused)
	sent := len(h.prompts())
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}})
	h.wantWaitingWake(WakeLaunch)
	if wrong := deathRetyped(h, read); wrong != "" {
		t.Fatal(wrong)
	}
	for whose, c := range map[string]Claim{"the claim": h.claim(), "the store": h.store.load(h.token)} {
		if got := c.EndedWork; got.Generation == read || got.Tells() || c.Pending != nil {
			t.Fatalf("%s holds the record %+v and the delivery %+v, want no record of the ended launch and nothing to tell", whose, got, c.Pending)
		}
	}
	h.wantBudgets(Budgets{LaunchFailures: 1})

	h.store.fail("ClaimActivity", nil)
	h.advanceToTurn(h.m)
	h.relaunched()
	h.wantPrompts(sent)
	h.wantBudgets(Budgets{})
}
