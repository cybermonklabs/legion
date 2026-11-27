package supervise

// The random walk of an end from outside the pod met again, beside the table of every order in
// outside_restart_test.go, whose harness of the end and its events it shares; the invariants it
// holds after every step are in outside_restart_walk_invariants_test.go.

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
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
// the claim in a turn of its confirmed task, ready holding its task sent and not confirmed, in
// about one seed of five of those idle after a turn that ran its task to its end, in about one of
// five relaunched over an earlier launch's death still untold, in a turn its agent was in when
// the task came or, as often, in the task's own turn (standEarlier), or, in about one seed of
// four, ready holding nothing, with no task and no turn, parks the relaunch behind another
// claim's wake, maybe sets a fault for the next writes, and sends the end: in about one seed of
// three a report read neither way, which died judges a death (diedFirst); in about one of the
// rest in four the daemon's own replacement of the pod, which displaced judges first
// (displacedFirst), its relaunch refused by the lifecycle recheck; a second after either; and
// otherwise a Gone report read as an end from outside the pod.
// Then come up to outsideSteps random steps, each one of a report read as outside, a report read
// neither way, an Uncertain report (a claim write), a fault set for the next writes (a write
// refused or of unknown outcome, or the relaunch refused by the lifecycle recheck), the faults
// cleared, a second passing, a daemon restart, the boot's replay a restart owes where its store
// held the decision (internal/daemon's recordedDeaths), and, while a read found the process
// running, the daemon's replacement of its pod. Each report read neither way carries a detail of
// its own, so each death's cause is its own. Before each step, drawn from streams of their own so
// the steps above stay the ones each seed walked before them, may come: the runtime's sweep
// reporting the process running (sweepRunning), as the reviewer's probe sent it, at any step of
// a walk whose first report was read neither way before any report read the pod ended, where
// the claim records the ended launch live and the store takes its writes, whatever waits or runs:
// a death's relaunch waiting in the pacing, any launch waiting, ready or idle, in a turn, after a
// restart; a read of the process at a caller's request that finds it running (ReadProcess), in
// such a walk, wherever no launch waits; either running verdict ends every death died judged it
// finds, as often as one comes, and gives back the Death and the launch failure that death
// charged (reportedProcess, EndedWork.DeathCounted), so no run of false deaths reaches the deaths
// limit (chargeDeath); on the daemon that read the end, where the claim stood holding its task
// sent and still holds it, a late acknowledgement of that task's
// prompt (acked), as the send in flight at the end gives one, or the shim's second answer for one
// prompt; the steps of a process a read found running (live): the end of its turn with an
// activity report that says nothing runs, the turn start of the task sent again, a suspension
// held for that turn, and an end of the process; and, once in a walk, an operator's suspension,
// held for the turn where the claim is in one over a process the daemon does not hold gone
// (holdSuspension), and landing where it arrives otherwise. An event the machine answers with an
// error is allowed. The walk closes with the faults cleared, the replay still owed, one report
// read neither way, the relaunch's turn, the operator's resume where the claim is suspended, and
// the relaunch's ready.
//
// After every step, with the seed and the steps walked in each failure:
//
//  1. Where the end's first report was read as outside, or was the daemon's own replacement of
//     the pod, and no restart came while the store held no record of the ended launch typed as
//     an end from outside the pod or as displaced's, the claim is charged nothing, in memory or
//     in the store; otherwise, at most one death and its launch failure, those of the one death
//     judged that no running verdict ended: a read that finds the process running ends a judged
//     death and gives back what it charged (reportedProcess), so the end a later report finds is
//     judged afresh, however many came before it. What the earlier launch's death charged before
//     the walk began is no part of either (outsideWalk.base).
//  2. The claim and the store hold the task, where the claim held one, at every step: it is
//     never retired, but by the end of a turn that confirmed it in a process a read found
//     running, which served it.
//  3. Once the claim reads the end decided (Claim.DeathCharged), the end of the turn
//     (Claim.LastTurnEnd) does not move in it, and once the stored claim reads it decided, not in
//     the store either (turnEndWatch). A decision D1 made in memory and its store refused is gone
//     at a restart, and D2 makes it again, so the claim's watch starts again where the restored
//     claim is undecided; the store's never does. A read that finds the process running clears
//     the decision, in the claim and in the store, so both watches start again there.
//  4. At the relaunch's ready the task is sent once, never twice, and a claim that held nothing
//     is sent nothing but the daemon's own delivery of a record that tells (queueRelaunch); an
//     end the claim held decided from outside the pod is never told as a death
//     (outsideWalk.relaunch). The walk's claim holds no record of a launch before the ended one
//     but the earlier launch's death it stood over, so every other record it tells is one an end
//     of the ended launch made.
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
//     (TimerTurn) is never armed while the machine holds the process gone (Claim.ProcessGone). A
//     process a read found running can let the wait for a turn of a prompt sent to it run out,
//     which charges a prompt failure as for any live process.
//  7. A record that tells a turn cut off (EndedWork.Turn, TaskTurn) is told only where an end cut
//     one off: it is a record an end of the process made — a report judged, a suspension landing,
//     the relaunch's turn — that came while the claim was in a turn or held its task confirmed, or
//     one joined over such a record while that record still told (outsideWalk.cutAts). A record
//     an end made over a turn that ended normally, or over a task a turn confirmed and ran to its
//     end, tells no turn cut off.
//  8. A record of the ended launch, told or not, holds the cause of the end that first judged that
//     launch: died's in that death's own words, displaced's or the end from outside the pod's, as
//     judge routes the first report it judges (firstJudgement), and the claim types it as that
//     judgement where displaced's or the end from outside the pod's judged first
//     (recordedDisplaced, recordedFromOutside). A read that finds the process running ends that
//     judgement, and a restart whose store kept no judgement of the launch forgets it, so the
//     next report judges afresh (storedJudgement); a judgement over the record of a death a read
//     ended leaves a record told by a delivery or still to tell, never told by none. An end that
//     ended nothing worth telling over the earlier launch's record still untold keeps that
//     record's words (leaveRecord), which are no cause of the ended launch.
//  9. No end is told to a process that has not ended. While a process a read found running
//     lives — no report has found it gone, no suspension stopped it and no launch replaced it —
//     the claim holds no record of the ended launch that tells, in memory or in the store, so no
//     prompt, relaunch task or catch-up tells it an end of its own; no prompt to it says that it
//     ended, that a turn of it was cut off or that its pod ran out of memory (tellsEnd); the task
//     the death took back goes to it again behind the one sentence of a task sent again
//     (sentAgainTask), and no other prompt carries that sentence. A false death erases nothing:
//     where the claim stood over the earlier launch's death still untold, every prompt to that
//     process tells that end in its own words, until a turn confirms one that told it, and tells
//     no other, and the sentence of a task sent again follows that end's words. The death a
//     running verdict ended, a read's or a sweep's, never came, so it is never told afterwards:
//     no record that tells holds its cause or its relaunch's, and no prompt, a relaunched role's
//     among them, says it (outsideWalk.outlived); and no launch replaces the live process before
//     a step has ended it, as the relaunch of that death would at its turn.
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
// check, the claim and the store still holding the death after it; judge leaving TimerTurn armed
// on a gone verdict, or acked arming it over a gone process, fails invariant 6; recordEnd
// recording every end as one that cut off the task's turn, or recording the turn a claim idle
// after a normal turn end ran as cut off (EndedWork.Turn of an idle state, seed 2 among others),
// fails invariant 7; and markFromOutside letting a later report read as outside give every
// standing record of the launch its own cause fails invariant 8 (seed 7 among others).
//
// Over the record of a death a running verdict ended: reportedProcess keeping the take-back's
// mark (Machine.cutTurnOf) fails invariant 7 (37 seeds, 48, 638 and 976 among them), and keeping
// the death's turn on the record fails it too (23 seeds, 9 and 48 among them); markDisplaced
// leaving that record untyped fails invariant 8 (seeds 48, 271, 341, 404, 457, 481, 504, 768,
// 960), and so does died typing it without its own cause (14 seeds, 9, 48 and 204 among them),
// markFromOutside typing it so (seeds 74, 151, 204, 206, 472, 754, 894), and retype leaving it
// told (14 seeds, 9 and 204 among them); reportedProcess leaving it untold fails invariant 9 (63
// seeds, 9 and 48 among them), and so does Delivery.message sending the task sent again with no
// sentence (41 seeds, 9 and 48 among them), and reportedProcess marking a record that joined the
// earlier launch's told whole rather than putting that one back (18 seeds, 115, 262 and 638
// among them; at 638 and 906 the task the death took back goes again with no word of that
// launch's end). judge leaving a death's relaunch waiting when the sweep finds the process
// running, its order before 4728740d, fails invariant 9 at the relaunch's turn, which records
// that death again and tells it (seeds 247, 306, 403, 483, 687, 767), or invariant 6 first, a
// report the waiting launch drops leaving the turn-start timer armed over the process it found
// gone (seeds 125, 130, 359, 372, 544, 917). The walk holds no project pause, so judge dropping
// only a relaunch the pacing holds walks here as the fix does; the rows of a relaunch the pause's
// gate holds catch it (relaunch_waits_test.go). reportedProcess keeping the Death a death counted
// fails invariant 1 where false deaths follow one another (seeds 125, 204, 325, 359, 415, 544,
// 771), and so does chargeDeath counting it without marking it (EndedWork.DeathCounted), on the
// same seeds; reportedProcess keeping the launch failure that death's relaunch charged fails it
// too (38 seeds, 9 and 48 among them).
//
// The test fails unless some seed walks each of the orders the walk was given for them: the
// running read, the late acknowledgement over a gone process, a report read neither way over
// displaced's record, an idle turn end, a turn start of the task sent again after a read, a
// suspension held for a turn, an operator's suspension that lands where it arrives, after a read,
// a displacement, a death and an end from outside the pod, a read over the earlier launch's death
// still untold, the task sent again behind that death's words, the sweep's report of the process
// running, and running verdicts ending as many deaths in one walk as the deaths limit, and
// unless some sweep report lands while a death's relaunch waits in the pacing, while any launch
// waits, ready or idle, in a turn and after a restart; it logs how many seeds walk each, how many
// end two deaths so, and how many sweep reports land at each of those points.
func TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders(t *testing.T) {
	var reads, acks, displacedReports, turnEnds, turnStarts, held, suspended, earlierReads, earlierSentAgain, sweeps int
	var twice, limit int
	var liveEnds [3]int
	var sweepsAt [5]int
	for seed := uint64(1); seed <= outsideSeeds; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			w := newOutsideWalk(t, seed)
			w.run()
			reads, acks = reads+min(w.reads, 1), acks+min(w.acks, 1)
			displacedReports += min(w.displacedReports, 1)
			turnEnds, turnStarts = turnEnds+min(w.turnEnds, 1), turnStarts+min(w.turnStarts, 1)
			held, suspended = held+min(w.held, 1), suspended+min(w.suspended, 1)
			earlierReads, earlierSentAgain = earlierReads+min(w.earlierReads, 1), earlierSentAgain+min(w.earlierSentAgain, 1)
			sweeps += min(w.sweeps, 1)
			twice += min(w.outlivedDeaths/2, 1)
			limit += min(w.outlivedDeaths/testLimits().LaunchFailures, 1)
			for kind, n := range w.liveEnds {
				liveEnds[kind] += min(n, 1)
			}
			for at, n := range w.sweepsAt {
				sweepsAt[at] += n
			}
		})
	}
	t.Logf("seeds that walk a running read: %d; a late acknowledgement over a gone process: %d; a report read neither way over displaced's record: %d; an idle turn end: %d; a turn start after a read: %d; a suspension held for a turn: %d; an operator's suspension landing: %d; after a read, a displacement: %d, a death: %d, an end from outside the pod: %d; a read over an earlier launch's end still untold: %d; the task sent again behind that end's words: %d; the sweep's report of the process running: %d",
		reads, acks, displacedReports, turnEnds, turnStarts, held, suspended, liveEnds[0], liveEnds[1], liveEnds[2], earlierReads, earlierSentAgain, sweeps)
	t.Logf("sweep reports of the process running while a death's relaunch waits in the pacing: %d; while any launch waits: %d; ready or idle: %d; in a turn: %d; after a restart: %d",
		sweepsAt[0], sweepsAt[1], sweepsAt[2], sweepsAt[3], sweepsAt[4])
	t.Logf("seeds whose running verdicts end two deaths or more: %d; as many as the deaths limit: %d", twice, limit)
	if !t.Failed() && (reads == 0 || acks == 0 || displacedReports == 0 || turnEnds == 0 || turnStarts == 0 || held == 0 ||
		suspended == 0 || slices.Contains(liveEnds[:], 0) || earlierReads == 0 || earlierSentAgain == 0 || sweeps == 0 ||
		limit == 0 || slices.Contains(sweepsAt[:], 0)) {
		t.Fatal("a seed list that walks none of one of the orders, want each walked")
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
	// kinds is a fifth stream from the seed, of whether an idle turn end or an operator's
	// suspension comes before a random step where one can (idleTurnEnd, suspend), drawn apart for
	// the reason more is.
	kinds *rand.Rand
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
	// reads, acks and displacedReports are how many running reads, late acknowledgements, and
	// reports read neither way over displaced's record the walk took.
	reads, acks, displacedReports int
	// alive is whether a read found the process running and no report has found it gone since,
	// and podEnded whether a report read the pod ended, from outside it or by the daemon's
	// replacement, after which no read finds it running.
	alive, podEnded bool
	// turnEnds, turnStarts, held and suspended are how many idle turn ends, turn starts after a
	// read, suspensions held for a turn and suspensions landing where they arrived the walk took,
	// suspensions how many suspensions it drew, and liveEnds how many displacements, deaths and
	// ends from outside the pod it drew of a process a read found running (liveEnd).
	turnEnds, turnStarts, held, suspended, suspensions int
	liveEnds                                           [3]int
	// life is a seventh stream from the seed, of the steps of a process a read found running
	// (live), drawn apart for the reason more is.
	life *rand.Rand
	// ending is whether the step running is an end of the process (outsideWalk.step), cutAts the
	// moments (EndedWork.At) of the records that may tell a turn cut off (invariant 7),
	// judgedCause the cause of the end that first judged the ended launch, "" while none stands,
	// readEnded whether the last read ended a death the claim held the record of, and
	// judgedAfterRead whether the judgement came over that record (invariant 8).
	ending                     bool
	cutAts                     map[int64]bool
	judgedCause                string
	readEnded, judgedAfterRead bool
	// cutSince is whether an end has cut off a turn since the walk began, or since the last read
	// that found the process running, which ends the death that read found and every turn that
	// death named (reportedProcess): a role is told a turn was interrupted only after such an end
	// (outsideWalk.relaunch).
	cutSince bool
	// quietPrompts and quietFailures are how many prompts the claim had been sent and how many
	// prompt failures it carried when the walk last held its process not running, which no step
	// adds to until a read finds it running again (invariant 6); closing is whether the relaunch's
	// ready has begun, which sends the relaunched process its task.
	quietPrompts, quietFailures int
	closing                     bool
	// dying is how many lines died had written for the claim when its displacement watch began
	// (displacementStands): a death died judged before a read found the process running wrote
	// them, and no report after the displacement may add one.
	dying int
	// path is the path a process a read found running takes (live), 0 until it is drawn.
	path int
	// details is how many reports read neither way the walk has sent, each with a detail of its
	// own, so each death's cause is its own (invariant 8). takenBack is the id the last take-back
	// that landed gave the claim's task, sentAgain that id where a read found the process running
	// over it, which goes to the process again (invariant 9), and seen how many prompts the claim
	// had been sent at the last check.
	details              int
	takenBack, sentAgain string
	seen                 int
	// earlierCause is the cause of the death of a launch before the ended one, where the claim
	// stood relaunched over it with its record still untold (standEarlier), "" otherwise;
	// earlierTold is whether a confirmation told that record and no let-go has taken that
	// telling back since; base is what that death charged before the walk began (invariant 1);
	// earlierReads how many running verdicts, a read's or a sweep's, the walk took over it while
	// it was untold; and earlierSentAgain how many prompts sent the task again behind its words
	// (invariant 9).
	earlierCause     string
	earlierTold      bool
	base             Budgets
	earlierReads     int
	earlierSentAgain int
	// turnEnd holds the end of the turn where the decision of the end put it (invariant 3).
	turnEnd turnEndWatch
	// sweep is a ninth stream from the seed, of whether the runtime's sweep reports the ended
	// process running before a random step where it can (sweepRunning), drawn apart for the
	// reason more is. sweeps is how many such reports the machine judged, and sweepsAt how many
	// came while a death's relaunch waited in the pacing, while any launch waited, ready or idle,
	// in a turn, and after a restart. outlivedDeaths is how many running verdicts, a read's or a
	// sweep's, ended a death died judged, as many as come: each gives back the Death that death
	// counted (EndedWork.DeathCounted), so no run of them takes the claim to the deaths limit
	// (chargeDeath). outlived is the causes of those deaths, the record's and the relaunch's,
	// which nothing may tell again (invariant 9).
	sweep          *rand.Rand
	sweeps         int
	sweepsAt       [5]int
	outlivedDeaths int
	outlived       []string
}

func newOutsideWalk(t *testing.T, seed uint64) *outsideWalk {
	w := &outsideWalk{t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, 0)), more: rand.New(rand.NewPCG(seed, 1)),
		extra: rand.New(rand.NewPCG(seed, 3)), kinds: rand.New(rand.NewPCG(seed, 4)), life: rand.New(rand.NewPCG(seed, 6)),
		sweep: rand.New(rand.NewPCG(seed, 8)), h: pacedHarness(t, wakeRate), cutAts: map[int64]bool{}}
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
	// idle, drawn from a sixth stream for the reason more is drawn apart, stands one claim in
	// five of the rest idle after a turn that ran its task to its end, so an end comes over a
	// turn that ended normally and that no end cut off (invariant 7).
	idle := !nothing && rand.New(rand.NewPCG(w.seed, 5)).IntN(5) == 0
	// earlier, drawn from an eighth stream, stands one claim in five of the rest relaunched over
	// an earlier launch's death still untold, in the turn that stream draws next (standEarlier).
	stand := rand.New(rand.NewPCG(w.seed, 7))
	earlier := !nothing && stand.IntN(5) == 0
	switch {
	case nothing:
		h.reach(StateReady)
		w.walked = append(w.walked, "stood ready holding nothing")
	case earlier:
		w.standEarlier(stand.IntN(2) == 0)
	case idle:
		h.reach(StateIdle)
		w.walked = append(w.walked, "stood idle after a turn that ran its task to its end")
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
	w.quietPrompts = w.sent
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
		w.observe("the end, the daemon's replacement of the pod", runtime.Observation{Kind: runtime.Gone, Displaced: true})
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
		if w.alive {
			steps = append(steps, w.displace)
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

// standEarlier stands the claim relaunched over a death of its earlier launch that no prompt has
// told yet: that launch, ready holding its task sent and not confirmed, dies of its pod's memory
// limit, so its record tells in that death's plain words (endDied), and the relaunch's ready
// sends the task behind that record's sentence. Where own is false, the agent answers that prompt
// busy, in a turn of its own (recordBusyTurn), so the task is never confirmed and no end takes it
// back. Where own is true, the turn is the task's own, its prompt's result still to come: its
// confirmation tells the record, and an end takes that telling back with the task
// (untellInFlight, interrupted), so a false death's take-back sends the task again behind that
// record's sentence. What that death charged is the walk's base (invariant 1).
func (w *outsideWalk) standEarlier(own bool) {
	h := w.h
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: owedTask})
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
		Detail: "the pod's roles container was OOMKilled", Cause: runtime.DeathCause{Kind: runtime.DeathMemoryLimit, Limit: 8 << 30}}})
	w.earlierCause = h.claim().EndedWork.Cause
	if h.m.View().WakeWaiting != nil {
		h.advanceToTurn(h.m)
	}
	sent := len(h.prompts())
	h.relaunched()
	ready := h.wantPrompts(sent + 1)[sent]
	stood := "in its task's own turn, its prompt's result still to come"
	if !own {
		h.must(StreamPromptResult{Claim: h.token, DeliveryID: ready.DeliveryID, AgentInvoked: false, Error: busyAnswer})
		stood = "in a turn its agent was in when its task came"
	}
	h.must(StreamTurnStart{Claim: h.token})
	w.task, w.base = owedTask, h.claim().Budgets
	w.walked = append(w.walked, "stood relaunched over an earlier death still untold, "+stood)
}

// step runs one step, notes it, and checks the invariants after it. Its error is allowed: a
// write the store refused or whose outcome is unknown fails the event that made it. A step that
// is an end of the process (ending) and comes while the claim records the ended launch in a turn,
// or holding its task confirmed, cuts that turn off, so the record of the launch it leaves may
// tell a turn cut off; so may a record joined over such a record while that one still told
// (invariant 7).
func (w *outsideWalk) step(name string, run func() error) {
	w.t.Helper()
	pre := w.h.claim()
	if err := run(); err != nil {
		name += " (answered: " + err.Error() + ")"
	}
	post := w.h.claim()
	before, after := pre.EndedWork, post.EndedWork
	if after.Cause != "" && after.Generation == w.generation {
		cut := w.ending && pre.Generation == w.generation && pre.Locator != nil &&
			(pre.State == StateWorking || pre.Pending != nil && !pre.Pending.ConfirmedAt.IsZero())
		joined := !after.At.Equal(before.At) && before.Tells() && (before.Turn || before.TaskTurn) &&
			w.cutAts[before.At.UnixNano()]
		if cut || joined {
			w.cutAts[after.At.UnixNano()] = true
		}
		w.cutSince = w.cutSince || cut
	}
	if p, q := pre.Pending, post.Pending; p != nil && q != nil && p.ID != q.ID && !p.ConfirmedAt.IsZero() && q.ConfirmedAt.IsZero() {
		w.takenBack = q.ID
	}
	w.ending = false
	w.walked = append(w.walked, name)
	w.check()
}

// report is a step of the runtime's report of the ended process, of kind read as reading.
func (w *outsideWalk) report(name string, kind runtime.ObservationKind, reading runtime.PodEndReading) {
	w.observe(name, runtime.Observation{Kind: kind, EndReading: reading})
}

// observe is a step of the runtime's report of the ended process, observation's locator and
// moment its own, and a detail of its own where it is read neither way. A report that finds the
// process gone is an end of it, which judges the launch first where no judgement of it stands
// and the machine judges the report (judge: the claim records the launch's process and no launch
// waits), under the cause firstJudgement gives.
func (w *outsideWalk) observe(name string, observation runtime.Observation) {
	h := w.h
	c := h.claim()
	if observation.Kind == runtime.NotRecordedProcess && c.Generation == w.generation && c.recordedDisplaced() {
		w.displacedReports++
	}
	if gone := observation.Kind == runtime.Gone || observation.Kind == runtime.NotRecordedProcess; gone {
		if !observation.Displaced && observation.EndReading == "" {
			w.details++
			observation.Detail = fmt.Sprintf("report %d", w.details)
		}
		judged := c.Generation == w.generation && c.Locator != nil && slices.Contains(LiveStates(), c.State) &&
			h.m.View().WakeWaiting == nil
		if judged && w.judgedCause == "" {
			w.judgedCause, w.judgedAfterRead = firstJudgement(c, observation), w.readEnded
		}
		w.ending, w.alive = true, false
		w.podEnded = w.podEnded || observation.Displaced || observation.EndReading == runtime.PodEndNotOurs
	}
	observation.Locator, observation.At = w.ended, h.clock.Now()
	w.step(name, func() error { return h.handle(RuntimeObservation{Observation: observation}) })
}

// beside is the steps drawn before a random step, where each can come: from sweep, one time in
// two, the runtime's sweep reporting the process running (sweepRunning), in a walk whose first
// report was read neither way before any report read the pod ended, where the claim records the
// ended launch live and the store takes every write but the relaunch's recheck; from extra, one
// time in two, a running read (readRunning), in such a walk, where no launch waits, and, one time
// in three, a late acknowledgement of the prompt of the task the claim stood owing (lateAck); the
// steps of a process a running verdict found running (live); and from kinds, one time in two an
// idle turn end where such a verdict found the process running and the claim is in a turn
// (idleTurnEnd), and one time in eight an operator's suspension, once in a walk (suspend). A
// running verdict ends every death died judged it finds, however many came before it
// (outlivedDeaths), and gives back the Death that death counted, so a later death is judged
// under the deaths limit as the first was (chargeDeath) and relaunched.
func (w *outsideWalk) beside() {
	c := w.h.claim()
	ours := c.Generation == w.generation && c.Locator != nil
	writes := w.fault == "" || w.fault == faultRelaunchRefused
	if ours && w.diedFirst && !w.podEnded && writes && slices.Contains(LiveStates(), c.State) && w.sweep.IntN(2) == 0 {
		w.sweepRunning()
	}
	if c = w.h.claim(); ours && w.diedFirst && !w.podEnded && w.h.m.View().WakeWaiting == nil && writes &&
		w.extra.IntN(2) == 0 {
		w.readRunning()
	}
	if c = w.h.claim(); ours && w.owing && !w.restarted && c.Pending != nil && c.Pending.ConfirmedAt.IsZero() &&
		w.extra.IntN(3) == 0 {
		w.lateAck()
	}
	if c = w.h.claim(); w.alive && c.Generation == w.generation && c.Locator != nil {
		w.live()
	}
	if c = w.h.claim(); w.alive && c.Generation == w.generation && c.State == StateWorking && w.kinds.IntN(2) == 0 {
		w.idleTurnEnd()
	}
	if c = w.h.claim(); w.suspensions == 0 && c.Generation == w.generation && slices.Contains(LiveStates(), c.State) &&
		w.kinds.IntN(8) == 0 {
		w.suspend()
	}
}

// live is the steps of a process a running verdict, a read's or a sweep's, found running, in a
// row, on one of two paths drawn from life at the first of them (path). On the first, the end of
// the turn the claim is in, with an activity report that says nothing runs (idleTurnEnd), at
// whose end the task the death took back goes to the process again; the turn start of that task
// (turnStart); an operator's suspension, held for that turn (suspend); and the end of that turn,
// which runs the suspension. On the second, the end of the turn the claim is in, and then, one
// time in two while the claim is in no turn, an end of the process (liveEnd). So a running
// verdict is followed by the orders the reviews found by hand: a held suspension at the end of
// the turn of the task a death took back and the process ran on, and a displacement, a death or
// an end from outside the pod after a turn that ended normally, with the record the verdict left:
// the cleared death's marked told by no delivery, which no prompt may tell, or the earlier
// launch's record still untold that the verdict put back in its place, which every prompt tells
// until a turn confirms one (invariant 9).
func (w *outsideWalk) live() {
	if w.path == 0 {
		w.path = 1 + w.life.IntN(2)
	}
	for range 6 {
		c := w.h.claim()
		if !w.alive || c.Generation != w.generation || c.Locator == nil {
			return
		}
		switch {
		case c.State == StateWorking && (w.path == 2 || w.turnStarts == 0 || w.held > 0):
			w.idleTurnEnd()
		case w.path == 1 && c.State != StateWorking && w.turnStarts == 0 && c.Pending != nil &&
			c.Pending.ConfirmedAt.IsZero() && slices.Contains(w.h.outstanding(), c.Pending.ID):
			w.turnStart()
		case w.path == 1 && c.State == StateWorking && w.suspensions == 0:
			w.suspend()
		case w.path == 2 && c.State != StateWorking && w.life.IntN(2) == 0:
			w.liveEnd()
		default:
			return
		}
	}
}

// turnStart is a step of the start of the turn of the task the claim sent again to a process a
// read found running, which confirms the task and tells the record its prompt carried
// (confirmCarrying).
func (w *outsideWalk) turnStart() {
	w.turnStarts++
	w.step("a turn start of the task sent again", func() error {
		return w.h.handle(StreamTurnStart{Claim: w.h.token})
	})
}

// liveEnd is a step of an end of a process a read found running, drawn from life: the daemon's
// replacement of its pod (displace), a death read neither way, or an end from outside the pod.
func (w *outsideWalk) liveEnd() {
	switch w.life.IntN(3) {
	case 0:
		w.displace()
	case 1:
		w.liveEnds[1]++
		w.observe("a death of the process a read found running", runtime.Observation{Kind: runtime.Gone})
	default:
		w.liveEnds[2]++
		w.observe("an end from outside the pod of the process a read found running",
			runtime.Observation{Kind: runtime.Gone, EndReading: runtime.PodEndNotOurs})
	}
}

// displace is a step of the daemon's replacement of the pod of a process a read found running,
// drawn after a turn that ended normally (liveEnd) or among the random steps (run).
func (w *outsideWalk) displace() {
	w.liveEnds[0]++
	w.observe("the daemon's replacement of the pod of the process a read found running",
		runtime.Observation{Kind: runtime.Gone, Displaced: true})
}

// idleTurnEnd is a step of the end of the turn the claim is in, in a process a read found running,
// with the process's activity report saying nothing runs: a turn that confirmed the task served
// it, and the claim no longer holds it (invariant 2).
func (w *outsideWalk) idleTurnEnd() {
	w.turnEnds++
	w.step("an idle turn end, its activity report saying nothing runs", func() error {
		h := w.h
		before := h.claim().Pending
		h.reportActivity()
		err := h.handle(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; before != nil && !before.ConfirmedAt.IsZero() && (p == nil || p.Task != w.task) {
			w.task = ""
		}
		return err
	})
}

// suspend is a step of an operator's suspension of the claim: held for the turn it is in where
// its process is not one the daemon holds gone (holdSuspension), and an end of the process that
// lands where it arrives otherwise.
func (w *outsideWalk) suspend() {
	w.suspensions++
	w.ending = true
	w.step("an operator's suspension", func() error {
		h := w.h
		err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		switch {
		case errors.Is(err, ErrSuspendHeld):
			w.held++
			w.ending = false
			return nil
		case h.claim().State == StateSuspended:
			w.suspended++
			w.alive = false
		}
		return err
	})
}

// readRunning is a step of a read of the process at a caller's request (ReadProcess) that finds
// it running, with no launch waiting, which a read would not make (ProcessReplacedByLaunch), and
// with the store taking the claim's write. A death it finds judged is over (reportedProcess): the
// claim reads no judged death and no decision, in memory and in the store, and the process lives
// on until a report finds it gone (outsideWalk.alive), so the watches of that judgement and of
// the end of the turn its decision put start again (invariants 3 and 5), and the next death
// judged of the process is one of its own (invariant 1).
func (w *outsideWalk) readRunning() {
	w.reads++
	if w.earlierCause != "" && !w.earlierTold {
		w.earlierReads++
	}
	w.step("a read at a caller's request, finding the process running", func() error {
		h := w.h
		pre := h.claim()
		h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Alive})
		if got := h.m.ReadProcess(h.ctx); got != ProcessRuns {
			w.fail("the read answered %q, want %q", got, ProcessRuns)
		}
		w.outlive(pre, "the read")
		return nil
	})
}

// outlive is the bookkeeping of a running verdict, a read's or a sweep's, that found the process
// running over pre, the claim as it stood before the verdict. A death it finds judged is over
// (reportedProcess), so a death of its own may come after it (invariant 1), and its causes, the
// record's and the one its relaunch carries (Claim.LastDeathCause, endRecordedDeath), are never
// told again (invariant 9). The verdict ends the death and every turn that death named, so a
// record it leaves tells no turn cut off, and only an end after it may say one was (invariant 7).
func (w *outsideWalk) outlive(pre Claim, verdict string) {
	h := w.h
	if c, stored := h.claim(), h.store.load(h.token); c.judgedDeath() || stored.judgedDeath() {
		w.fail("the claim reads a judged death %v (%v stored) after %s found the process running, want it over",
			c.judgedDeath(), stored.judgedDeath(), verdict)
	}
	if pre.DeathCharged || pre.judgedDeath() {
		w.outlivedDeaths++
		w.outlived = append(w.outlived, endRecordedDeath(pre.LastDeathCause))
		if e := pre.EndedWork; e.Died && e.Generation == w.generation {
			w.outlived = append(w.outlived, e.Cause)
		}
	}
	w.death, w.turnEnd, w.cutAts, w.cutSince = false, turnEndWatch{}, map[int64]bool{}, false
	w.alive, w.judgedCause, w.judgedAfterRead, w.sentAgain = true, "", false, ""
	// A record of an earlier launch the verdict put back in place of the death's is no record of
	// this launch's end, and a later end of it is a first end (invariant 8).
	w.readEnded = pre.EndedWork.Died && pre.EndedWork.Generation == w.generation &&
		h.claim().EndedWork.Generation == w.generation
	// The task that death took back goes to the process again (invariant 9).
	if p := pre.Pending; p != nil && p.ID == w.takenBack {
		w.sentAgain = p.ID
	}
}

// sweepRunning is a step of the runtime's sweep reporting the ended process running (observe,
// judge), as the reviewer's probe sends it, at any point of a walk where it can come: while a
// death's relaunch waits for its turn in the pacing, or any launch waits, where a read cannot
// come (ProcessReplacedByLaunch), and ready or idle, in a turn, and after a restart, where a read
// can come too. The agent answers the sweep's question of its turn as the claim stands, in a turn
// where the claim is working. A death the sweep finds judged is over, as a read ends it
// (reportedProcess), and so is its waiting relaunch (dropOutlivedRelaunch), so the step keeps the
// read's bookkeeping for it (outlive); over no death judged it says only that the process runs,
// and the walk holds it alive. Left waiting, as judge left it before 4728740d, the relaunch
// replaces the live process at its turn and tells the relaunched role the death the sweep ended
// (invariant 9).
func (w *outsideWalk) sweepRunning() {
	h := w.h
	pre, waiting := h.claim(), h.m.View().WakeWaiting != nil
	w.sweeps++
	for at, landed := range []bool{waiting && pre.chargedDeath(), waiting, pre.State != StateWorking,
		pre.State == StateWorking, w.restarted} {
		if landed {
			w.sweepsAt[at]++
		}
	}
	ends := pre.DeathCharged || pre.judgedDeath()
	if ends && w.earlierCause != "" && !w.earlierTold {
		w.earlierReads++
	}
	w.step("the sweep's report of the process, finding it running", func() error {
		h.conn.SetStreaming(pre.State == StateWorking)
		err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: w.ended, Kind: runtime.Alive, At: h.clock.Now()}})
		h.conn.SetStreaming(false)
		if ends {
			w.outlive(pre, "the sweep")
		}
		w.alive = true
		return err
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
		w.judgedCause = storedJudgement(stored, w.generation)
		w.judgedAfterRead = w.judgedAfterRead && w.judgedCause != ""
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
	if c := h.claim(); c.Generation == w.generation && c.State != StateSuspended {
		if waiting := h.m.View().WakeWaiting; waiting == nil || waiting.Kind != WakeLaunch {
			w.fail("the claim waits for %+v, want its relaunch waiting its turn", waiting)
		}
		w.ending = true
		w.step("the relaunch's turn", func() error { h.advanceToTurn(h.m); return nil })
	}
	if h.claim().State == StateSuspended {
		w.step("the operator's resume", func() error {
			return h.handle(RequestResume{Claim: h.token, By: SuspendedByOperator})
		})
		if h.m.View().WakeWaiting != nil {
			w.step("the resumed launch's turn", func() error { h.advanceToTurn(h.m); return nil })
		}
	}
	w.closing = true
	before := len(h.prompts())
	w.step("the relaunch's ready", func() error {
		h.relaunched()
		return nil
	})
	prompts := h.prompts()[before:]
	switch {
	case w.task == "" && (len(prompts) > 1 || len(prompts) == 1 && !strings.Contains(prompts[0].Message, "ended because")):
		w.fail("the relaunched role was sent %+v, want nothing but the daemon's own delivery of a record that tells: it held no task", prompts)
	case w.task != "" && (len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, w.task)):
		w.fail("the relaunched role was sent %+v, want its task %q once", prompts, w.task)
	}
	for _, p := range prompts {
		if w.relaunchedDeath && strings.Contains(p.Message, endFromOutside) {
			w.fail("the relaunched role was sent %q, want a death died judged never told as an end from outside the pod", p.Message)
		}
		if (w.relaunchedDisplacement || w.outside) && strings.Contains(p.Message, diedWords) {
			w.fail("the relaunched role was sent %q, want a displacement or an end decided from outside the pod never told as a death", p.Message)
		}
		if !w.cutSince && strings.Contains(p.Message, "interrupted") {
			w.fail("the relaunched role was sent %q, want no turn told interrupted where no end cut one off since the last read", p.Message)
		}
	}
}
