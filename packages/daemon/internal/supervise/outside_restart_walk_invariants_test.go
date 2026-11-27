package supervise

// The invariants the random walk of an end from outside the pod holds after every step
// (outside_restart_walk_test.go, TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders).

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// diedWords is how a death's cause opens where the runtime read no plain cause of it (endDied).
const diedWords = "the process died"

// firstJudgement is the cause of the end that a report of the claim's process, observation,
// judges first, as judge routes it: displaced's for the daemon's own replacement, died's over a
// death died judged, in that death's words where its record stands, the end from outside the
// pod's for a report read so or over that end's record, displaced's for one read neither way
// over displaced's record, and died's otherwise, in this death's words (endDied).
func firstJudgement(c Claim, observation runtime.Observation) string {
	e := c.EndedWork
	switch {
	case observation.Displaced:
		return endDisplaced
	case c.judgedDeath() && e.Died && e.Generation == c.Generation:
		return e.Cause
	case c.judgedDeath():
		return diedWords
	case observation.EndReading == runtime.PodEndNotOurs || c.recordedFromOutside():
		return endFromOutside
	case c.recordedDisplaced():
		return endDisplaced
	}
	return endDied(observation)
}

// storedJudgement is the cause of the end that judged the launch generation names as the claim c
// a restart restored holds it, and "" where it holds none: a record of that launch typed by its
// judgement, or the decision of a death that recorded none.
func storedJudgement(c Claim, generation uint64) string {
	e, ours := c.EndedWork, c.EndedWork.Generation == generation
	switch {
	case c.Generation != generation:
	case ours && e.Displaced:
		return endDisplaced
	case ours && e.FromOutside:
		return endFromOutside
	case ours && e.Died:
		return e.Cause
	case c.DeathCharged:
		return diedWords
	}
	return ""
}

// check holds the invariants after a step (TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders).
func (w *outsideWalk) check() {
	w.t.Helper()
	h := w.h
	c, stored := h.claim(), h.store.load(h.token)
	budgets, storedBudgets := beyond(c.Budgets, w.base), beyond(stored.Budgets, w.base)
	if w.reads > 0 || w.sweeps > 0 {
		budgets.PromptFailures, storedBudgets.PromptFailures = 0, 0
	}
	e, sent := c.EndedWork, len(h.prompts())
	// A suspension that stops the process, or a launch that replaces it, ends it as a report that
	// finds it gone does (invariant 9). No launch of the walk replaces a process a running verdict
	// found running before a step ended it: the relaunch of the death that verdict ended goes with
	// that death (dropOutlivedRelaunch), and left waiting it lets the live process go for that
	// death at its turn.
	if w.alive && !w.closing && c.Generation != w.generation {
		w.fail("a launch replaced the process a running verdict found running, want no launch to let it go for the death that verdict ended")
	}
	if c.State == StateSuspended || c.Generation != w.generation || c.Locator == nil {
		w.alive = false
	}
	if w.alive || w.closing {
		w.quietPrompts, w.quietFailures = sent, c.Budgets.PromptFailures
	}
	switch {
	case budgets.PromptFailures != 0 || storedBudgets.PromptFailures != 0:
		w.fail("the claim carries budgets %+v (%+v stored), want no prompt failure for a process reported gone",
			c.Budgets, stored.Budgets)
	case sent != w.quietPrompts:
		w.fail("the claim was sent %+v while no read found its process running, want nothing sent to a process reported gone",
			h.prompts()[w.quietPrompts:])
	case c.Budgets.PromptFailures != w.quietFailures:
		w.fail("the claim carries budgets %+v, want no prompt failure charged while no read found its process running", c.Budgets)
	case c.ProcessGone && h.armed(TimerTurn):
		w.fail("TimerTurn is armed over a process reported gone, want it disarmed")
	case !w.lost && !w.diedFirst && (budgets != Budgets{} || storedBudgets != Budgets{}):
		w.fail("the claim carries budgets %+v (%+v stored), want nothing charged for an end from outside the pod or a displacement that no restart lost the record of",
			c.Budgets, stored.Budgets)
	case !atMostDeaths(budgets, w.judged) || !atMostDeaths(storedBudgets, w.judged):
		w.fail("the claim carries budgets %+v (%+v stored), want at most %d deaths and a launch failure for each",
			c.Budgets, stored.Budgets, w.judged)
	case e.Tells() && (e.Turn || e.TaskTurn) && !w.cutAts[e.At.UnixNano()]:
		w.fail("the claim's record %+v tells a turn cut off, want none told where no end cut one off", e)
	case e.Generation == w.generation && w.judgedCause != "" && e.Cause != "" && e.Cause != w.judgedCause && e.Cause != w.earlierCause:
		w.fail("the claim's record %+v holds the cause %q, want the cause of the end that first judged the launch, %q",
			e, e.Cause, w.judgedCause)
	case c.Generation == w.generation && c.Locator != nil &&
		(w.judgedCause == endDisplaced && !c.recordedDisplaced() || w.judgedCause == endFromOutside && !c.recordedFromOutside()):
		w.fail("the claim's record %+v is not typed as the judgement that first judged the launch, %q", e, w.judgedCause)
	case w.judgedAfterRead && e.Generation == w.generation && e.Told && e.ToldBy == "":
		w.fail("the claim's record %+v of the end judged after a read is told by no delivery, want it told to the relaunched role", e)
	case w.alive && (e.Tells() && e.Generation == w.generation || stored.EndedWork.Tells() && stored.EndedWork.Generation == w.generation):
		w.fail("the claim's record %+v (%+v stored) tells while the process a read found running lives, want no end told that did not happen",
			e, stored.EndedWork)
	case e.Tells() && w.toldOutlived(e.Cause) != "" || stored.EndedWork.Tells() && w.toldOutlived(stored.EndedWork.Cause) != "":
		w.fail("the claim's record %+v (%+v stored) tells the death a running verdict ended, want no end told that did not happen",
			e, stored.EndedWork)
	}
	// The earlier launch's record is told where a confirmation told it, and untold again where a
	// let-go takes that telling back, so a death's record joins it and keeps it (EndedWork.Earlier,
	// untellInFlight). A record of another end that replaced it told leaves it told.
	switch {
	case w.earlierCause == "":
	case e.Cause == w.earlierCause:
		w.earlierTold = e.Told && e.ToldBy != ""
	case e.Earlier != nil && e.Earlier.Cause == w.earlierCause:
		w.earlierTold = false
	}
	prompts := h.prompts()
	for _, p := range prompts[min(w.seen, len(prompts)):] {
		// A prompt may tell the earlier launch's end, which is real, and no other. The task
		// the death took back goes behind that end's task instruction and the sent-again
		// sentence. The earlier end in this walk did not interrupt the task.
		earlier := w.earlierCause != "" && strings.Contains(p.Message, w.earlierCause) && !strings.Contains(p.Message, diedWords)
		behind := earlier && strings.HasSuffix(p.Message, "Then continue the task.\n\n"+sentAgainTask+owedTask)
		if w.alive && p.DeliveryID == w.sentAgain && behind {
			w.earlierSentAgain++
		}
		if cause := w.toldOutlived(p.Message); cause != "" {
			w.fail("the claim's process was sent %q, which tells %s, the death a running verdict ended: want no end told that did not happen",
				p.Message, cause)
		}
		switch {
		case !w.alive:
		case w.earlierCause != "" && !w.earlierTold && !earlier:
			w.fail("the process a read found running was sent %q, want the earlier launch's end it was never told told first: a false death erases nothing",
				p.Message)
		case tellsEnd(p.Message) && !earlier:
			w.fail("the process a read found running was sent %q, want no end told to a process that has not ended", p.Message)
		case p.DeliveryID == w.sentAgain && p.Message != sentAgainTask+owedTask && !behind:
			w.fail("the task the death took back went again to the process a read found running as %q, want it behind the one sentence of a task sent again",
				p.Message)
		case p.DeliveryID != w.sentAgain && strings.Contains(p.Message, sentAgainTask):
			w.fail("the process a read found running was sent %q, want the sentence of a task sent again in front of the task the death took back alone",
				p.Message)
		}
	}
	w.seen = len(prompts)
	if c.Generation == w.generation {
		w.outside = c.decidedFromOutside()
	}
	if wrong := cmp.Or(notHeld(h, w.task), w.turnEnd.see(h), outsideEndQueued(h), w.deathStands(), w.displacementStands(),
		deathUnjudged(h, w.ended)); wrong != "" {
		w.fail("%s", wrong)
	}
}

// toldOutlived is the cause of a death a running verdict ended (outsideWalk.outlived) that text
// tells, and "" where it tells none: that death never came, so nothing may tell it (invariant 9).
func (w *outsideWalk) toldOutlived(text string) string {
	for _, cause := range w.outlived {
		if strings.Contains(text, cause) {
			return cause
		}
	}
	return ""
}

// tellsEnd is whether a prompt tells its process that it ended, or that a turn of it was cut off:
// a record's sentence or a take-back's, or the note of a pod that ran out of memory.
func tellsEnd(message string) bool {
	return strings.Contains(message, "ended because") || strings.Contains(message, "interrupted") ||
		strings.Contains(message, outOfMemoryGuidance)
}

// beyond is b less base, each count apart: what the walk charged beyond the death of an earlier
// launch the claim stood over (standEarlier).
func beyond(b, base Budgets) Budgets {
	return Budgets{LaunchFailures: b.LaunchFailures - base.LaunchFailures, Deaths: b.Deaths - base.Deaths,
		PromptFailures: b.PromptFailures - base.PromptFailures, PromptRetires: b.PromptRetires - base.PromptRetires}
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
	case c.Locator == nil:
		// The claim records no process now, but a restart can restore one whose let-go the
		// store did not take, and the watch starts again from it.
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
	case c.Locator == nil:
		w.displacement = false
	case c.recordedDisplaced():
		if !w.displacement {
			w.dying = len(w.h.logs.lines(diedLine, "claim="+string(w.h.token)))
		}
		w.displacement = true
	}
	if !w.displacement {
		return ""
	}
	return displacementRejudged(w.h, w.generation, w.dying)
}

// displacementRejudged answers how the claim no longer holds the displacement displaced judged
// first over the launch generation names as that displacement, and "" where it does or where the
// claim records another launch. The first judgement of a process stands, and a displacement is
// one: a later report read neither way goes to displaced as that displacement again (judge,
// recordedDisplaced), and one read as outside types its record as one from outside the pod and
// keeps displaced's type (markFromOutside). So no report of the process reaches died (diedLine)
// after the dying lines it had written before the displacement, and the claim holds displaced's
// record of that launch, never a death died judged, with its paced wake, where it holds one,
// waiting in the pacing's second queue (WakeQueue), all read off the view the machine last
// published (publish). Both walks check it.
func displacementRejudged(h *harness, generation uint64, dying int) string {
	v := h.m.view.Load()
	switch c := v.Claim; {
	case c.Generation != generation || c.Locator == nil:
	case len(h.logs.lines(diedLine, "claim="+string(h.token))) > dying:
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
