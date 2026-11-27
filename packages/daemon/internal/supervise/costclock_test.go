package supervise

// The clocks of the cost reading (costclock.go): where the reading starts and stops a claim's
// clocks of R5's first and second waits — at a daemon's start, at a seen report of the latest
// write, at the daemon's put-back, and at the machine's lines of its view of the worker's turns —
// read through the machine and its own log, or from a log written line by line.

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// A daemon's start stops every clock as of the line before it, which is the last the reading
// knows the daemon before it lived, and the clock stands still until the claim's worker next acts
// in the log. A number seen under a daemon whose last line came two minutes later, which started
// again long after the number's bound, and taken once the worker acted again, is not listed. A
// clock that a worker's act started again after the start, and whose bound ends at a write of its
// own number — the first bound's own end writing it again over the seen report the started daemon
// read from the store (decision 20), which follows the bound by the time that decision takes — has
// not passed the bound. A worker that acted after the start and then took nothing for a bound is
// listed from that act.
func TestTaskCostLogStopsTheClockAtADaemonsStartUntilTheWorkersNextAct(t *testing.T) {
	b := costLines{t: t, at: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	cost := b.readCost([]string{
		b.written(0, "restarted", 1, 1, TaskValueQueued),
		b.seen(0, "restarted", 1, 1),
		b.turn(0, turnStartedLine, "rewritten-at-its-bound"),
		b.turn(0, turnStartedLine, "restarted-after-a-turn"),
		b.written(30*time.Second, "restarted-after-a-turn", 1, 1, TaskValueQueued),
		b.seen(30*time.Second, "restarted-after-a-turn", 1, 1),
		b.written(time.Minute, "rewritten-at-its-bound", 1, 1, TaskValueQueued),
		b.seen(time.Minute, "rewritten-at-its-bound", 1, 1),
		b.turn(2*time.Minute, turnStartedLine, "another-claim"),
		b.line(30*time.Minute, DaemonStartedLine, "", "project", "INFRA"),
		b.turn(31*time.Minute, turnEndedLine, "restarted-after-a-turn"),
		b.turn(32*time.Minute, turnEndedLine, "rewritten-at-its-bound"),
		b.written(32*time.Minute+takeBound+50*time.Millisecond, "rewritten-at-its-bound", 1, 2, TaskValueUnseen),
		b.seen(38*time.Minute, "rewritten-at-its-bound", 1, 2),
		b.answered(39*time.Minute, "rewritten-at-its-bound", 1, TakeOutcomeAccepted, ""),
		b.answered(40*time.Minute, "restarted", 1, TakeOutcomeAccepted, ""),
	})
	want := []string{
		"restarted-after-a-turn task 1: the clock started 2026-11-14T09:31:00.000Z, the bound passed 2026-11-14T09:36:00.000Z, still owed at the window's end",
	}
	if !slices.Equal(cost.Unreached, want) {
		t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
			strings.Join(cost.Unreached, "\n"), strings.Join(want, "\n"))
	}
}

// A seen report starts a number's clock only where it names the claim's latest write: that
// revision of that epoch. A bucket created again counts its revisions from 1 once more, so a late
// report of the bucket before it — revision 1 of the old epoch, which the daemon logs since it is
// of another epoch than the revision it recorded seen — is no report of revision 1 of the new
// one: it leaves the new write unseen, as the machine reads it, which charges nobody, so the clock
// stops and the number is not listed. The new epoch's own report starts the clock, and a number
// its worker then leaves untaken is listed from that report. A seen report that names no epoch is
// an older daemon's, matched by its revision alone.
//
// The negative control is the match by revision alone, as the reading made it before the seen
// report named its epoch: the late report of the old bucket starts the clock again, and the
// number whose new write its worker reported seen and then saw replaced by the old one is listed.
func TestTaskCostLogMatchesASeenReportToItsWriteByEpochAndRevision(t *testing.T) {
	const old, created = "2026-11-14T08:00:00.123456789Z", "2026-11-14T09:01:00.000000001Z"
	b := costLines{t: t, at: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	cost := b.readCost([]string{
		b.writtenIn(0, "created-again", 1, old, 1, TaskValueQueued),
		b.seenIn(500*time.Millisecond, "created-again", 1, old, 1),
		b.writtenIn(time.Minute, "created-again", 1, created, 1, TaskValueRepair),
		b.seenIn(time.Minute+500*time.Millisecond, "created-again", 1, created, 1),
		b.seenIn(time.Minute+time.Second, "created-again", 1, old, 1),

		b.writtenIn(0, "created-again-and-seen", 1, old, 1, TaskValueQueued),
		b.seenIn(500*time.Millisecond, "created-again-and-seen", 1, old, 1),
		b.writtenIn(time.Minute, "created-again-and-seen", 1, created, 1, TaskValueRepair),
		b.seenIn(time.Minute+time.Second, "created-again-and-seen", 1, created, 1),

		b.writtenIn(0, "an-older-daemons-report", 1, old, 1, TaskValueQueued),
		b.seen(time.Second, "an-older-daemons-report", 1, 1),
	})
	want := []string{
		"an-older-daemons-report task 1: the clock started 2026-11-14T09:00:01.000Z, the bound passed 2026-11-14T09:05:01.000Z, still owed at the window's end",
		"created-again-and-seen task 1: the clock started 2026-11-14T09:01:01.000Z, the bound passed 2026-11-14T09:06:01.000Z, still owed at the window's end",
	}
	if !slices.Equal(cost.Unreached, want) {
		t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
			strings.Join(cost.Unreached, "\n"), strings.Join(want, "\n"))
	}
}

// The reader reads the daemon's own put-back (decision 8): a run the daemon gave an accepted take,
// whose report named no task, was no turn of that number. Walked through the machine and its log
// as the daemon writes it, both ways the report can come — after the run's end frame, and ahead of
// it while the run goes on — that run counts as a turn of no cause, its start and its end, so the
// number took one turn, its own. The charge the second wait then makes one bound after the
// put-back, or after the end of the run the take waited behind, is no charge of a worker whose
// turn began within the second bound, though the run began ten seconds after the take. The take
// taken back at that charge is owed a second time, so its second take is no take too many, and no
// time owed went a first bound untaken.
//
// The negative control is the put-back line not read: the number took two turns, and the run the
// daemon gave the take is read as a turn within the second bound of a charge made after it.
func TestTaskCostLogReadsTheMachinesOwnPutBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		// putBack walks the claim from the start of the run the daemon gave the take to the take
		// awaiting its turn on an idle claim, with the put-back between.
		putBack func(h *harness)
	}{
		{"the report after the run's end", func(h *harness) {
			h.advance(10 * time.Minute)
			h.mustQuiet(StreamTurnEnd{Claim: testToken})
			h.advance(time.Minute)
			if err := h.reportTurn(0, nil); err != nil {
				h.t.Fatalf("report the run as no task's: %v", err)
			}
		}},
		{"the report ahead of the run's end", func(h *harness) {
			h.advance(time.Minute)
			if err := h.reportTurn(0, nil); err != nil {
				h.t.Fatalf("report the run as no task's: %v", err)
			}
			h.advance(10 * time.Minute)
			h.mustQuiet(StreamTurnEnd{Claim: testToken})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, log := jsonHarness(t, currentPlugin)
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:1", Row: 1,
				Phase: phase.Implementing, Generation: 1})
			h.mustTake()
			h.advance(10 * time.Second)
			h.must(StreamTurnStart{Claim: testToken})
			tc.putBack(h)
			if p := h.pending(); !p.awaitsStart() {
				t.Fatalf("after the put-back the task is %+v, want its take awaiting its turn", p)
			}
			h.advance(turnStartBound)
			h.wantCharges(WaitTurnStart)
			h.mustTake()
			h.advance(30 * time.Second)
			h.must(StreamTurnStart{Claim: testToken})
			h.advance(5 * time.Minute)
			h.must(StreamTurnEnd{Claim: testToken})
			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending %+v, want the task finished by its own turn's report", p)
			}
			if n := strings.Count(log.String(), `"msg":"`+notTheTakesTurnLine+`"`); n != 1 {
				t.Fatalf("the log holds %d put-back lines, want one", n)
			}

			cost := readLogCost(t, h, log)
			if cost.TurnsStarted != (TurnCauses{Task: 1, None: 1}) || cost.TurnsEnded != (TurnCauses{Task: 1, None: 1}) {
				t.Errorf("turns started %+v and ended %+v, want one by a task number and the put-back run with no cause each", cost.TurnsStarted, cost.TurnsEnded)
			}
			if want := []TaskTurns{{string(testToken), 1, 1}}; !slices.Equal(cost.TurnsOfTask, want) {
				t.Errorf("turns of each number %+v, want %+v", cost.TurnsOfTask, want)
			}
			if !maps.Equal(cost.ChargesByWait, map[string]int{WaitTurnStart: 1}) {
				t.Errorf("charges by wait %v, want the one charge of the second wait", cost.ChargesByWait)
			}
			if len(cost.ChargedWithinBound) != 0 || len(cost.TakenNotOncePerOwed) != 0 || len(cost.Unreached) != 0 {
				t.Errorf("the first verdict fails on %q, the third on %q, and %q were owed past the first bound; want neither verdict to fail and no number owed so",
					cost.ChargedWithinBound, cost.TakenNotOncePerOwed, cost.Unreached)
			}
		})
	}
}

// Verdict 1 reads a take's second wait from where the machine begins it (secondWaitNow): the
// latest of the take's acceptance, its put-back, the end of a turn that ran while it awaited its
// turn, and the worker's first act after a daemon's start. A charge of the second wait is a charge
// of a worker whose turn began within the second bound wherever a turn began within one bound of
// that start, however long after the acceptance, and it is listed with the start it was judged
// from. Here the take's own turn began within one bound of each — of the worker's first act after
// a daemon's start, a take it sent again; of its put-back; and of the end of the run it waited
// behind after a put-back that came ahead of that run's end — and the worker was charged all the
// same, which the daemon must never do.
//
// The negative controls are the second wait begun at the acceptance alone, where the put-back's
// charge is listed against the run the put-back gave back and the other two are not listed, at
// the put-back but never at the end of the run it waited behind, and at no act after a daemon's
// start: each leaves its charge unlisted or listed against the wrong turn.
func TestTaskCostLogFirstVerdictReadsTheSecondWaitFromWhereTheMachineBeginsIt(t *testing.T) {
	b := costLines{t: t, at: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	taken := func(claim string) []string {
		return []string{
			b.written(0, claim, 1, 1, TaskValueQueued),
			b.seen(0, claim, 1, 1),
			b.answered(time.Second, claim, 1, TakeOutcomeAccepted, ""),
		}
	}
	taskTurn := func(offset time.Duration, msg, claim string) string {
		return b.line(offset, msg, claim, "generation", 1, "task", 1)
	}
	putBack := func(offset time.Duration, claim string) string {
		return b.line(offset, notTheTakesTurnLine, claim, "task", 1)
	}
	charged := func(offset time.Duration, claim string) string {
		return b.line(offset, promptFailureLine, claim, "wait", WaitTurnStart, "bound", int64(turnStartBound))
	}
	var lines []string
	lines = append(lines, taken("after-a-daemon-start")...)
	lines = append(lines,
		b.line(30*time.Minute, DaemonStartedLine, "", "project", "INFRA"),
		b.line(31*time.Minute, takeAnsweredLine, "after-a-daemon-start", "task", 1, "outcome", TakeOutcomeAccepted, "hold", "", "repeat", true),
		taskTurn(32*time.Minute, turnStartedLine, "after-a-daemon-start"),
		charged(56*time.Minute, "after-a-daemon-start"))
	lines = append(lines, taken("put-back")...)
	lines = append(lines,
		taskTurn(10*time.Second, turnStartedLine, "put-back"),
		taskTurn(20*time.Minute, turnEndedLine, "put-back"),
		putBack(21*time.Minute, "put-back"),
		taskTurn(22*time.Minute, turnStartedLine, "put-back"),
		charged(46*time.Minute, "put-back"))
	lines = append(lines, taken("in-turn-hold")...)
	lines = append(lines,
		taskTurn(10*time.Second, turnStartedLine, "in-turn-hold"),
		putBack(time.Minute, "in-turn-hold"),
		taskTurn(30*time.Minute, turnEndedLine, "in-turn-hold"),
		taskTurn(31*time.Minute, turnStartedLine, "in-turn-hold"),
		charged(55*time.Minute, "in-turn-hold"))
	cost := b.readCost(lines)
	want := []string{
		"after-a-daemon-start task 1: taken 2026-11-14T09:00:01.000Z, its second wait begun again 2026-11-14T09:31:00.000Z, a turn started 2026-11-14T09:32:00.000Z, charged 2026-11-14T09:56:00.000Z (wait turn-start, bound 25m0s)",
		"put-back task 1: taken 2026-11-14T09:00:01.000Z, its second wait begun again 2026-11-14T09:21:00.000Z, a turn started 2026-11-14T09:22:00.000Z, charged 2026-11-14T09:46:00.000Z (wait turn-start, bound 25m0s)",
		"in-turn-hold task 1: taken 2026-11-14T09:00:01.000Z, its second wait begun again 2026-11-14T09:30:00.000Z, a turn started 2026-11-14T09:31:00.000Z, charged 2026-11-14T09:55:00.000Z (wait turn-start, bound 25m0s)",
	}
	if !slices.Equal(cost.ChargedWithinBound, want) {
		t.Fatalf("the first verdict fails on\n%s\nwant\n%s", strings.Join(cost.ChargedWithinBound, "\n"), strings.Join(want, "\n"))
	}
	if want := []TaskTurns{{"after-a-daemon-start", 1, 1}, {"in-turn-hold", 1, 1}, {"put-back", 1, 1}}; !slices.Equal(cost.TurnsOfTask, want) {
		t.Errorf("turns of each number %+v, want %+v: each put-back run is a turn of no cause", cost.TurnsOfTask, want)
	}
}

// readLogCost reads the machine's whole log, every line of the walk inside the window.
func readLogCost(t *testing.T, h *harness, log *testlog.Buffer) TaskCost {
	t.Helper()
	cost, err := ReadTaskCost(strings.NewReader(log.String()), TaskCostArgs{From: epoch.Add(-time.Hour), To: h.clock.Now().Add(time.Second), Take: takeBound})
	if err != nil {
		t.Fatal(err)
	}
	return cost
}

// logDaemonStarted writes the daemon's start line into the machine's log, as internal/daemon writes
// it once its boot's steps are done: after it restored every claim (restart).
func logDaemonStarted(h *harness) {
	h.deps.Log.Info(DaemonStartedLine, "project", "INFRA")
}

// daemonRestart is a daemon stopped for d and started again (outage), its lines written as
// internal/daemon writes them: its starting line first, then the restore of the claim (NewMachine).
// The start line, which comes after the restore, is the test's to write (logDaemonStarted).
func daemonRestart(h *harness, d time.Duration) {
	h.t.Helper()
	h.m.Wait()
	h.clock.dropAll()
	h.clock.Advance(d)
	h.deps.Log.Info(DaemonStartingLine, "project", "INFRA")
	h.start(h.store.load(h.token))
}

// unreachedFrom is the line the reading lists for the claim's task number whose first-wait clock
// started at from, passed the first bound with no take, and was still owed at the window's end.
func unreachedFrom(h *harness, number uint64, from time.Time) []string {
	return []string{fmt.Sprintf("%s task %d: the clock started %s, the bound passed %s, still owed at the window's end",
		h.token, number, lineTime(from), lineTime(from.Add(takeBound)))}
}

// After a daemon's start the machine arms R5's second wait over a take awaiting its turn no
// earlier than its view of the worker's turns is whole again: here, at the agent's answer to the
// ask the shim's first hello makes (connview.go). The reading begins verdict 1's window there too,
// from the machine's two lines of that view (viewCutLine, viewWholeLine), and not at the worker's
// next act it decodes: a take the worker sends again two minutes after the answer, answered from
// the record. The daemon charges one bound after the answer and takes the task back; the worker
// takes it again, and that turn starts thirty seconds after the charge, inside one bound of the
// take sent again, so a window begun at that act would read a turn within the bound and fail a
// charge the daemon made rightly. Both places of the daemon's start line are read so: after the
// restore and before the hello, and after the answer, as a boot whose last steps outlast the
// shim's hello and its agent's answer writes it.
//
// The negative controls are the reading before it read the two lines (the change's parent), whose
// window began at the take sent again: the charge is listed against the retaken task's turn; and a
// start line that stops every clock whatever began its start (daemonStarted), whose window, with
// the start line written after the answer, began at the take sent again as well.
func TestTaskCostLogReadsTheTurnStartWaitAfterADaemonStartFromTheAnswerNotTheWorkersNextAct(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		startedAfterTheAnswer bool
	}{
		{"the start line before the hello", false},
		{"the start line after the answer", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, log := jsonHarness(t, currentPlugin)
			h.manualTakes()
			h.reach(StateReady)
			h.takeOf("implement the plan")
			value, _ := h.values.value(h.token)
			h.advance(time.Minute)
			daemonRestart(h, 0)
			if !tc.startedAfterTheAnswer {
				logDaemonStarted(h)
			}
			h.advance(time.Minute)
			h.connect()
			h.advance(time.Minute)
			h.answerAsk(false)
			h.wantArmedFromNow(TimerTurnStart, turnStartBound, "at the answer")
			if tc.startedAfterTheAnswer {
				logDaemonStarted(h)
			}
			h.advance(2 * time.Minute)
			if again, err := h.takeValue(value); err != nil || again.Outcome != TakeOutcomeAccepted {
				t.Fatalf("the take sent again = %+v, %v; want it answered accepted from the record", again, err)
			}
			h.advance(turnStartBound - 2*time.Minute)
			h.wantCharges(WaitTurnStart)
			h.mustTake()
			h.advance(30 * time.Second)
			h.must(StreamTurnStart{Claim: h.token})
			h.advance(5 * time.Minute)
			h.must(StreamTurnEnd{Claim: h.token})

			cost := readLogCost(t, h, log)
			if !maps.Equal(cost.ChargesByWait, map[string]int{WaitTurnStart: 1}) {
				t.Fatalf("charges by wait %v, want the one charge of the second wait", cost.ChargesByWait)
			}
			if len(cost.ChargedWithinBound) != 0 {
				t.Errorf("the first verdict fails on %q, want the charge one bound after the answer passed", cost.ChargedWithinBound)
			}
		})
	}
}

// A take the worker makes after a daemon's start, before the ask its shim's hello makes is
// answered, is under no second wait until the machine's view of the worker's turns is whole again
// (connview.go): the wait begins at the agent's answer, and the reading's verdict 1 begins its
// window there too (viewWholeLine), not at the take. Through the machine and its own log, the
// daemon charges one bound after the answer, and the reading passes the charge. In a log of a
// daemon that charged a worker whose turn began within one bound of that answer, though more than
// a bound after its take, the reading lists the charge, its second wait begun again at the answer.
//
// The negative control is the reading before it read the two lines (the change's parent), whose
// window began at the take and ended before that turn: the charge is not listed.
func TestTaskCostLogReadsTheTurnStartWaitOfATakeMadeBeforeTheAnswerFromTheAnswer(t *testing.T) {
	t.Run("the machine's own log", func(t *testing.T) {
		h, log := jsonHarness(t, currentPlugin)
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		daemonRestart(h, 0)
		logDaemonStarted(h)
		h.advance(time.Minute)
		h.mustTake()
		h.advance(time.Minute)
		h.connect()
		h.advance(time.Minute)
		h.answerAsk(false)
		h.wantArmedFromNow(TimerTurnStart, turnStartBound, "at the answer")
		h.advance(turnStartBound)
		h.wantCharges(WaitTurnStart)
		h.mustTake()
		h.advance(30 * time.Second)
		h.must(StreamTurnStart{Claim: h.token})
		h.advance(5 * time.Minute)
		h.must(StreamTurnEnd{Claim: h.token})

		cost := readLogCost(t, h, log)
		if !maps.Equal(cost.ChargesByWait, map[string]int{WaitTurnStart: 1}) || len(cost.ChargedWithinBound) != 0 {
			t.Errorf("charges by wait %v, and the first verdict fails on %q; want the one charge of the second wait, passed",
				cost.ChargesByWait, cost.ChargedWithinBound)
		}
	})
	t.Run("a turn within one bound of the answer", func(t *testing.T) {
		const claim = "taken-before-the-answer"
		b := costLines{t: t, at: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
		cost := b.readCost([]string{
			b.written(0, claim, 1, 1, TaskValueQueued),
			b.seen(time.Second, claim, 1, 1),
			b.line(10*time.Minute, DaemonStartingLine, "", "project", "INFRA"),
			b.line(10*time.Minute, viewCutLine, claim, "task", 1),
			b.line(10*time.Minute, DaemonStartedLine, "", "project", "INFRA"),
			b.answered(11*time.Minute, claim, 1, TakeOutcomeAccepted, ""),
			b.line(12*time.Minute, viewAskedLine, claim, "task", 1),
			b.line(13*time.Minute, viewWholeLine, claim, "task", 1),
			b.line(36*time.Minute+30*time.Second, turnStartedLine, claim, "generation", 1, "task", 1),
			b.line(38*time.Minute, promptFailureLine, claim, "wait", WaitTurnStart, "bound", int64(turnStartBound)),
		})
		want := []string{claim + " task 1: taken 2026-11-14T09:11:00.000Z, its second wait begun again 2026-11-14T09:13:00.000Z, a turn started 2026-11-14T09:36:30.000Z, charged 2026-11-14T09:38:00.000Z (wait turn-start, bound 25m0s)"}
		if !slices.Equal(cost.ChargedWithinBound, want) {
			t.Fatalf("the first verdict fails on\n%s\nwant\n%s", strings.Join(cost.ChargedWithinBound, "\n"), strings.Join(want, "\n"))
		}
	})
}

// A close stops R5's first wait over a value the worker reported seeing, and the agent's answer to
// the ask the next hello makes arms it again, whole (connview.go); the reading stops and starts
// the number's clock at the machine's two lines of that view (viewCutLine, viewWholeLine). The
// value is seen a minute before the close, no connection stands for longer than the first bound,
// and the value is left untaken for a bound after the answer: the daemon charges the first wait
// one bound after the answer, and the reading lists the number from the answer, not from its seen
// report.
//
// The negative control is the reading before it read the two lines (the change's parent), whose
// clock ran through the close and passed the bound while no connection stood.
func TestTaskCostLogStopsTheClockAtACloseAndStartsItWholeAtTheAnswerOfNoTurn(t *testing.T) {
	h, log := jsonHarness(t, currentPlugin, longReconnect)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	number := h.pending().Number
	h.seen()
	h.advance(time.Minute)
	h.reconnectLater()
	h.advance(takeBound + time.Minute)
	h.wantCharges()
	h.helloOf(true, false)
	h.advance(time.Minute)
	h.answerAsk(false)
	answered := h.clock.Now()
	h.wantArmedFromNow(TimerTake, takeBound, "at the answer")
	h.advance(takeBound)
	h.wantCharges(WaitTake)

	cost := readLogCost(t, h, log)
	if want := unreachedFrom(h, number, answered); !slices.Equal(cost.Unreached, want) {
		t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
			strings.Join(cost.Unreached, "\n"), strings.Join(want, "\n"))
	}
}

// No act of the worker starts the number's clock while the machine's view of its turns is cut
// (claimClock.runs reads cut), as the machine's first wait waits for a whole view (firstWaitNow):
// a seen report the worker makes over HTTP while its connection is closed starts none. A close, a
// task written and reported seen while no connection stands, a gap past the first bound, the hello
// and the agent's answer, then no take for a bound: the daemon charges the first wait one bound
// after the answer, and the reading lists the number from the answer, not from the seen report.
//
// The negative control is the seen report starting the clock over a cut view (claimClock.runs
// without its cut): the reading lists a bound that passed while no connection stood.
func TestTaskCostLogStartsNoClockAtASeenReportMadeWhileNoConnectionStands(t *testing.T) {
	h, log := jsonHarness(t, currentPlugin, longReconnect)
	h.manualTakes()
	h.reach(StateReady)
	h.reconnectLater()
	h.advance(time.Minute)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	number := h.pending().Number
	h.seen()
	h.advance(takeBound + time.Minute)
	h.wantCharges()
	h.helloOf(true, false)
	h.advance(time.Minute)
	h.answerAsk(false)
	answered := h.clock.Now()
	h.wantArmedFromNow(TimerTake, takeBound, "at the answer")
	h.advance(takeBound)
	h.wantCharges(WaitTake)

	cost := readLogCost(t, h, log)
	if want := unreachedFrom(h, number, answered); !slices.Equal(cost.Unreached, want) {
		t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
			strings.Join(cost.Unreached, "\n"), strings.Join(want, "\n"))
	}
}

// A turn frame on the connection a reconnect hello opened, before the agent's answer, makes the
// machine's view of the worker's turns whole (seeView) through the one writer of that view, which
// logs the crossing (setView, viewWholeLine): the machine arms the first wait there, and the
// reading starts the number's clock there. A value reported seen and left untaken, a close, a gap
// past the first bound, the hello over the pending task, and a turn's end frame before the answer:
// the frame writes the whole line, the daemon charges the first wait one bound after it, and the
// reading lists the number from the frame.
//
// The negative control is the frame's view made whole with no line (connSeen set past setView):
// the close's line holds the reading's clock still, and the number is not listed.
func TestTaskCostLogStartsTheClockAtTheTurnFrameThatMakesTheViewWhole(t *testing.T) {
	h, log := jsonHarness(t, currentPlugin, longReconnect)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	number := h.pending().Number
	h.seen()
	h.advance(time.Minute)
	h.reconnectLater()
	h.advance(takeBound + time.Minute)
	h.wantCharges()
	h.helloOf(true, false)
	h.advance(time.Minute)
	wholeLines := len(h.records.of(viewWholeLine))
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	whole := h.clock.Now()
	if got := len(h.records.of(viewWholeLine)); got != wholeLines+1 {
		t.Fatalf("the turn frame wrote %d whole lines, want the one of the view it made whole", got-wholeLines)
	}
	h.wantArmedFromNow(TimerTake, takeBound, "at the turn frame")
	h.advance(takeBound)
	h.wantCharges(WaitTake)

	cost := readLogCost(t, h, log)
	if want := unreachedFrom(h, number, whole); !slices.Equal(cost.Unreached, want) {
		t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
			strings.Join(cost.Unreached, "\n"), strings.Join(want, "\n"))
	}
}

// The view lines are written where the machine's view of the worker's turns crosses whole, and
// nowhere else (setView): a view set whole over one already whole changes no bound, since the
// machine arms a bound again only where it was disarmed or its take changed (armWaits), so a whole
// line there would start the reading's clocks, and a take's second wait, again where the machine's
// run on. The walk sets the view whole over a whole one five times — at the launch (launchNow's
// letGo), at the process's first hello (helloed), at a hello over no pending task with no close
// before it (helloView), and at a suspension's two let-gos, its stop's (suspendProcess) and its
// own (suspended) — and crosses once each way, at a close and at the hello over no task after it:
// the log holds the cut line and the whole line of that crossing alone.
//
// The negative control is a whole line at every set to whole: the log holds five more.
func TestTaskCostLogWritesNoWholeLineWhereTheViewWasWholeAlready(t *testing.T) {
	h, _ := jsonHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	h.connect()
	h.reconnectLater()
	h.connect()
	h.reach(StateSuspended)

	var lines []string
	for _, r := range h.records.ofAny(viewCutLine, viewWholeLine) {
		lines = append(lines, r.message)
	}
	if want := []string{viewCutLine, viewWholeLine}; !slices.Equal(lines, want) {
		t.Fatalf("the view lines are\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// A close over no pending task, and the hello after it over none: that hello asks nothing, and the
// view is whole at it (helloView), which the machine logs (viewWholeLine), so the reading's clocks
// run again from there. A task written after it is taken and no turn starts: the daemon charges
// the second wait one bound after the take, takes the task back and writes it again, and the
// reading passes the charge. The worker reports the value written again seen and leaves it a bound
// untaken: the daemon charges the first wait, and the reading lists the number from that report.
//
// The negative control is that hello's view made whole with no line: the close's line holds the
// reading's clock still, and the number is not listed.
func TestTaskCostLogReadsTheTurnStartWaitAfterAHelloOverNoTaskFromTheTake(t *testing.T) {
	h, log := jsonHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	h.reconnectLater()
	h.advance(time.Minute)
	h.helloOf(true, false)
	h.advance(time.Minute)
	wantTakeChargedPastBothBounds(t, h, log, "implement the plan")
}

// wantTakeChargedPastBothBounds is task taken with no turn started after it, over a connection
// whose view the reading's clocks run on: the daemon charges the second wait one bound after the
// take, and the reading passes the charge; the value written again is reported seen and left a
// bound untaken, the daemon charges the first wait, and the reading lists the number from that
// report.
func wantTakeChargedPastBothBounds(t *testing.T, h *harness, log *testlog.Buffer, task string) {
	t.Helper()
	number := h.takeOf(task)
	h.advance(turnStartBound)
	h.wantCharges(WaitTurnStart)
	h.seen()
	seen := h.clock.Now()
	h.advance(takeBound)
	h.wantCharges(WaitTurnStart, WaitTake)

	cost := readLogCost(t, h, log)
	if len(cost.ChargedWithinBound) != 0 {
		t.Errorf("the first verdict fails on %q, want the charge one bound after the take passed", cost.ChargedWithinBound)
	}
	if want := unreachedFrom(h, number, seen); !slices.Equal(cost.Unreached, want) {
		t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
			strings.Join(cost.Unreached, "\n"), strings.Join(want, "\n"))
	}
}

// A daemon start's own ask of a claim it restored working (askFirst) serves the shim's first
// hello since that start, and the agent's answer to it makes the view whole with nothing relaunched
// (askTurnOver), which the machine logs (viewWholeLine), so the reading's clocks run again from
// there. The agent answers that it is still in the take's turn, whose end then finishes the task
// by its report. A second task is taken and no turn starts: the daemon charges the second wait one
// bound after the take, and the reading passes the charge. The value written again is reported
// seen and left a bound untaken: the daemon charges the first wait, and the reading lists the
// number from that report.
//
// The negative control is that answer's view made whole with no line: the restore's line
// (viewCutLine) holds the reading's clock still for good, since no later change of the view writes
// one, and the number is not listed.
func TestTaskCostLogRunsTheClocksFromTheAnswerToADaemonStartsOwnAskAfterItsTurn(t *testing.T) {
	h, log := jsonHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateWorking)
	daemonRestart(h, 0)
	logDaemonStarted(h)
	h.advance(time.Minute)
	h.conn.SetStreaming(true)
	h.connect()
	h.wantState(StateWorking)
	h.advance(time.Minute)
	h.must(StreamTurnEnd{Claim: h.token})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v after the turn's end, want the task finished by its report", p)
	}
	h.advance(time.Minute)
	wantTakeChargedPastBothBounds(t, h, log, "carry on")
}

// A daemon start restores a claim whose process was launching with no hello seen (viewNoHello),
// which the machine logs (viewCutLine). That process's first hello makes the view whole (helloed),
// which it logs too (viewWholeLine), and the reading's clocks run from there. The agent registers
// and says it is ready, a task is written, and its value is reported seen and left a bound
// untaken: the daemon charges the first wait, and the reading lists the number from that report.
//
// The negative control is that first hello's view made whole with no line: the restore's line
// holds the reading's clock still, and the number is not listed.
func TestTaskCostLogRunsTheTurnClocksFromTheFirstHelloOfAProcessRestoredLaunching(t *testing.T) {
	h, log := jsonHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateLaunching)
	daemonRestart(h, 0)
	logDaemonStarted(h)
	h.advance(time.Minute)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	number := h.pending().Number
	h.seen()
	seen := h.clock.Now()
	h.advance(takeBound)
	h.wantCharges(WaitTake)

	cost := readLogCost(t, h, log)
	if want := unreachedFrom(h, number, seen); !slices.Equal(cost.Unreached, want) {
		t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
			strings.Join(cost.Unreached, "\n"), strings.Join(want, "\n"))
	}
}

// No time between the last line of a daemon and the start of the next one counts toward the first
// bound: the machine a daemon start builds runs none of its waits until the worker's first act and
// its view's whole line, so the time no daemon ran is no time the worker had to take its task.
// A daemon's starting line (DaemonStartingLine) is the first of its lines the reading decodes, so
// the reading stops every clock there, as of the last line it decoded before, and no line of the
// new daemon after it — its boot's lines, a line it decodes before its restore and start lines,
// such as a boot write of another claim's value — counts as the daemon before's. A number seen a
// minute before its daemon's last line, a gap of more than the first bound with no line, the new
// daemon's starting, boot, restore and start lines, and a take a minute after the view's whole
// line: the number was owed no bound with no take, and the third verdict passes, through the
// machine and its own log and in a log written line by line.
//
// The negative controls: the same gap with the daemon before the start writing lines through it,
// another claim's turn, lists the number from its seen report, since that daemon lived past the
// bound; a log with no starting line, as an older daemon's, is read with its start line as the
// start, and lists the number too; and so does the reading before this change (its parent), which
// took the line it decoded before the restore for one of the daemon before.
func TestTaskCostLogCountsNoGapBetweenTwoDaemonsTowardTheFirstBoundBeforeATakeOrTurn(t *testing.T) {
	t.Run("the machine's own log, no line in the gap", func(t *testing.T) {
		h, log := jsonHarness(t, currentPlugin)
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		h.seen()
		h.advance(time.Minute)
		daemonRestart(h, takeBound+10*time.Minute)
		logDaemonStarted(h)
		h.advance(time.Minute)
		h.connect()
		h.advance(time.Minute)
		h.answerAsk(false)
		h.advance(time.Minute)
		h.mustTake()

		cost := readLogCost(t, h, log)
		if len(cost.Unreached) != 0 || len(cost.TakenNotOncePerOwed) != 0 {
			t.Errorf("%q were owed past the first bound, and the third verdict fails on %q; want neither", cost.Unreached, cost.TakenNotOncePerOwed)
		}
	})
	const claim = "owed-across-a-gap"
	b := costLines{t: t, at: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	starting := b.line(19*time.Minute, DaemonStartingLine, "", "project", "INFRA")
	listed := []string{claim + " task 1: the clock started 2026-11-14T09:00:01.000Z, the bound passed 2026-11-14T09:05:01.000Z, taken 2026-11-14T09:22:00.000Z"}
	for _, tc := range []struct {
		name             string
		gap, start, want []string
	}{
		{name: "no line in the gap", start: []string{starting}},
		{name: "the daemon before the start writing lines through the gap", gap: []string{
			b.turn(10*time.Minute, turnStartedLine, "another-claim"),
			b.turn(11*time.Minute, turnEndedLine, "another-claim"),
		}, start: []string{starting}, want: listed},
		{name: "no starting line, as an older daemon's log", want: listed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{
				b.written(0, claim, 1, 1, TaskValueQueued),
				b.seen(time.Second, claim, 1, 1),
			}
			lines = append(lines, tc.gap...)
			lines = append(lines, tc.start...)
			lines = append(lines,
				b.line(19*time.Minute+time.Second, "daemon: opening the store; a connection error that the next attempt can clear is tried again", ""),
				b.written(19*time.Minute+30*time.Second, "another-claim", 1, 1, TaskValueBoot),
				b.line(20*time.Minute, viewCutLine, claim, "task", 1),
				b.line(20*time.Minute, DaemonStartedLine, "", "project", "INFRA"),
				b.line(21*time.Minute, viewWholeLine, claim, "task", 1),
				b.answered(22*time.Minute, claim, 1, TakeOutcomeAccepted, ""),
			)
			if cost := b.readCost(lines); !slices.Equal(cost.Unreached, tc.want) {
				t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
					strings.Join(cost.Unreached, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// `legion start`'s configuration warnings, which the next daemon's process writes before its
// starting line (cmd/legion's start logs them before daemon.Run, whose first line is the starting
// line), are no line of the daemon before: the reading decodes none of them, so none says that
// daemon lived any longer (alive), and the start judges every clock as of the last line the
// reading decoded before it. A number seen at 09:00:01, no line until a warning at 09:19, the
// starting line, the restore's cut line, the start line, the whole line and a take a minute after
// it: the number was owed no bound with no take.
//
// The negative control is alive taken from every line the reading reads: the warning says the
// daemon before lived to 09:19, and the number is listed as owed past its bound from 09:05:01.
func TestTaskCostLogReadsAWarningBeforeTheStartingLineAsNoLineOfTheDaemonBefore(t *testing.T) {
	const claim = "owed-across-a-gap"
	b := costLines{t: t, at: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	cost := b.readCost([]string{
		b.written(0, claim, 1, 1, TaskValueQueued),
		b.seen(time.Second, claim, 1, 1),
		b.line(19*time.Minute, "setting max_fix_attempts changes nothing: remove max_fix_attempts from legion.yaml", "", "level", "WARN"),
		b.line(19*time.Minute+time.Second, DaemonStartingLine, "", "project", "INFRA"),
		b.line(20*time.Minute, viewCutLine, claim, "task", 1),
		b.line(20*time.Minute, DaemonStartedLine, "", "project", "INFRA"),
		b.line(21*time.Minute, viewWholeLine, claim, "task", 1),
		b.answered(22*time.Minute, claim, 1, TakeOutcomeAccepted, ""),
	})
	if len(cost.Unreached) != 0 || len(cost.TakenNotOncePerOwed) != 0 {
		t.Fatalf("%q were owed past the first bound, and the third verdict fails on %q; want neither", cost.Unreached, cost.TakenNotOncePerOwed)
	}
}

// A view the started daemon made whole before its start line — the shim's hello and its agent's
// answer decided while the boot's last steps ran — keeps the clocks that line started: the
// machine's first wait runs from the answer, and the start line, which the daemon writes after
// its restore, stops nothing of the daemon that wrote it. A task written after the restore and
// reported seen before the hello, the answer, then the start line, and no take: the daemon charges
// the first wait one bound after the answer, and the reading lists the number from the answer.
//
// The negative control is the reading before this change (its parent), whose start line stopped
// that clock and left it standing: the number is not listed.
func TestTaskCostLogKeepsTheTurnClocksAWholeLineBeforeTheStartLineStarted(t *testing.T) {
	h, log := jsonHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	daemonRestart(h, 0)
	h.advance(time.Minute)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	number := h.pending().Number
	h.seen()
	h.advance(time.Minute)
	h.connect()
	h.advance(time.Minute)
	h.answerAsk(false)
	answered := h.clock.Now()
	h.wantArmedFromNow(TimerTake, takeBound, "at the answer")
	logDaemonStarted(h)
	h.advance(takeBound)
	h.wantCharges(WaitTake)

	cost := readLogCost(t, h, log)
	if want := unreachedFrom(h, number, answered); !slices.Equal(cost.Unreached, want) {
		t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
			strings.Join(cost.Unreached, "\n"), strings.Join(want, "\n"))
	}
}
