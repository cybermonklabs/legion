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
// about two seeds of three a Gone report read as an end from outside the pod, and in the rest a
// report read neither way, which died decides as a death (diedFirst), and a second after it.
// Then come up to outsideSteps random steps, each one of a report read as outside, a report read
// neither way, an Uncertain report (a claim write), a fault set for the next writes (a write
// refused or of unknown outcome, or the relaunch refused by the lifecycle recheck), the faults
// cleared, a second passing, a daemon restart, and the boot's replay a restart owes where its
// store held the decision (internal/daemon's recordedDeaths). An event the machine answers with
// an error is allowed. The walk closes with the faults cleared, the replay still owed, one
// report read neither way, the relaunch's turn and its ready.
//
// After every step, with the seed and the steps walked in each failure:
//
//  1. Where the end's first report was read as outside and no restart came while the store held
//     no record of the ended launch typed as an end from outside the pod, the claim is charged
//     nothing, in memory or in the store; otherwise, at most one death and its launch failure.
//  2. The claim and the store hold the task, where the claim held one, at every step: it is
//     never retired.
//  3. Once the claim reads the end decided (Claim.DeathCharged), the end of the turn
//     (Claim.LastTurnEnd) does not move in it, and once the stored claim reads it decided, not in
//     the store either (turnEndWatch). A decision D1 made in memory and its store refused is gone
//     at a restart, and D2 makes it again, so the claim's watch starts again where the restored
//     claim is undecided; the store's never does.
//  4. At the relaunch's ready the task is sent once, never twice, and a claim that held nothing
//     is sent nothing.
//  5. While the claim holds its end decided from outside the pod, a paced wake the machine holds
//     at a place waits in the pacing's second queue: such an end charges no death, so its
//     relaunch is never placed as a charged death's (outsideEndQueued). And once the claim reads
//     its end decided as a death of its own process (chargedDeath), as died decides it, that
//     first decision stands for as long as the claim holds it over that process: the claim never
//     reads as a decided end from outside the pod, and a paced wake it holds waits in the first
//     queue (deathRetyped, outsideWalk.deathStands).
//
// The negative controls are the table test's: judge routing on decidedFromOutside fails
// invariant 1, a report read neither way over a record with no decision charged a death, on one
// daemon or after a restart that kept the record; endedFromOutside deciding again at every
// report fails invariant 3, D2's report before the replay over a stored decision moving the end
// of the turn after the decision; WakeQueue reading Claim.DeathCharged in place of
// Claim.chargedDeath fails invariant 5; and endedFromOutside typing the end as one from outside
// the pod where the claim held it decided on entry (markFromOutside) fails invariant 5's second
// half, a report read as outside over a death died recorded nothing of retyping that death.
func TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders(t *testing.T) {
	for seed := uint64(1); seed <= outsideSeeds; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			newOutsideWalk(t, seed).run()
		})
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
	h    *harness
	// ended and generation are the process the end ended and its launch, task the task the claim
	// held, "" for none, and sent how many prompts went out before the end.
	ended      runtime.Locator
	generation uint64
	task       string
	sent       int
	// walked is every step so far, in order, which a failure prints with the seed.
	walked []string
	// fault is the fault set for the next writes, "" for none.
	fault string
	// replayOwed is whether the last restart's boot owes the claim its replay, and replayRead the
	// generation that boot read.
	replayOwed bool
	replayRead uint64
	// lost is whether a restart came while the store held no record of the end as one from
	// outside the pod.
	lost bool
	// diedFirst is whether the end's first report was read neither way, so died decided the end
	// as a death.
	diedFirst bool
	// death is whether the claim holds its end decided as a death of its own process, the first
	// decision over the ended launch (invariant 5's second half, deathStands).
	death bool
	// turnEnd holds the end of the turn where the decision of the end put it (invariant 3).
	turnEnd turnEndWatch
}

func newOutsideWalk(t *testing.T, seed uint64) *outsideWalk {
	w := &outsideWalk{t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, 0)), more: rand.New(rand.NewPCG(seed, 1)),
		h: pacedHarness(t, wakeRate)}
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
		w.task = owedTask
		w.walked = append(w.walked, "stood ready holding its task sent and not confirmed")
	}
	queueAhead(h, 1)
	w.ended, w.generation, w.sent = h.locator(), h.generation(), len(h.prompts())
	second := func() { w.step("a second passed", func() error { h.advance(time.Second); return nil }) }
	// A fault before the end, so the end's own take-back and decision can fail as a later
	// report's can.
	if w.rng.IntN(2) == 0 {
		w.setFault()
	}
	if w.diedFirst {
		w.report("the end, read neither way", runtime.NotRecordedProcess, "")
		// The runtime reads the end as one from outside the pod only at a later event of the pod,
		// the one that carries DisruptionTarget, so the reports after this one come later.
		second()
	} else {
		w.report("the end, read as outside", runtime.Gone, runtime.PodEndNotOurs)
	}
	for range w.rng.IntN(outsideSteps + 1) {
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
	w.step(name, func() error {
		return w.h.handle(RuntimeObservation{Observation: runtime.Observation{
			Locator: w.ended, Kind: kind, At: w.h.clock.Now(), EndReading: reading,
		}})
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
		if !stored.EndedWork.FromOutside || stored.EndedWork.Generation != w.generation {
			w.lost = true
		}
		w.turnEnd.restarted(h)
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
}

// check holds the invariants after a step (TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders).
func (w *outsideWalk) check() {
	w.t.Helper()
	h := w.h
	c, stored := h.claim(), h.store.load(h.token)
	switch {
	case !w.lost && !w.diedFirst && (c.Budgets != Budgets{} || stored.Budgets != Budgets{}):
		w.fail("the claim carries budgets %+v (%+v stored), want nothing charged for an end from outside the pod that no restart lost the record of",
			c.Budgets, stored.Budgets)
	case !atMostOneDeath(c.Budgets) || !atMostOneDeath(stored.Budgets):
		w.fail("the claim carries budgets %+v (%+v stored), want at most the one death and its launch failure",
			c.Budgets, stored.Budgets)
	}
	if wrong := cmp.Or(notHeld(h, w.task), w.turnEnd.see(h), outsideEndQueued(h), w.deathStands()); wrong != "" {
		w.fail("%s", wrong)
	}
}

// deathStands holds invariant 5's second half: once the claim reads its end decided as a death
// of its own process (chargedDeath), deathRetyped holds while the claim records the ended launch
// and holds its end decided. A restart whose store held no decision forgets the death, and the
// relaunch lets that process go, so each ends the watch.
func (w *outsideWalk) deathStands() string {
	switch c := w.h.claim(); {
	case c.Generation != w.generation || !c.DeathCharged:
		w.death = false
	case c.chargedDeath():
		w.death = true
	}
	if !w.death {
		return ""
	}
	return deathRetyped(w.h, w.generation)
}

// atMostOneDeath is whether b charges nothing but one death and its launch failure at most.
func atMostOneDeath(b Budgets) bool {
	return b == Budgets{LaunchFailures: b.LaunchFailures, Deaths: b.Deaths} && b.LaunchFailures <= 1 && b.Deaths <= 1
}
