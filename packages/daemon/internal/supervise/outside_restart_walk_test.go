package supervise

// The random walk of an end from outside the pod met again, beside the table of every order in
// outside_restart_test.go, whose harness of the end and its events it shares.

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// outsideSeeds and outsideSteps bound TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders: the
// seeds it walks, one subtest each, and the most random steps one walk takes after the end.
const (
	outsideSeeds = 1000
	outsideSteps = 10
)

// The faults a random walk sets for the next writes (outsideWalk.setFault).
const (
	faultTakeBackRefused = "the take-back's PutDelivery refused"
	faultClaimRefused    = "the claim's PutClaim refused"
	faultUnknownLanded   = "writes of unknown outcome, landed"
	faultUnknownLost     = "writes of unknown outcome, not landed"
	// faultRelaunchRefused is the lifecycle recheck's refusal of every relaunch raised while it
	// is set, which leaves the claim over its ended process with no relaunch waiting, so the
	// reports after it reach judge rather than being dropped while one waits (launchWaits).
	faultRelaunchRefused = "the relaunch refused by the lifecycle recheck"
)

// An end from outside the pod holds its invariants in random orders of the events that can meet
// it, against the real machine and this file's harness, from a fixed list of seeds, so every run
// walks the same orders and a failing seed is replayed by its subtest's name. Each seed stands
// the claim in a turn of its confirmed task, ready holding its task sent and not confirmed, or,
// in about one seed of four, ready holding nothing, with no task and no turn, parks the relaunch
// behind another claim's wake, maybe sets a fault for the next writes, and sends the end: in
// about one seed of three a report read neither way, which died judges a death (diedFirst); in
// about one of the rest in four the daemon's own replacement of the pod, which displaced judges
// first (displacedFirst), its relaunch refused by the lifecycle recheck; a second after either;
// and otherwise a Gone report read as an end from outside the pod.
// Then come up to outsideSteps random steps, each one of a report read as outside, a report read
// neither way, an Uncertain report (a claim write), a fault set for the next writes (a write
// refused or of unknown outcome, or the relaunch refused by the lifecycle recheck), the faults
// cleared, a second passing, a daemon restart, and the boot's replay a restart owes where its
// store held the decision (internal/daemon's recordedDeaths). Before each of them, drawn from a
// stream of their own so the steps above stay the ones each seed walked before them, may come a
// read of the process at a caller's request that finds it running (ReadProcess), once in a walk,
// where the claim holds a death died judged, no launch waits and the store takes the claim's
// writes; and, on the daemon that read the end, where the claim stood holding its task sent and
// still holds it, a late acknowledgement of that task's prompt (acked), as the send in flight at
// the end gives one, or the shim's second answer for one prompt. An event the machine answers
// with an error is allowed. The walk closes with the faults cleared, the replay still owed, one
// report read neither way, the relaunch's turn and its ready.
//
// After every step, with the seed and the steps walked in each failure:
//
//  1. Where the end's first report was read as outside, or was the daemon's own replacement of
//     the pod, and no restart came while the store held no record of the ended launch typed as
//     an end from outside the pod or as displaced's, the claim is charged nothing, in memory or
//     in the store; otherwise, at most one death and its launch failure for each death judged,
//     and a read that finds the process running ends a judged death (reportedProcess), so the
//     end a later report finds is judged afresh.
//  2. The claim and the store hold the task, where the claim held one, at every step: it is
//     never retired.
//  3. Once the claim reads the end decided (Claim.DeathCharged), the end of the turn
//     (Claim.LastTurnEnd) does not move in it, and once the stored claim reads it decided, not in
//     the store either (turnEndWatch). A decision D1 made in memory and its store refused is gone
//     at a restart, and D2 makes it again, so the claim's watch starts again where the restored
//     claim is undecided; the store's never does. A read that finds the process running clears
//     the decision, in the claim and in the store, so both watches start again there.
//  4. At the relaunch's ready the task is sent once, never twice, and a claim that held nothing
//     is sent nothing; an end the claim held decided from outside the pod is never told as a
//     death (outsideWalk.relaunch).
//  5. While the claim holds its end decided from outside the pod, a paced wake the machine holds
//     at a place waits in the pacing's second queue: such an end charges no death, so its
//     relaunch is never placed as a charged death's (outsideEndQueued). And once the claim holds
//     a death of the ended launch that died judged (judgedDeath), decided or recorded with its
//     take-back refused, that first judgement stands for as long as the claim holds that launch,
//     or until a read finds the process running: the claim never reads as an end from outside
//     the pod, a paced wake it holds waits in the first queue, no step logs a line of an end
//     continued uncharged (continuedFromOutsideLine), and the relaunch tells the role nothing of
//     an end from outside the pod (deathRetyped, outsideWalk.deathStands, outsideWalk.relaunch).
//     Once the claim holds displaced's record of the ended launch (recordedDisplaced), that
//     first judgement stands for as long as the claim holds that launch: no report reaches died,
//     the claim never holds a death died judged, a paced wake it holds waits in the second queue,
//     and the relaunch tells the role nothing of a death (displacementRejudged,
//     outsideWalk.displacementStands, outsideWalk.relaunch). A restart whose store held neither
//     the record nor the decision forgets the judgement, and the claim it restores is judged
//     afresh. The claim reads a charged death only where died judged its process
//     (deathUnjudged): an end endedFromOutside decided, displaced's first judgement of it
//     included, reads as decided from outside the pod.
//  6. No prompt failure is charged for a process reported gone, and the turn-start timer
//     (TimerTurn) is never armed while the machine holds the process gone (Claim.ProcessGone).
//
// The negative controls are the table test's: judge routing on decidedFromOutside fails
// invariant 1, a report read neither way over a record with no decision charged a death, on one
// daemon or after a restart that kept the record; endedFromOutside deciding again at every
// report fails invariant 3, D2's report before the replay over a stored decision moving the end
// of the turn after the decision; WakeQueue reading Claim.DeathCharged in place of
// Claim.chargedDeath fails invariant 5; judge sending every report read as outside to
// endedFromOutside whatever the claim holds, as it did before judgedDeath, fails invariant 5's
// second half, a report read as outside over a death died judged continued as an end from
// outside the pod and logging that end's line, or invariant 3 first, where that report over a
// decided death moves the end of the turn; markFromOutside leaving a standing record of the
// launch untyped fails invariant 5's last sentence, a report read as outside over displaced's
// record deciding a charged death that died never judged; judge sending a report read neither
// way over displaced's record to died fails invariant 1 or the displacement's sentence of
// invariant 5; displaced leaving no record of an end that ended nothing worth telling fails
// invariant 1 where the claim stood holding nothing or holding its task sent and not confirmed;
// reportedProcess leaving the record's death judgement standing fails the running read's own
// check, the claim and the store still holding the death after it; markFromOutside keeping the
// cause of a death a running read ended on the record it types fails invariant 4's last
// sentence; and judge leaving TimerTurn armed on a gone verdict, or acked arming it over a gone
// process, fails invariant 6.
//
// The test fails unless some seed walks each of the three orders the walk was given for them:
// the running read over a judged death, the late acknowledgement over a gone process, and a
// report read neither way over displaced's record; it logs how many seeds walk each.
func TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders(t *testing.T) {
	var reads, acks, displacedReports int
	for seed := uint64(1); seed <= outsideSeeds; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			w := newOutsideWalk(t, seed)
			w.run()
			reads, acks = reads+min(w.reads, 1), acks+min(w.acks, 1)
			displacedReports += min(w.displacedReports, 1)
		})
	}
	t.Logf("seeds that walk a running read over a judged death: %d; a late acknowledgement over a gone process: %d; a report read neither way over displaced's record: %d",
		reads, acks, displacedReports)
	if !t.Failed() && (reads == 0 || acks == 0 || displacedReports == 0) {
		t.Fatal("a seed list that walks none of one of the three orders, want each walked")
	}
}

// outsideWalk is one random order of the events that can meet an end from outside the pod, and
// what its invariants read of the steps it has walked.
type outsideWalk struct {
	t    *testing.T
	seed uint64
	rng  *rand.Rand
	// more is a second stream from the seed, of the choices drawn beside rng's: whether the end's
	// first report is read neither way, whether the claim stands holding nothing, and whether a
	// fault set is the relaunch's refusal. Drawn apart, they leave every draw of rng as it is, so
	// a seed that takes none of them walks the order rng alone gives it.
	more *rand.Rand
	// extra is a fourth stream from the seed, of whether a running read or a late
	// acknowledgement comes before a random step where one can (readRunning, lateAck), drawn
	// apart for the reason more is.
	extra *rand.Rand
	h     *harness
	// ended and generation are the process the end ended and its launch, task the task the claim
	// held, "" for none, owing whether it stood holding that task sent and not confirmed, and sent
	// how many prompts went out before the end.
	ended      runtime.Locator
	generation uint64
	task       string
	owing      bool
	sent       int
	// walked is every step so far, in order, which a failure prints with the seed.
	walked []string
	// fault is the fault set for the next writes, "" for none.
	fault string
	// replayOwed is whether the last restart's boot owes the claim its replay, and replayRead the
	// generation that boot read.
	replayOwed bool
	replayRead uint64
	// lost is whether a restart came while the store held no record of the end typed as one from
	// outside the pod or as displaced's, and restarted whether any restart came.
	lost      bool
	restarted bool
	// diedFirst is whether the end's first report was read neither way, so died judged the end
	// a death, and displacedFirst whether it was the daemon's own replacement of the pod
	// (runtime.Observation.Displaced), so displaced judged it first. displacedFirst is drawn
	// from a third stream of the seed, for the reason more is drawn apart, and only where the
	// first report is not read neither way.
	diedFirst      bool
	displacedFirst bool
	// death is whether the claim holds a death of the ended launch that died judged
	// (judgedDeath), the first judgement over that launch, and relaunchedDeath whether the
	// claim held one when its relaunch let the ended process go (invariant 5's second half,
	// deathStands). continued is how many lines of an end continued uncharged from outside the
	// pod the claim had logged at the last check (continuedLines).
	death           bool
	relaunchedDeath bool
	continued       int
	// displacement is whether the claim holds displaced's record of the ended launch
	// (recordedDisplaced), the first judgement over that launch, and relaunchedDisplacement
	// whether it held one when its relaunch let the ended process go (invariant 5, the
	// displacement's sentence).
	displacement           bool
	relaunchedDisplacement bool
	// outside is whether the claim held its end decided from outside the pod
	// (decidedFromOutside) at the last check that found it recording the ended launch, which
	// the relaunch never tells as a death (outsideWalk.relaunch).
	outside bool
	// judged is how many deaths of the ended launch the claim may be charged: one, and one more
	// for each read that found the process running over a judged death (invariant 1).
	judged int
	// reads, acks and displacedReports are how many running reads, late acknowledgements, and
	// reports read neither way over displaced's record the walk took.
	reads, acks, displacedReports int
	// turnEnd holds the end of the turn where the decision of the end put it (invariant 3).
	turnEnd turnEndWatch
}

func newOutsideWalk(t *testing.T, seed uint64) *outsideWalk {
	w := &outsideWalk{t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, 0)), more: rand.New(rand.NewPCG(seed, 1)),
		extra: rand.New(rand.NewPCG(seed, 3)), h: pacedHarness(t, wakeRate), judged: 1}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("seed %d walked: %s", seed, strings.Join(w.walked, "; "))
		}
	})
	return w
}

// fail ends the walk with the seed and the steps walked so far in front of what failed.
func (w *outsideWalk) fail(format string, args ...any) {
	w.t.Helper()
	w.t.Fatalf("seed %d, after %s: %s", w.seed, strings.Join(w.walked, "; "), fmt.Sprintf(format, args...))
}

// run walks the seed's order: the claim stood, the end, the random steps and the close.
func (w *outsideWalk) run() {
	h := w.h
	w.diedFirst = w.more.IntN(3) == 0
	w.displacedFirst = !w.diedFirst && rand.New(rand.NewPCG(w.seed, 2)).IntN(4) == 0
	nothing, inTurn := w.more.IntN(4) == 0, w.rng.IntN(2) == 0
	switch {
	case nothing:
		h.reach(StateReady)
		w.walked = append(w.walked, "stood ready holding nothing")
	case inTurn:
		h.reach(StateWorking)
		w.task = owedTask
		w.walked = append(w.walked, "stood in a turn of its confirmed task")
	default:
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: owedTask})
		w.task, w.owing = owedTask, true
		w.walked = append(w.walked, "stood ready holding its task sent and not confirmed")
	}
	queueAhead(h, 1)
	w.ended, w.generation, w.sent = h.locator(), h.generation(), len(h.prompts())
	second := func() { w.step("a second passed", func() error { h.advance(time.Second); return nil }) }
	// A fault before the end, so the end's own take-back and decision can fail as a later
	// report's can. A displaced end's relaunch is refused, so the claim stands over the ended
	// process holding displaced's record of the end, and the reports after it reach judge rather
	// than being dropped while that relaunch waits (launchWaits).
	switch {
	case w.displacedFirst:
		w.setFaultTo(faultRelaunchRefused)
	case w.rng.IntN(2) == 0:
		w.setFault()
	}
	switch {
	case w.diedFirst:
		w.report("the end, read neither way", runtime.NotRecordedProcess, "")
		// The runtime reads the end as one from outside the pod only at a later event of the pod,
		// the one that carries DisruptionTarget, so the reports after this one come later.
		second()
	case w.displacedFirst:
		w.step("the end, the daemon's replacement of the pod", func() error {
			return h.handle(RuntimeObservation{Observation: runtime.Observation{
				Locator: w.ended, Kind: runtime.Gone, At: h.clock.Now(), Displaced: true,
			}})
		})
		// A read of the same pod as ended from outside it comes at a later event, as after a
		// restart that lost the runtime's record of the replacement (sandbox's meanToEndPod).
		second()
	default:
		w.report("the end, read as outside", runtime.Gone, runtime.PodEndNotOurs)
	}
	for range w.rng.IntN(outsideSteps + 1) {
		w.beside()
		steps := []func(){
			func() { w.report("a report read as outside", runtime.Gone, runtime.PodEndNotOurs) },
			func() { w.report("a report read neither way", runtime.NotRecordedProcess, "") },
			func() { w.report("an Uncertain report", runtime.Uncertain, "") },
			w.setFault,
			w.clearFaults,
			second,
			w.restart,
		}
		if w.replayOwed {
			steps = append(steps, w.replay)
		}
		steps[w.rng.IntN(len(steps))]()
	}
	w.clearFaults()
	if w.replayOwed {
		w.replay()
	}
	w.report("a report read neither way", runtime.NotRecordedProcess, "")
	w.relaunch()
}

// step runs one step, notes it, and checks the invariants after it. Its error is allowed: a
// write the store refused or whose outcome is unknown fails the event that made it.
func (w *outsideWalk) step(name string, run func() error) {
	w.t.Helper()
	if err := run(); err != nil {
		name += " (answered: " + err.Error() + ")"
	}
	w.walked = append(w.walked, name)
	w.check()
}

// report is a step of the runtime's report of the ended process, of kind read as reading.
func (w *outsideWalk) report(name string, kind runtime.ObservationKind, reading runtime.PodEndReading) {
	if c := w.h.claim(); kind == runtime.NotRecordedProcess && c.Generation == w.generation && c.recordedDisplaced() {
		w.displacedReports++
	}
	w.step(name, func() error {
		return w.h.handle(RuntimeObservation{Observation: runtime.Observation{
			Locator: w.ended, Kind: kind, At: w.h.clock.Now(), EndReading: reading,
		}})
	})
}

// beside is the steps drawn from extra before a random step, each one time in three where it
// can come: a running read over a death died judged (readRunning), once in a walk, where no
// launch waits and the store takes every write but the relaunch's recheck, and a late
// acknowledgement of the prompt of the task the claim stood owing (lateAck). A second running
// read would let a third death be judged, which fails the claim at the deaths limit (chargeDeath)
// rather than relaunching it, and the walk closes on the relaunch.
func (w *outsideWalk) beside() {
	c := w.h.claim()
	ours := c.Generation == w.generation && c.Locator != nil
	writes := w.fault == "" || w.fault == faultRelaunchRefused
	if ours && w.reads == 0 && c.judgedDeath() && w.h.m.View().WakeWaiting == nil && writes && w.extra.IntN(3) == 0 {
		w.readRunning()
	}
	if c = w.h.claim(); ours && w.owing && !w.restarted && c.Pending != nil && c.Pending.ConfirmedAt.IsZero() &&
		w.extra.IntN(3) == 0 {
		w.lateAck()
	}
}

// readRunning is a step of a read of the process at a caller's request (ReadProcess) that finds
// it running, over a death died judged, with no launch waiting, which a read would not make
// (ProcessReplacedByLaunch), and with the store taking the claim's write. The death is over
// (reportedProcess): the claim reads no judged death and no decision, in memory and in the store,
// so the watches of that judgement and of the end of the turn its decision put start again
// (invariants 3 and 5), and the next death judged of the process is one of its own (invariant 1).
func (w *outsideWalk) readRunning() {
	w.reads++
	w.step("a read at a caller's request, finding the process running", func() error {
		h := w.h
		h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Alive})
		if got := h.m.ReadProcess(h.ctx); got != ProcessRuns {
			w.fail("the read answered %q, want %q", got, ProcessRuns)
		}
		if c, stored := h.claim(), h.store.load(h.token); c.judgedDeath() || stored.judgedDeath() {
			w.fail("the claim reads a judged death %v (%v stored) after the read found the process running, want it over",
				c.judgedDeath(), stored.judgedDeath())
		}
		w.death, w.turnEnd, w.judged = false, turnEndWatch{}, w.judged+1
		return nil
	})
}

// lateAck is a step of an acknowledgement, after the end, of the prompt of the task the claim
// stood holding sent and not confirmed: one whose send was in flight when the end came, or the
// shim's second answer for one prompt (shimwire's Dedupe). It comes only on the daemon that read
// the end, which holds the process gone, so no wait for a turn is armed over it (acked,
// invariant 6).
func (w *outsideWalk) lateAck() {
	w.acks++
	w.step("a late acknowledgement of the task's prompt", func() error {
		h := w.h
		return h.handle(PromptAcked{Claim: h.token, Generation: w.generation, DeliveryID: h.claim().Pending.ID})
	})
}

// setFault is a step that sets one fault, at random, for the next writes: one of the store's
// four, or, one time in five, the lifecycle recheck's refusal of the relaunch, drawn from more.
func (w *outsideWalk) setFault() {
	faults := []string{faultTakeBackRefused, faultClaimRefused, faultUnknownLanded, faultUnknownLost}
	fault := faults[w.rng.IntN(len(faults))]
	if w.more.IntN(len(faults)+1) == 0 {
		fault = faultRelaunchRefused
	}
	w.setFaultTo(fault)
}

// setFaultTo is a step that sets fault for the next writes.
func (w *outsideWalk) setFaultTo(fault string) {
	w.step("a fault set: "+fault, func() error {
		w.fault = fault
		w.applyFault()
		return nil
	})
}

// clearFaults is a step after which the store answers every write again.
func (w *outsideWalk) clearFaults() {
	w.step("the faults cleared", func() error {
		w.fault = ""
		w.applyFault()
		return nil
	})
}

// applyFault makes the machine's store answer as the fault set says, the store of a machine a
// restart built included.
func (w *outsideWalk) applyFault() {
	h := w.h
	answerEveryWrite(h)
	switch w.fault {
	case faultTakeBackRefused:
		h.store.fail("PutDelivery", errStoreRefused)
	case faultClaimRefused:
		h.store.fail("PutClaim", errStoreRefused)
	case faultUnknownLanded, faultUnknownLost:
		h.m.deps.Store = &unknownOutcomeStore{memStore: h.store, landed: w.fault == faultUnknownLanded}
	case faultRelaunchRefused:
		h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	}
}

// restart is a step of a daemon restart: the machine is built from what the store holds, the
// fault set stands, and the boot owes the claim its replay where the store holds the decision of
// a live claim recording a process, as internal/daemon's recordedDeaths selects.
func (w *outsideWalk) restart() {
	w.step("a restart", func() error {
		h := w.h
		h.restart()
		// Another role's place in the first queue stands for the restart's wave, as in walk.
		firstAhead(h.deps.WakePace.Pacer, 1)
		w.applyFault()
		stored := h.store.load(h.token)
		if e := stored.EndedWork; !e.FromOutside && !e.Displaced || e.Generation != w.generation {
			w.lost = true
		}
		w.restarted = true
		w.turnEnd.restarted(h)
		// The restored claim holds the death or the displacement only where a write carried its
		// record or its decision into the store, which the step's check reads again
		// (deathStands, displacementStands).
		w.death, w.displacement = false, false
		w.replayOwed = stored.DeathCharged && stored.Locator != nil && slices.Contains(LiveStates(), stored.State)
		w.replayRead = stored.Generation
		return nil
	})
}

// replay is a step of the boot's replay the last restart owes, with the generation it read.
func (w *outsideWalk) replay() {
	w.replayOwed = false
	w.step("the boot's replay", func() error {
		return w.h.handle(RecordedDeath{Claim: w.h.token, Generation: w.replayRead})
	})
}

// relaunch is the close's relaunch: its turn, where it still waits for one, and its ready, at
// which the task is sent once, and nothing to a claim that held none (invariant 4). A relaunch
// charged no death waits in the second queue, where on the daemon that read the end the wake
// queueAhead parked waits ahead of it, and that wake's turn's jitter puts the relaunch's turn
// past the moment its view names: the clock moves on to that turn as the pacer gives it
// (advanceToTurn).
func (w *outsideWalk) relaunch() {
	h := w.h
	if c := h.claim(); c.Generation == w.generation {
		if waiting := h.m.View().WakeWaiting; waiting == nil || waiting.Kind != WakeLaunch {
			w.fail("the claim waits for %+v, want its relaunch waiting its turn", waiting)
		}
		w.step("the relaunch's turn", func() error { h.advanceToTurn(h.m); return nil })
	}
	// own is whether the record the relaunched role is told is the one the ended launch's own end
	// made: a record of an earlier launch still untold keeps its own words (leaveRecord).
	own := h.claim().EndedWork.Generation == w.generation
	w.step("the relaunch's ready", func() error {
		h.relaunched()
		return nil
	})
	prompts := h.prompts()[w.sent:]
	switch {
	case w.task == "" && len(prompts) != 0:
		w.fail("the relaunched role was sent %+v, want nothing: it held no task", prompts)
	case w.task != "" && (len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, w.task)):
		w.fail("the relaunched role was sent %+v, want its task %q once", prompts, w.task)
	}
	for _, p := range prompts {
		if w.relaunchedDeath && strings.Contains(p.Message, endFromOutside) {
			w.fail("the relaunched role was sent %q, want a death died judged never told as an end from outside the pod", p.Message)
		}
		if own && (w.relaunchedDisplacement || w.outside) && strings.Contains(p.Message, "the process died") {
			w.fail("the relaunched role was sent %q, want a displacement or an end decided from outside the pod never told as a death", p.Message)
		}
	}
}

// check holds the invariants after a step (TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders).
func (w *outsideWalk) check() {
	w.t.Helper()
	h := w.h
	c, stored := h.claim(), h.store.load(h.token)
	switch {
	case c.Budgets.PromptFailures != 0 || stored.Budgets.PromptFailures != 0:
		w.fail("the claim carries budgets %+v (%+v stored), want no prompt failure for a process reported gone",
			c.Budgets, stored.Budgets)
	case c.ProcessGone && h.armed(TimerTurn):
		w.fail("TimerTurn is armed over a process reported gone, want it disarmed")
	case !w.lost && !w.diedFirst && (c.Budgets != Budgets{} || stored.Budgets != Budgets{}):
		w.fail("the claim carries budgets %+v (%+v stored), want nothing charged for an end from outside the pod or a displacement that no restart lost the record of",
			c.Budgets, stored.Budgets)
	case !atMostDeaths(c.Budgets, w.judged) || !atMostDeaths(stored.Budgets, w.judged):
		w.fail("the claim carries budgets %+v (%+v stored), want at most %d deaths and a launch failure for each",
			c.Budgets, stored.Budgets, w.judged)
	}
	if c.Generation == w.generation {
		w.outside = c.decidedFromOutside()
	}
	if wrong := cmp.Or(notHeld(h, w.task), w.turnEnd.see(h), outsideEndQueued(h), w.deathStands(), w.displacementStands(),
		deathUnjudged(h, w.ended)); wrong != "" {
		w.fail("%s", wrong)
	}
}

// deathStands holds invariant 5's second half: once the claim holds a death of the ended launch
// that died judged (judgedDeath), deathRetyped holds while the claim records that launch, and no
// step logs a line of an end continued uncharged from outside the pod. The relaunch lets that
// process go, which ends the watch, a restart starts it again from the claim it restores, and a
// read that finds the process running ends it (readRunning).
func (w *outsideWalk) deathStands() string {
	held, lines := w.death, continuedLines(w.h)
	logged := lines > w.continued
	w.continued = lines
	switch c := w.h.claim(); {
	case c.Generation != w.generation:
		w.relaunchedDeath = w.relaunchedDeath || w.death
		w.death = false
	case c.judgedDeath():
		w.death = true
	}
	switch {
	case held && logged:
		return "the claim logged a line of an end from outside its pod continued charged nothing over the death died judged first, want none"
	case !w.death:
		return ""
	}
	return deathRetyped(w.h, w.generation)
}

// displacementStands holds invariant 5's sentence of a displacement: once the claim holds
// displaced's record of the ended launch (recordedDisplaced), displacementRejudged holds while
// the claim records that launch. The relaunch lets that process go, which ends the watch, and a
// restart starts it again from the claim it restores.
func (w *outsideWalk) displacementStands() string {
	switch c := w.h.claim(); {
	case c.Generation != w.generation:
		w.relaunchedDisplacement = w.relaunchedDisplacement || w.displacement
		w.displacement = false
	case c.recordedDisplaced():
		w.displacement = true
	}
	if !w.displacement {
		return ""
	}
	return displacementRejudged(w.h, w.generation)
}

// displacementRejudged answers how the claim no longer holds the displacement displaced judged
// first over the launch generation names as that displacement, and "" where it does or where the
// claim records another launch. The first judgement of a process stands, and a displacement is
// one: a later report read neither way goes to displaced as that displacement again (judge,
// recordedDisplaced), and one read as outside types its record as one from outside the pod and
// keeps displaced's type (markFromOutside). So no report of the process reaches died (diedLine),
// and the claim holds displaced's record of that launch, never a death died judged, with its
// paced wake, where it holds one, waiting in the pacing's second queue (WakeQueue), all read off
// the view the machine last published (publish). Both walks check it.
func displacementRejudged(h *harness, generation uint64) string {
	v := h.m.view.Load()
	switch c := v.Claim; {
	case c.Generation != generation:
	case len(h.logs.lines(diedLine, "claim="+string(h.token))) != 0:
		return "a report of the displaced process reached died, want each later report taken as that displacement"
	case !c.recordedDisplaced() || c.judgedDeath():
		return fmt.Sprintf("the claim reads deathCharged=%v over the record %+v, want displaced's record of the launch it judged first, never a death",
			c.DeathCharged, c.EndedWork)
	case v.place != nil && v.place.Queue() != wakepace.Second:
		return "the claim holds the displacement displaced judged first and its paced wake waits in the pacing's first queue, want the second: a displacement charges no death"
	}
	return ""
}

// atMostDeaths is whether b charges nothing but deaths and launch failures, n of each at most.
func atMostDeaths(b Budgets, n int) bool {
	return b == Budgets{LaunchFailures: b.LaunchFailures, Deaths: b.Deaths} && b.LaunchFailures <= n && b.Deaths <= n
}
