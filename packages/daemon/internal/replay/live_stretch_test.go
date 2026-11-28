package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// skippedLoopContinuationWindow is a window from 09:00 to 14:00 recorded under legion-v20.42.5 with
// a cap of 20, its inputs reaching back to the daemon's boot at 06:00, and no boot inside the
// window. Eight roots work 5 minutes in planning and in implementing and enter testing at 09:15.
// ACME-1 to ACME-4 end their first stretch in a recorded testing conflict at 09:40, return to
// testing at 09:50, pass to reviewing at 10:00, are rejected by review at 10:20, and return to
// testing at 10:30, where they stand to the window's end. ACME-5 to ACME-8 fail testing at 09:40,
// with no conflict, and return to testing at 09:50, where they stand to the window's end. The
// window holds 12 stretches, 4 testing conflicts, 4 other testing stretches and 4 reviewing
// stretches with no conflict; no phase takes a term of its own, so every stretch draws at 1/3.
func skippedLoopContinuationWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(14, 0))
	b.reaches(3 * time.Hour)
	b.capped(20)
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.boot(clock(6, 0))
	for i := 1; i <= 4; i++ {
		key := fmt.Sprintf("ACME-%d", i)
		rootThrough(b, key, fmt.Sprintf("a%02d", i), clock(9, 0), clock(9, 5),
			moveAt(9, 10, phase.Implementing, workflow.TriggerPlannerCompleted),
			moveAt(9, 15, phase.Testing, workflow.TriggerImplementationReady),
			moveAt(9, 40, phase.Implementing, workflow.TriggerConflict),
			moveAt(9, 50, phase.Testing, workflow.TriggerImplementationReady),
			moveAt(10, 0, phase.Reviewing, workflow.TriggerTesterPassed),
			moveAt(10, 20, phase.Implementing, workflow.TriggerReviewRejected),
			moveAt(10, 30, phase.Testing, workflow.TriggerImplementationReady))
	}
	for i := 5; i <= 8; i++ {
		key := fmt.Sprintf("ACME-%d", i)
		rootThrough(b, key, fmt.Sprintf("a%02d", i), clock(9, 0), clock(9, 5),
			moveAt(9, 10, phase.Implementing, workflow.TriggerPlannerCompleted),
			moveAt(9, 15, phase.Testing, workflow.TriggerImplementationReady),
			moveAt(9, 40, phase.Implementing, workflow.TriggerTesterFailed),
			moveAt(9, 50, phase.Testing, workflow.TriggerImplementationReady))
	}
	return b.build()
}

// A stretch that draws no conflict at its recorded conflict end goes on past the recorded loop
// (stretchEnds) in the same live stretch (liveStretch), which draws no more, also where it ends
// later in another phase. The test replays skippedLoopContinuationWindow under legion-v20.42.5 at
// the seeds 1 to 6: each of ACME-1 to ACME-4 whose first draw, in testing at 09:40, makes no
// conflict goes on through reviewing to its review's rejection and draws once in the run. It fails
// where that stretch draws again at the rejection, and where no stretch of ACME-1 to ACME-4 draws
// no conflict at 09:40 at any of the six seeds.
func TestAStretchPastASkippedLoopDoesNotDrawAgainAtALaterPhase(t *testing.T) {
	t.Parallel()
	window := skippedLoopContinuationWindow(t)
	skipped := 0
	for seed := int64(1); seed <= 6; seed++ {
		var draws []conflictDraw
		if _, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, "legion-v20.42.5"), Branch: drawnConflictBranch,
			Seed: seed, Slowdown: 1, PGDSN: replayDSN(t), Out: t.TempDir(),
			onDraw: func(draw conflictDraw) { draws = append(draws, draw) }}); err != nil {
			t.Fatal(err)
		}
		byIssue := map[string][]conflictDraw{}
		for _, draw := range draws {
			byIssue[draw.at.issue] = append(byIssue[draw.at.issue], draw)
		}
		for i := 1; i <= 4; i++ {
			key := fmt.Sprintf("ACME-%d", i)
			got := byIssue[key]
			if len(got) == 0 || got[0].conflicted {
				continue
			}
			skipped++
			if len(got) != 1 {
				t.Errorf("at seed %d %s skipped its testing conflict and still drew %d times (%+v), want 1", seed, key, len(got), got)
			}
		}
	}
	if skipped == 0 {
		t.Fatal("no ACME-1..4 stretch skipped its testing conflict across seeds 1..6")
	}
}

// A stretch whose conflict the daemon answers with a merge forward keeps its phase and goes on past
// its recorded loop in the same live stretch (mergedForward, stretchEnds), which draws no more. The
// test replays skippedLoopContinuationWindow under main, whose conflicting head in testing keeps its
// phase while the implementer merges the base forward, at the seeds 1 to 6: each of ACME-1 to
// ACME-4 draws once in the run, whichever its first draw made, and some conflict among them is
// merged forward. It fails where a stretch merged forward past its loop draws again at its review's
// rejection, and where no stretch of ACME-1 to ACME-4 is merged forward at any of the six seeds.
func TestAStretchMergedForwardPastARecordedLoopDrawsOnce(t *testing.T) {
	t.Parallel()
	window := skippedLoopContinuationWindow(t)
	mergedForward := 0
	for seed := int64(1); seed <= 6; seed++ {
		draws, out := drawsOfRun(t, window, "main", drawnConflictBranch, seed)
		log := replayLog(t, out)
		for i := 1; i <= 4; i++ {
			key := fmt.Sprintf("ACME-%d", i)
			if got := drawsOf(draws, key); len(got) != 1 {
				t.Errorf("at seed %d %s drew %d times (%+v), want once", seed, key, len(got), got)
			}
			for _, line := range log {
				if line.Msg == mergeForwardAskedLine && line.Issue == key {
					mergedForward++
				}
			}
		}
	}
	if mergedForward == 0 {
		t.Fatal("no ACME-1..4 conflict was merged forward across seeds 1..6")
	}
}

// phaseMismatchWindow is a window from 09:00 to 14:00 recorded under legion-v20.42.5 with a cap of
// 20, its inputs reaching back to the daemon's boot at 06:00, and a daemon process that stops and
// the next that starts at each of boots. Eight roots enter testing at 10:15, pass to reviewing at
// 10:50 and end their stretch in reviewing at 11:10: ACME-1 to ACME-4 in a recorded conflict,
// returning to reviewing at 11:30 and approved to accepting at 11:40, and ACME-5 to ACME-8 in
// review_rejected, returning to testing at 11:20. Every recorded stretch ends in reviewing, 4 of 8
// in a conflict, and none in testing; no recorded stretch ends after 11:10, so a boot after it
// moves no stretch of the fit, and the chance is 1/2 whatever the boots.
func phaseMismatchWindow(t *testing.T, boots ...time.Time) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(14, 0))
	b.reaches(3 * time.Hour)
	b.capped(20)
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.boot(clock(6, 0))
	for _, at := range boots {
		b.stopped(at)
		b.boot(at)
	}
	for i := 1; i <= 8; i++ {
		key := fmt.Sprintf("ACME-%d", i)
		moves := []builtMove{
			moveAt(10, 10, phase.Implementing, workflow.TriggerPlannerCompleted),
			moveAt(10, 15, phase.Testing, workflow.TriggerImplementationReady),
			moveAt(10, 50, phase.Reviewing, workflow.TriggerTesterPassed),
		}
		if i <= 4 {
			moves = append(moves,
				moveAt(11, 10, phase.Implementing, workflow.TriggerConflict),
				moveAt(11, 20, phase.Testing, workflow.TriggerImplementationReady),
				moveAt(11, 30, phase.Reviewing, workflow.TriggerTesterPassed),
				moveAt(11, 40, phase.Accepting, workflow.TriggerReviewApproved))
		} else {
			moves = append(moves,
				moveAt(11, 10, phase.Implementing, workflow.TriggerReviewRejected),
				moveAt(11, 20, phase.Testing, workflow.TriggerImplementationReady))
		}
		rootThrough(b, key, fmt.Sprintf("a%02d", i), clock(10, 0), clock(10, 5), moves...)
	}
	return b.build()
}

// bootPhaseMismatchWindow is phaseMismatchWindow with a daemon process that stops and the next that
// starts at 10:30, where each of the eight roots stands in testing, and at each of later. Each
// stretch spans the boot at 10:30 and draws there in reviewing, its fitted phase, while its issue
// stands in testing.
func bootPhaseMismatchWindow(t *testing.T, later ...time.Time) Window {
	return phaseMismatchWindow(t, append([]time.Time{clock(10, 30)}, later...)...)
}

// A conflict a boot draws for a stretch whose fitted phase is later than the phase its issue
// stands in at the boot does not enter in the phase the issue stands in. The test replays
// bootPhaseMismatchWindow under legion-v20.42.5 at the seeds 1 to 6, where every recorded stretch
// ends in reviewing and none in testing: no conflict moves an issue out of testing, and some drawn
// conflict moves an issue out of reviewing.
func TestABootDrawDoesNotMoveAnEarlierPhaseIntoConflict(t *testing.T) {
	t.Parallel()
	window := bootPhaseMismatchWindow(t)
	fromTesting, fromReviewing := 0, 0
	for seed := int64(1); seed <= 6; seed++ {
		_, log, _ := replayLoopWindow(t, window, "legion-v20.42.5", seed)
		for _, line := range log {
			if line.Msg != phaseChangedLine || line.Trigger != workflow.TriggerConflict {
				continue
			}
			switch phase.Phase(line.From) {
			case phase.Testing:
				fromTesting++
			case phase.Reviewing:
				fromReviewing++
			}
		}
	}
	if fromTesting > 0 || fromReviewing == 0 {
		t.Errorf("drawn conflicts left testing %d times and reviewing %d times across seeds 1..6, want 0 from testing and >0 from reviewing",
			fromTesting, fromReviewing)
	}
}

// mismatchKeys is the eight roots of phaseMismatchWindow.
var mismatchKeys = []string{"ACME-1", "ACME-2", "ACME-3", "ACME-4", "ACME-5", "ACME-6", "ACME-7", "ACME-8"}

// drawsOfRun replays window under rule on branch at seed, and answers each draw of a stretch's
// conflict the run made, in its order, and the run's output directory.
func drawsOfRun(t *testing.T, window Window, rule string, branch Branch, seed int64) ([]conflictDraw, string) {
	t.Helper()
	out := t.TempDir()
	var draws []conflictDraw
	if _, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, rule), Branch: branch, Seed: seed, Slowdown: 1,
		PGDSN: replayDSN(t), Out: out, onDraw: func(draw conflictDraw) { draws = append(draws, draw) }}); err != nil {
		t.Fatalf("replay the window under %s on %s at seed %d: %v", rule, branch.Name, seed, err)
	}
	return draws, out
}

// firstDraws is each issue's first draw among draws.
func firstDraws(draws []conflictDraw) map[string]conflictDraw {
	first := map[string]conflictDraw{}
	for _, draw := range draws {
		if _, drew := first[draw.at.issue]; !drew {
			first[draw.at.issue] = draw
		}
	}
	return first
}

// conflictMoves is each move of key by conflict in log, in its order.
func conflictMoves(log []replayLine, key string) []replayLine {
	var moves []replayLine
	for _, line := range log {
		if line.Msg == phaseChangedLine && line.Issue == key && line.Trigger == workflow.TriggerConflict {
			moves = append(moves, line)
		}
	}
	return moves
}

// reviewingPlace is the place of the draw of key's stretch in phaseMismatchWindow, as a test names
// it (drawPlace): the reviewing piece from 10:50 whose recorded move ends it, its first draw.
func reviewingPlace(key string) string {
	return drawPlace(stretchDraw{issue: key, start: clock(10, 50), phase: phase.Reviewing})
}

// A boot draws a stretch under way once, at its first read inside the stretch, at the chance of
// the phase the record ends it in and at the place of the piece that ends it, and its result holds
// for the whole live stretch: a later boot inside the stretch draws it no more. A conflict drawn
// while the issue stands in another phase waits for the stretch's end in the fitted phase and
// enters there with no draw of its own; a stretch that draws none goes on as the record shows. The
// test replays bootPhaseMismatchWindow under legion-v20.42.5 at the seeds 1 to 3, with the boot at
// 10:30 alone and with a second boot at 11:00, where each issue stands in reviewing, its fitted
// phase: each issue's first draw comes at 10:30, at the boot, in testing, at reviewing's chance of
// 1/2, at the place of its reviewing piece from 10:50, and is its only draw up to its stretch's end
// at 11:10. One that draws a conflict moves back from reviewing at 11:10, first, and never from
// testing; one that draws none draws no more and makes no conflict. Each run's draws make some of
// each. It fails where a boot's conflict enters in testing at the boot, where the second boot draws
// a stretch again or enters its waiting conflict, and where the stretch's end draws again.
func TestABootsConflictInAnotherPhaseWaitsForTheStretchsEnd(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		later []time.Time
	}{{"one boot", nil}, {"a second boot", []time.Time{clock(11, 0)}}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			window := bootPhaseMismatchWindow(t, c.later...)
			clean, deferred := 0, 0
			for seed := int64(1); seed <= 3; seed++ {
				draws, out := drawsOfRun(t, window, "legion-v20.42.5", drawnConflictBranch, seed)
				log := replayLog(t, out)
				for _, key := range mismatchKeys {
					var before []conflictDraw
					for _, draw := range draws {
						if draw.at.issue == key && draw.drawn.Before(clock(11, 15)) {
							before = append(before, draw)
						}
					}
					if len(before) != 1 {
						t.Errorf("at seed %d %s drew %d times before 11:15 (%+v), want once, at the boot at 10:30", seed, key, len(before), before)
						continue
					}
					draw := before[0]
					if drawPlace(draw.at) != reviewingPlace(key) || !draw.boot || !draw.drawn.Equal(clock(10, 30)) || draw.running != phase.Testing ||
						math.Abs(draw.chance-0.5) > 1e-6 {
						t.Errorf("at seed %d %s drew at %s (a boot's: %t), in %s, at %.6f; want %s at the boot at 10:30, in testing, at 0.5",
							seed, key, stamp(draw.drawn), draw.boot, draw.running, draw.chance, reviewingPlace(key))
					}
					moves := conflictMoves(log, key)
					switch draw.outcome {
					case drawnDeferred:
						deferred++
						if len(moves) == 0 || !moves[0].Time.Equal(clock(11, 10)) || moves[0].From != string(phase.Reviewing) {
							t.Errorf("at seed %d %s drew a conflict at the boot and moved by conflict %+v, want first from reviewing at 11:10:00", seed, key, moves)
						}
						for _, move := range moves {
							if move.From == string(phase.Testing) {
								t.Errorf("at seed %d %s moved back by conflict from testing at %s", seed, key, stamp(move.Time))
							}
						}
					case drawnClean:
						clean++
						if total := len(drawsOf(draws, key)); total != 1 || len(moves) != 0 {
							t.Errorf("at seed %d %s drew no conflict at the boot and then drew %d times in all and moved by conflict %+v, want once and none",
								seed, key, total, moves)
						}
					default:
						t.Errorf("at seed %d %s's boot draw made %q, want %q or %q", seed, key, draw.outcome, drawnClean, drawnDeferred)
					}
				}
			}
			if clean == 0 || deferred == 0 {
				t.Errorf("the boot drew %d stretches with no conflict and %d with a conflict that waits across seeds 1..3, want some of each", clean, deferred)
			}
		})
	}
}

// drawsOf is each draw of key among draws, in its order.
func drawsOf(draws []conflictDraw, key string) []conflictDraw {
	var of []conflictDraw
	for _, draw := range draws {
		if draw.at.issue == key {
			of = append(of, draw)
		}
	}
	return of
}

// A boot after a stretch has ended moves nothing of that stretch's draw: a draw reads no boot after
// its own moment. The test replays bootPhaseMismatchWindow under legion-v20.42.5 at the seeds 1 to
// 3, with and without a boot at 12:00, after every stretch drawn at 10:30 has ended: each issue's
// first draw is the same in both runs, its place, its moment, u, its chance and its outcome, and
// those draws make both a conflict that waits and no conflict. It fails where a boot moves the draw
// of a stretch that ended before it.
func TestABootAfterAStretchEndedMovesNoEarlierDraw(t *testing.T) {
	t.Parallel()
	without, with := bootPhaseMismatchWindow(t), bootPhaseMismatchWindow(t, clock(12, 0))
	outcomes := map[string]int{}
	for seed := int64(1); seed <= 3; seed++ {
		before, _ := drawsOfRun(t, without, "legion-v20.42.5", drawnConflictBranch, seed)
		after, _ := drawsOfRun(t, with, "legion-v20.42.5", drawnConflictBranch, seed)
		first, later := firstDraws(before), firstDraws(after)
		for _, key := range mismatchKeys {
			a, drew := first[key]
			b, drewToo := later[key]
			switch {
			case !drew || !drewToo:
				t.Errorf("at seed %d %s drew without the boot at 12:00: %t, and with it: %t, want both", seed, key, drew, drewToo)
			case drawPlace(a.at) != drawPlace(b.at) || !a.drawn.Equal(b.drawn) || a.u != b.u || a.chance != b.chance || a.outcome != b.outcome:
				t.Errorf("at seed %d %s first drew %s at %s, u %v at %v, %s without the boot at 12:00, and %s at %s, u %v at %v, %s with it", seed, key,
					drawPlace(a.at), stamp(a.drawn), a.u, a.chance, a.outcome, drawPlace(b.at), stamp(b.drawn), b.u, b.chance, b.outcome)
			default:
				outcomes[a.outcome]++
			}
		}
	}
	if outcomes[drawnClean] == 0 || outcomes[drawnDeferred] == 0 {
		t.Errorf("the first draws made %v across seeds 1..3, want some %q and some %q", outcomes, drawnClean, drawnDeferred)
	}
}

// A boot's draw of a stretch is the draw that stretch makes at its end where no boot reads inside
// it: both take the place of the piece that ends the stretch, so a run at one seed draws the same u
// for the stretch, whether a boot reads inside it or not (common random numbers), and a conflict a
// boot drew in another phase enters where the end's draw would make it. The test replays
// phaseMismatchWindow under legion-v20.42.5 at the seeds 1 to 3, with no boot inside it and with
// the boot at 10:30: each issue's first draw takes the place of its reviewing piece from 10:50, the
// first draw of its stretch, with the same u in both runs, at its end at 11:10 without the boot and
// at 10:30 with it, and makes a conflict in both or in neither; the issue's first conflict move
// comes at the same moment in both, and those draws make both a conflict and none. It fails where a
// boot's draw takes another place, and where a conflict that waits enters elsewhere than the end's.
func TestABootsDrawTakesThePlaceOfTheStretchsEnd(t *testing.T) {
	t.Parallel()
	noBoot, booted := phaseMismatchWindow(t), bootPhaseMismatchWindow(t)
	conflicted, clean := 0, 0
	for seed := int64(1); seed <= 3; seed++ {
		endDraws, endOut := drawsOfRun(t, noBoot, "legion-v20.42.5", drawnConflictBranch, seed)
		bootDraws, bootOut := drawsOfRun(t, booted, "legion-v20.42.5", drawnConflictBranch, seed)
		endLog, bootLog := replayLog(t, endOut), replayLog(t, bootOut)
		atEnd, atBoot := firstDraws(endDraws), firstDraws(bootDraws)
		for _, key := range mismatchKeys {
			e, drew := atEnd[key]
			b, drewToo := atBoot[key]
			if !drew || !drewToo {
				t.Errorf("at seed %d %s drew with no boot: %t, and with the boot: %t, want both", seed, key, drew, drewToo)
				continue
			}
			if drawPlace(e.at) != reviewingPlace(key) || drawPlace(b.at) != reviewingPlace(key) || e.u != b.u || e.conflicted != b.conflicted {
				t.Errorf("at seed %d %s first drew %s, u %v (a conflict: %t) with no boot, and %s, u %v (a conflict: %t) with the boot; want both %s with one u",
					seed, key, drawPlace(e.at), e.u, e.conflicted, drawPlace(b.at), b.u, b.conflicted, reviewingPlace(key))
			}
			if e.boot || !e.drawn.Equal(clock(11, 10)) || !b.boot || !b.drawn.Equal(clock(10, 30)) {
				t.Errorf("at seed %d %s first drew at %s (a boot's: %t) with no boot and at %s (a boot's: %t) with it, want at 11:10:00 its end and at 10:30:00 the boot",
					seed, key, stamp(e.drawn), e.boot, stamp(b.drawn), b.boot)
			}
			endMoves, bootMoves := conflictMoves(endLog, key), conflictMoves(bootLog, key)
			endMoved, bootMoved := len(endMoves) > 0, len(bootMoves) > 0
			if endMoved != bootMoved || endMoved && !endMoves[0].Time.Equal(bootMoves[0].Time) {
				t.Errorf("at seed %d %s moved by conflict %+v with no boot and %+v with the boot, want the first at one moment", seed, key, endMoves, bootMoves)
			}
			if b.conflicted {
				conflicted++
			} else {
				clean++
			}
		}
	}
	if conflicted == 0 || clean == 0 {
		t.Errorf("the first draws made %d conflicts and %d with none across seeds 1..3, want some of each", conflicted, clean)
	}
}

// Under the branch's Conflicts recorded no stretch draws, at a boot or at its end, and each
// recorded conflict enters as recorded (assumption 44). The test replays bootPhaseMismatchWindow
// under legion-v20.42.5 on that branch: the run makes no draw and writes no line of one, ACME-1 to
// ACME-4 move back by conflict from reviewing first at 11:10, their recorded conflicts, and ACME-5
// to ACME-8 never by conflict. It fails where the boot at 10:30 draws on that branch.
func TestTheRecordedConflictsBranchDrawsNoBootConflict(t *testing.T) {
	t.Parallel()
	draws, out := drawsOfRun(t, bootPhaseMismatchWindow(t), "legion-v20.42.5", MainBranch, 1)
	if len(draws) > 0 {
		t.Errorf("the recorded conflicts branch drew %d times (%+v), want none", len(draws), draws)
	}
	log := replayLog(t, out)
	for _, line := range log {
		if line.Msg == conflictDrawnLine {
			t.Errorf("the recorded conflicts branch wrote a line of a draw at %s, for %s", stamp(line.Time), line.Issue)
		}
	}
	for i, key := range mismatchKeys {
		moves := conflictMoves(log, key)
		switch {
		case i < 4 && (len(moves) == 0 || !moves[0].Time.Equal(clock(11, 10)) || moves[0].From != string(phase.Reviewing)):
			t.Errorf("%s moved by conflict %+v, want first from reviewing at 11:10:00, as recorded", key, moves)
		case i >= 4 && len(moves) > 0:
			t.Errorf("%s moved by conflict %+v, want never, as recorded", key, moves)
		}
	}
}

// drawnLine is a line of a draw in a run's log (conflictDrawnLine), by its attributes.
type drawnLine struct {
	Time    time.Time   `json:"time"`
	Msg     string      `json:"msg"`
	Issue   string      `json:"issue"`
	Stretch int         `json:"stretch"`
	Running phase.Phase `json:"running"`
	Fitted  phase.Phase `json:"fitted"`
	Start   time.Time   `json:"start"`
	Merges  int         `json:"merges"`
	By      string      `json:"by"`
	Chance  float64     `json:"chance"`
	U       float64     `json:"u"`
	Outcome string      `json:"outcome"`
}

// A run writes one line of its own to its log for each draw it makes, at the moment of the draw:
// the issue, the number of its live stretch, the running and the fitted phase, the replay's start
// of the stretch, M, whether a boot or the stretch's end drew it, the chance, u and the outcome. No
// daemon writes the line, and the trace reads nothing of it, so no move is read from it. The test
// replays bootPhaseMismatchWindow under legion-v20.42.5 at seed 1: the log holds one such line for
// each draw the run makes, in the order it makes them, each giving that draw, and the eight boot
// draws each name a live stretch of its own; the trace does not read the line's msg. It fails for a
// draw with no line or a line that gives another draw, and for a line the trace reads.
func TestEachDrawWritesALineOfItsOwn(t *testing.T) {
	t.Parallel()
	if TraceReads(conflictDrawnLine) {
		t.Errorf("the trace reads %q, a line of the replay's own", conflictDrawnLine)
	}
	draws, out := drawsOfRun(t, bootPhaseMismatchWindow(t), "legion-v20.42.5", drawnConflictBranch, 1)
	raw, err := os.ReadFile(filepath.Join(out, logFile))
	if err != nil {
		t.Fatal(err)
	}
	var lines []drawnLine
	for i, text := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var head struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(text, &head); err != nil {
			t.Fatalf("read line %d of the run's log: %v", i+1, err)
		}
		if head.Msg != conflictDrawnLine {
			continue
		}
		var line drawnLine
		if err := json.Unmarshal(text, &line); err != nil {
			t.Fatalf("read line %d of the run's log, a line of a draw: %v", i+1, err)
		}
		lines = append(lines, line)
	}
	if len(lines) != len(draws) || len(draws) == 0 {
		t.Fatalf("the run drew %d times and wrote %d lines of a draw, want one line for each of some draws", len(draws), len(lines))
	}
	stretches := map[int]bool{}
	for i, draw := range draws {
		line, by := lines[i], "end"
		if draw.boot {
			by = "boot"
			stretches[draw.stretch] = true
		}
		if line.Issue != draw.at.issue || line.Stretch != draw.stretch || line.Running != draw.running || line.Fitted != draw.at.phase ||
			!line.Start.Equal(draw.start) || !line.Time.Equal(draw.drawn) || line.Merges != draw.merges || line.By != by || line.Chance != draw.chance ||
			line.U != draw.u || line.Outcome != draw.outcome {
			t.Errorf("line %d of a draw reads %+v, want the draw %+v", i+1, line, draw)
		}
	}
	if len(stretches) != len(mismatchKeys) {
		t.Errorf("the boot draws named %d live stretches, want %d, one for each issue", len(stretches), len(mismatchKeys))
	}
}
