package supervise

// The cost reading (costreading.go): the documentation site's page and the sample log committed
// beside this package hold each other to the reader, the reader reads what the machine itself
// writes, and each of R9's verdicts lists what fails it.

import (
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// taskCostSample is the sample log the documentation site's page reads, from this package's
// directory (cmd/legion's TestDocExampleTaskCost holds the page to it). It is named .jsonl, the
// daemon's JSON lines, because the repository's .gitignore ignores *.log.
const taskCostSample = "testdata/task-cost.jsonl"

// readSample is the sample log the page's examples read.
func readSample(t *testing.T) []byte {
	t.Helper()
	sample, err := os.ReadFile(taskCostSample)
	if err != nil {
		t.Fatalf("read the sample: %v", err)
	}
	return sample
}

// jsonHarness is waitsHarness with the machine's log written as the daemon writes it — slog's
// JSON handler at its default level (cmd/legion's start) — into log, each line stamped with the
// harness clock's time rather than the wall clock's, so a window reads the walk. set, when it is
// given, changes the machine's deps before it is built.
func jsonHarness(t *testing.T, set ...func(*Deps)) (*harness, *testlog.Buffer) {
	t.Helper()
	h := newBareHarness(t)
	withWaits(&h.deps)
	for _, change := range set {
		change(&h.deps)
	}
	log := &testlog.Buffer{}
	h.deps.Log = slog.New(logRecorder{
		Handler: slog.NewJSONHandler(log, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Time(slog.TimeKey, h.clock.Now())
			}
			return a
		}}),
		records: h.records,
	})
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	return h, log
}

// keysByMessage is, for each message of the JSON lines in log, every attribute key a line of it
// carries.
func keysByMessage(t *testing.T, log string) map[string]map[string]bool {
	t.Helper()
	keys := map[string]map[string]bool{}
	for _, line := range strings.Split(log, "\n") {
		var fields map[string]any
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &fields) != nil {
			continue
		}
		msg, _ := fields["msg"].(string)
		if keys[msg] == nil {
			keys[msg] = map[string]bool{}
		}
		for key := range fields {
			keys[msg][key] = true
		}
	}
	return keys
}

// The reader reads the lines the machine itself writes, not only the sample written to look like
// them: a walk of a claim through a take answered twice, the take's turn, a turn of the worker's
// own, a resume written to a claim that had finished its last task in its session, and a take
// whose turn did not start within the second bound, logged through the daemon's own handler, reads
// back as those things. Every line of the sample carries only keys the machine writes on a line
// of that message.
func TestTaskCostReadsTheMachinesOwnLog(t *testing.T) {
	h, log := jsonHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:1", Row: 1,
		Phase: phase.Implementing, Generation: 1})
	value, _ := h.values.value(testToken)
	if again, err := h.takeValue(value); err != nil || again.Outcome != TakeOutcomeAccepted {
		t.Fatalf("the take sent again = %+v, %v; want it answered accepted from the record", again, err)
	}
	h.advance(30 * time.Second)
	h.must(StreamTurnStart{Claim: testToken})
	h.advance(10 * time.Minute)
	h.must(StreamTurnEnd{Claim: testToken})
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: testToken})
	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	h.advance(time.Minute)
	h.must(RequestDeliver{Claim: testToken, Task: "carry on with the plan", ID: "outbox:2", Row: 2,
		Phase: phase.Implementing, Generation: 1, Origin: OriginResume})
	h.advance(turnStartBound)
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: testToken})
	h.advance(5 * time.Minute)
	h.must(StreamTurnEnd{Claim: testToken})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v, want both tasks finished", p)
	}

	cost, err := ReadTaskCost(strings.NewReader(log.String()), TaskCostArgs{From: epoch.Add(-time.Hour), To: h.clock.Now().Add(time.Second), Take: takeBound})
	if err != nil {
		t.Fatal(err)
	}
	if cost.TurnsStarted != (TurnCauses{Task: 2, None: 1}) || cost.TurnsEnded != (TurnCauses{Task: 2, None: 1}) {
		t.Errorf("turns started %+v and ended %+v, want two by a task number and one with no cause each", cost.TurnsStarted, cost.TurnsEnded)
	}
	if want := []TaskTurns{{string(testToken), 1, 1}, {string(testToken), 2, 1}}; !slices.Equal(cost.TurnsOfTask, want) {
		t.Errorf("turns of each number %+v, want %+v", cost.TurnsOfTask, want)
	}
	if want := map[string]int{OriginPhaseStart: 1, OriginResume: 1}; !maps.Equal(cost.WrittenByOrigin, want) {
		t.Errorf("numbers written by origin %v, want %v", cost.WrittenByOrigin, want)
	}
	if want := map[string]int{OriginResume: 1}; !maps.Equal(cost.WaitedByOrigin, want) {
		t.Errorf("numbers written to a waiting claim by origin %v, want %v", cost.WaitedByOrigin, want)
	}
	if cost.Taken != 2 || cost.Finished != 2 || !maps.Equal(cost.FinishedBy, map[string]int{"report": 2}) {
		t.Errorf("taken %d, finished %d by %v; want two of each, both by the report", cost.Taken, cost.Finished, cost.FinishedBy)
	}
	if !maps.Equal(cost.ChargesByWait, map[string]int{WaitTurnStart: 1}) || cost.Retirements != 0 {
		t.Errorf("charges by wait %v and %d retirements, want the one charge of the second wait", cost.ChargesByWait, cost.Retirements)
	}
	if len(cost.ChargedWithinBound) != 0 || len(cost.TakenNotOncePerOwed) != 0 || len(cost.Unreached) != 0 {
		t.Errorf("the first verdict fails on %q, the third on %q, and %q were owed past the first bound; want neither verdict to fail and no number owed so",
			cost.ChargedWithinBound, cost.TakenNotOncePerOwed, cost.Unreached)
	}
	if len(cost.ResumeToFinished) != 1 || !strings.HasPrefix(cost.ResumeToFinished[0], string(testToken)+" task 2:") {
		t.Errorf("the resume verdict fails on %q, want task 2 alone", cost.ResumeToFinished)
	}

	machine := keysByMessage(t, log.String())
	sample := keysByMessage(t, string(readSample(t)))
	for msg := range costedLines {
		if sample[msg] == nil {
			continue
		}
		if machine[msg] == nil {
			t.Errorf("the walk wrote no %q, so the sample's lines of it are held to nothing", msg)
			continue
		}
		for key := range sample[msg] {
			if !machine[msg][key] {
				t.Errorf("the sample's %q lines carry %q, which the machine never writes on that line", msg, key)
			}
		}
	}
}

// costLogLine is one line of the daemon's log, as slog's JSON handler writes it, for a reading
// built line by line.
func costLogLine(t *testing.T, at time.Time, msg, claim string, attrs ...any) string {
	t.Helper()
	fields := map[string]any{"time": at.Format("2006-01-02T15:04:05.000Z07:00"), "level": "INFO", "msg": msg, "claim": claim}
	for i := 0; i+1 < len(attrs); i += 2 {
		fields[attrs[i].(string)] = attrs[i+1]
	}
	line, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// Each of R9's verdicts fails where the log shows what it forbids, and lists it: a charge of a
// worker whose turn began within the second bound after its take, a resume written to a claim
// that had finished its last task in its session, and a number taken twice for one time owed. A
// repeated answer from the record is no second take, a take-back opens a second time owed, and a
// silence repair's write to a waiting claim is no resume. The charges are counted by wait, "none"
// for a line that names no wait, and a charge at the limit that retired the process is counted
// among the retirements too; a charge made under no take fails no verdict.
func TestTaskCostLogVerdictsListWhatFailsThem(t *testing.T) {
	const claim = "legion-infra-infra-2811-tester"
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	written := func(offset time.Duration, task uint64, origin string, finished bool) string {
		return costLogLine(t, at.Add(offset), valueWrittenLine, claim, "task", task, "origin", origin, "finished", finished, "cause", TaskValueQueued)
	}
	accepted := func(offset time.Duration, task uint64, repeat bool) string {
		return costLogLine(t, at.Add(offset), takeAnsweredLine, claim, "task", task, "outcome", TakeOutcomeAccepted, "hold", "", "repeat", repeat)
	}
	lines := []string{
		// Before any task: a charge that names no wait, as an older daemon's line does, and a
		// retirement at the limit.
		costLogLine(t, at, promptFailureLine, claim),
		costLogLine(t, at, promptRetiredLine, claim, "wait", WaitAdoption),
		// Task 1: an accepted take charged five seconds after it, inside its bound, whose turn
		// began thirty seconds after it.
		written(0, 1, OriginPhaseStart, false),
		accepted(time.Second, 1, false),
		costLogLine(t, at.Add(6*time.Second), promptFailureLine, claim, "wait", WaitTurnStart, "bound", int64(turnStartBound)),
		costLogLine(t, at.Add(6*time.Second), takenBackLine, claim, "task", 1, "cause", WaitTurnStart),
		costLogLine(t, at.Add(31*time.Second), turnStartedLine, claim, "generation", 1),
		costLogLine(t, at.Add(time.Minute), turnEndedLine, claim, "generation", 1),
		// Task 2: written to a claim whose phase waited, as a resume, and taken twice with nothing
		// taken back between, beside a repeated answer.
		written(2*time.Minute, 2, OriginResume, true),
		accepted(2*time.Minute+time.Second, 2, false),
		accepted(2*time.Minute+2*time.Second, 2, true),
		accepted(2*time.Minute+3*time.Second, 2, false),
		// Task 3: the silence repair's write to a waiting claim, taken twice around a take-back.
		written(3*time.Minute, 3, OriginSilence, true),
		accepted(3*time.Minute+time.Second, 3, false),
		costLogLine(t, at.Add(3*time.Minute+2*time.Second), takenBackLine, claim, "task", 3, "cause", "compaction"),
		accepted(3*time.Minute+3*time.Second, 3, false),
	}
	cost, err := ReadTaskCost(strings.NewReader(strings.Join(lines, "\n")+"\n"), TaskCostArgs{From: at, To: at.Add(time.Hour), Take: takeBound})
	if err != nil {
		t.Fatal(err)
	}
	if len(cost.ChargedWithinBound) != 1 || !strings.HasPrefix(cost.ChargedWithinBound[0], claim+" task 1: taken 2026-11-14T09:00:01.000Z, a turn started 2026-11-14T09:00:31.000Z") {
		t.Fatalf("the first verdict fails on %q, want task 1's charge with the turn that began inside its bound", cost.ChargedWithinBound)
	}
	if want := map[string]int{"none": 1, WaitAdoption: 1, WaitTurnStart: 1}; !maps.Equal(cost.ChargesByWait, want) || cost.Retirements != 1 {
		t.Errorf("charges by wait %v and %d retirements, want %v and the one retirement", cost.ChargesByWait, cost.Retirements, want)
	}
	if want := []string{claim + " task 2: written 2026-11-14T09:02:00.000Z"}; !slices.Equal(cost.ResumeToFinished, want) {
		t.Errorf("the resume verdict fails on %q, want %q", cost.ResumeToFinished, want)
	}
	if want := []string{claim + " task 2: taken 2 times, owed 1"}; !slices.Equal(cost.TakenNotOncePerOwed, want) {
		t.Errorf("the third verdict fails on %q, want %q", cost.TakenNotOncePerOwed, want)
	}
	text := cost.Text()
	for _, want := range []string{
		"verdict: no charge of a worker whose turn began within the second bound after its take: fail\n  " + cost.ChargedWithinBound[0] + "\n",
		"verdict: no write of origin resume to a claim that finished its last task in its session: fail\n  " + claim + " task 2:",
		"verdict: each number taken once for each time the record owed it: fail\n  " + claim + " task 2: taken 2 times, owed 1\n",
		"retirements: 1\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the reading prints\n%s\nwhich does not hold %q", text, want)
		}
	}
}

// A take stands only for the task it took. A queue that replaces a task whose turn has started
// retires it with neither a take-back nor a finish line (retirePending), so the newer number's
// write is what ends the older take's time owed. A charge of the first wait on the newer number is
// then made under no take, and the older task's own turn is no turn within its bound.
func TestTaskCostLogEndsATakeWhenANewerNumberIsWritten(t *testing.T) {
	const claim = "legion-infra-infra-2811-tester"
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	lines := []string{
		costLogLine(t, at, valueWrittenLine, claim, "task", 1, "origin", OriginPhaseStart, "finished", false, "cause", TaskValueQueued),
		costLogLine(t, at.Add(time.Second), takeAnsweredLine, claim, "task", 1, "outcome", TakeOutcomeAccepted, "hold", "", "repeat", false),
		costLogLine(t, at.Add(2*time.Second), turnStartedLine, claim, "generation", 1, "task", 1),
		costLogLine(t, at.Add(5*time.Minute), valueWrittenLine, claim, "task", 2, "origin", OriginPhaseStart, "finished", false, "cause", TaskValueQueued),
		costLogLine(t, at.Add(6*time.Minute), promptFailureLine, claim, "wait", WaitTake, "bound", int64(time.Minute)),
	}
	cost, err := ReadTaskCost(strings.NewReader(strings.Join(lines, "\n")+"\n"), TaskCostArgs{From: at, To: at.Add(time.Hour), Take: takeBound})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(cost.ChargesByWait, map[string]int{WaitTake: 1}) {
		t.Fatalf("charges by wait %v, want the one charge of the first wait", cost.ChargesByWait)
	}
	if len(cost.ChargedWithinBound) != 0 {
		t.Errorf("the first verdict fails on %q, want the charge on task 2 held to no take of task 1", cost.ChargedWithinBound)
	}
}

// costLines is the line builders of a reading built line by line from at: any line, a value write
// of a number at a revision, its seen report, the two of an epoch the line names, a take's answer,
// and a turn line. A write and a seen report that name no epoch are an older daemon's lines, which
// the reading matches by revision alone.
type costLines struct {
	t  *testing.T
	at time.Time
}

func (b costLines) line(offset time.Duration, msg, claim string, attrs ...any) string {
	return costLogLine(b.t, b.at.Add(offset), msg, claim, attrs...)
}

func (b costLines) written(offset time.Duration, claim string, task, revision uint64, cause string) string {
	return b.line(offset, valueWrittenLine, claim, "task", task, "origin", OriginPhaseStart, "finished", false,
		"revision", revision, "cause", cause)
}

func (b costLines) seen(offset time.Duration, claim string, task, revision uint64) string {
	return b.line(offset, taskSeenLine, claim, "task", task, "revision", revision)
}

func (b costLines) writtenIn(offset time.Duration, claim string, task uint64, epoch string, revision uint64, cause string) string {
	return b.line(offset, valueWrittenLine, claim, "task", task, "origin", OriginPhaseStart, "finished", false,
		"epoch", epoch, "revision", revision, "cause", cause)
}

func (b costLines) seenIn(offset time.Duration, claim string, task uint64, epoch string, revision uint64) string {
	return b.line(offset, taskSeenLine, claim, "task", task, "epoch", epoch, "revision", revision)
}

func (b costLines) answered(offset time.Duration, claim string, task uint64, outcome TakeOutcome, hold string) string {
	return b.line(offset, takeAnsweredLine, claim, "task", task, "outcome", outcome, "hold", hold, "repeat", false)
}

func (b costLines) turn(offset time.Duration, msg, claim string) string {
	return b.line(offset, msg, claim, "generation", 1)
}

// readCost reads lines as an hour's window from b.at with the default first bound.
func (b costLines) readCost(lines []string) TaskCost {
	b.t.Helper()
	return b.readCostOf("", lines)
}

// readCostOf is readCost for the claim claim alone, or for every claim when it is "".
func (b costLines) readCostOf(claim string, lines []string) TaskCost {
	b.t.Helper()
	cost, err := ReadTaskCost(strings.NewReader(strings.Join(lines, "\n")+"\n"),
		TaskCostArgs{From: b.at, To: b.at.Add(time.Hour), Claim: claim, Take: takeBound})
	if err != nil {
		b.t.Fatal(err)
	}
	return cost
}

// costVerdicts is R9's three verdict lines as the reading prints them, each giving word.
func costVerdicts(word string) string {
	return "verdict: no charge of a worker whose turn began within the second bound after its take: " + word + "\n" +
		"verdict: no write of origin resume to a claim that finished its last task in its session: " + word + "\n" +
		"verdict: each number taken once for each time the record owed it: " + word + "\n"
}

// A number's clock runs only during R5's first wait, and each time owed whose clock passed the
// first bound with no take is listed with its claim, under the figure and under the third
// verdict, and nothing else is (the rule is costreading.go's doc comment), each case on a claim of
// its own. Listed: a number seen and never taken; one seen and taken only after its bound, which
// the late take does not undo, nor a turn that began after the bound; one seen, refused, written
// again at the hold's end and seen again, once, from that seen report; one taken back, written
// again and seen, and never taken again; one seen, whose process died, from its new process's
// seen report after the next registration; one taken, then taken back from a process the daemon
// stopped, with no new write, and never taken by the process relaunched, from that process's seen
// report; and one refused, whose process died before the hold ended, and never taken by the
// process relaunched, which the refusal no longer holds off,
// from that process's seen report. Not listed: a number taken after a turn that ran longer than
// the bound between its seen report and its take; one written, and written again at each bound,
// but never reported seen, which is the unseen rewrite's case; one whose clock started a bound
// before the window's end, so its bound ends there; one taken at its bound; one a newer number
// replaced, or whose value was deleted, within its bound; one whose clock a death stopped until a
// registration it was taken soon after; one a take the daemon could not decide stopped; and one
// seen by a process that died, whose relaunched process never reported it seen, which a new
// process is not charged for (decision 6).
//
// The reading reads the lines before the window for where each claim stands at its start, and
// counts only clock time inside it. Listed: a number written before the window and seen inside
// it, from that seen report; one written and seen before the window and
// never taken, from the window's start, as if seen there; and one whose write fell before the
// log's first line, taken before the log too, taken back in the window from a process the daemon
// stopped and seen by the next process, from that seen report, at the revision it names. Not
// listed: one written and seen long before the window and taken a minute into it, since the clock
// ran a minute inside the window; and one written and seen before the window under a turn that
// began before it and ran twenty minutes into it, taken soon after that turn's end.
//
// The negative controls: with a take-back that opens no new time owed, the stopped process's task
// is not listed; with a registration that leaves a take-back's or a refusal's stop standing,
// neither relaunched process's task is; with a registration that keeps the seen report of the
// process before it, and starts the clock from the registration, the task its relaunched process
// never saw is listed. With the reading cut at the window's start, the number stranded since
// before it is not listed, and with that cut and a seen report that owes nothing where its write
// is not in the log, as the reading was before, neither number written before the window is, nor
// the one written before the log; with only that seen report changed, the number written before
// the log is not listed; with the bound judged before the window, the number under a turn that
// began before it is listed; and with a clock that runs at the window's start counted from its
// own start, the numbers stranded since before the window and taken a minute into it are listed
// from before the window.
func TestTaskCostLogListsEachNumberNoTakeReachedWithinTheFirstBound(t *testing.T) {
	b := costLines{t: t, at: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	lines := []string{
		b.written(0, "seen-and-untaken", 1, 1, TaskValueQueued),
		b.seen(time.Second, "seen-and-untaken", 1, 1),

		b.written(0, "late-taken", 1, 1, TaskValueQueued),
		b.seen(0, "late-taken", 1, 1),
		b.answered(takeBound+time.Second, "late-taken", 1, TakeOutcomeAccepted, ""),

		b.written(0, "passed-before-a-turn", 1, 1, TaskValueQueued),
		b.seen(0, "passed-before-a-turn", 1, 1),
		b.turn(takeBound+time.Minute, turnStartedLine, "passed-before-a-turn"),
		b.turn(takeBound+2*time.Minute, turnEndedLine, "passed-before-a-turn"),
		b.answered(takeBound+3*time.Minute, "passed-before-a-turn", 1, TakeOutcomeAccepted, ""),

		b.written(0, "refused", 1, 1, TaskValueQueued),
		b.seen(time.Second, "refused", 1, 1),
		b.answered(2*time.Second, "refused", 1, TakeOutcomeRefused, HoldIssueInTurn),
		b.written(30*time.Minute, "refused", 1, 2, TaskValueHoldEnded),
		b.seen(30*time.Minute+time.Second, "refused", 1, 2),

		b.written(0, "taken-back", 1, 1, TaskValueQueued),
		b.seen(0, "taken-back", 1, 1),
		b.answered(time.Second, "taken-back", 1, TakeOutcomeAccepted, ""),
		b.line(2*time.Minute, takenBackLine, "taken-back", "task", 1, "cause", WaitTurnStart),
		b.written(2*time.Minute, "taken-back", 1, 2, TaskValueRetake),
		b.seen(2*time.Minute+time.Second, "taken-back", 1, 2),

		b.written(0, "died-and-untaken", 1, 1, TaskValueQueued),
		b.seen(0, "died-and-untaken", 1, 1),
		b.line(time.Minute, processDiedLine, "died-and-untaken", "incarnation", "pod/1", "observed", "gone"),
		b.line(20*time.Minute, agentRegisteredLine, "died-and-untaken", "generation", 2, "session", "ses_2"),
		b.seen(20*time.Minute+time.Second, "died-and-untaken", 1, 1),

		b.written(0, "taken-back-from-a-stopped-process", 1, 1, TaskValueQueued),
		b.seen(0, "taken-back-from-a-stopped-process", 1, 1),
		b.answered(time.Second, "taken-back-from-a-stopped-process", 1, TakeOutcomeAccepted, ""),
		b.line(2*time.Minute, takenBackLine, "taken-back-from-a-stopped-process", "task", 1, "cause", takenBackRetired),
		b.line(3*time.Minute, agentRegisteredLine, "taken-back-from-a-stopped-process", "generation", 2, "session", "ses_2"),
		b.seen(3*time.Minute+time.Second, "taken-back-from-a-stopped-process", 1, 1),

		b.written(0, "refused-and-relaunched", 1, 1, TaskValueQueued),
		b.seen(time.Second, "refused-and-relaunched", 1, 1),
		b.answered(2*time.Second, "refused-and-relaunched", 1, TakeOutcomeRefused, HoldIssueInTurn),
		b.line(4*time.Minute, processDiedLine, "refused-and-relaunched", "incarnation", "pod/1", "observed", "gone"),
		b.line(5*time.Minute, agentRegisteredLine, "refused-and-relaunched", "generation", 2, "session", "ses_2"),
		b.seen(5*time.Minute+time.Second, "refused-and-relaunched", 1, 1),

		b.written(0, "relaunched-and-unseen", 1, 1, TaskValueQueued),
		b.seen(0, "relaunched-and-unseen", 1, 1),
		b.line(time.Minute, processDiedLine, "relaunched-and-unseen", "incarnation", "pod/1", "observed", "gone"),
		b.line(2*time.Minute, agentRegisteredLine, "relaunched-and-unseen", "generation", 2, "session", "ses_2"),

		b.written(0, "taken-after-a-turn", 1, 1, TaskValueQueued),
		b.seen(time.Second, "taken-after-a-turn", 1, 1),
		b.turn(30*time.Second, turnStartedLine, "taken-after-a-turn"),
		b.turn(20*time.Minute, turnEndedLine, "taken-after-a-turn"),
		b.answered(24*time.Minute, "taken-after-a-turn", 1, TakeOutcomeAccepted, ""),

		b.written(0, "never-seen", 1, 1, TaskValueQueued),
		b.written(takeBound, "never-seen", 1, 2, TaskValueUnseen),
		b.written(2*takeBound, "never-seen", 1, 3, TaskValueUnseen),

		b.written(time.Hour-takeBound, "seen-a-bound-before-the-end", 1, 1, TaskValueQueued),
		b.seen(time.Hour-takeBound, "seen-a-bound-before-the-end", 1, 1),

		b.written(0, "taken-at-its-bound", 1, 1, TaskValueQueued),
		b.seen(0, "taken-at-its-bound", 1, 1),
		b.answered(takeBound, "taken-at-its-bound", 1, TakeOutcomeAccepted, ""),

		b.written(0, "replaced", 1, 1, TaskValueQueued),
		b.seen(0, "replaced", 1, 1),
		b.written(time.Minute, "replaced", 2, 2, TaskValueQueued),
		b.seen(time.Minute, "replaced", 2, 2),
		b.answered(2*time.Minute, "replaced", 2, TakeOutcomeAccepted, ""),

		b.written(0, "deleted", 1, 1, TaskValueQueued),
		b.seen(0, "deleted", 1, 1),
		b.line(time.Minute, valueDeletedLine, "deleted", "task", 1, "cause", valueDeletedDropped),

		b.written(0, "died", 1, 1, TaskValueQueued),
		b.seen(0, "died", 1, 1),
		b.line(time.Minute, processDiedLine, "died", "incarnation", "pod/1", "observed", "gone"),
		b.line(20*time.Minute, agentRegisteredLine, "died", "generation", 2, "session", "ses_2"),
		b.seen(20*time.Minute+time.Second, "died", 1, 1),
		b.answered(23*time.Minute, "died", 1, TakeOutcomeAccepted, ""),

		b.written(0, "undecided", 1, 1, TaskValueQueued),
		b.seen(0, "undecided", 1, 1),
		b.line(time.Minute, takeUndecidedLine, "undecided", "task", 1, "revision", 1, "error", "the store did not answer"),

		b.written(-10*time.Minute, "written-before-the-window", 1, 1, TaskValueQueued),
		b.seen(time.Minute, "written-before-the-window", 1, 1),

		b.written(-20*time.Minute, "stranded-before-the-window", 1, 1, TaskValueQueued),
		b.seen(-20*time.Minute+time.Second, "stranded-before-the-window", 1, 1),

		b.line(time.Minute, takenBackLine, "written-before-the-log", "task", 1, "cause", takenBackRetired),
		b.line(2*time.Minute, agentRegisteredLine, "written-before-the-log", "generation", 2, "session", "ses_2"),
		b.seenIn(2*time.Minute+time.Second, "written-before-the-log", 1, "2026-11-14T08:00:00.123456789Z", 7),

		b.written(-20*time.Minute, "taken-a-minute-into-the-window", 1, 1, TaskValueQueued),
		b.seen(-20*time.Minute, "taken-a-minute-into-the-window", 1, 1),
		b.answered(time.Minute, "taken-a-minute-into-the-window", 1, TakeOutcomeAccepted, ""),

		b.written(-30*time.Minute, "in-a-turn-at-the-window's-start", 1, 1, TaskValueQueued),
		b.seen(-30*time.Minute, "in-a-turn-at-the-window's-start", 1, 1),
		b.turn(-20*time.Minute, turnStartedLine, "in-a-turn-at-the-window's-start"),
		b.turn(20*time.Minute, turnEndedLine, "in-a-turn-at-the-window's-start"),
		b.answered(22*time.Minute, "in-a-turn-at-the-window's-start", 1, TakeOutcomeAccepted, ""),
	}
	cost := b.readCost(lines)
	want := []string{
		"died-and-untaken task 1: the clock started 2026-11-14T09:20:01.000Z, the bound passed 2026-11-14T09:25:01.000Z, still owed at the window's end",
		"late-taken task 1: the clock started 2026-11-14T09:00:00.000Z, the bound passed 2026-11-14T09:05:00.000Z, taken 2026-11-14T09:05:01.000Z",
		"passed-before-a-turn task 1: the clock started 2026-11-14T09:00:00.000Z, the bound passed 2026-11-14T09:05:00.000Z, taken 2026-11-14T09:08:00.000Z",
		"refused task 1: the clock started 2026-11-14T09:30:01.000Z, the bound passed 2026-11-14T09:35:01.000Z, still owed at the window's end",
		"refused-and-relaunched task 1: the clock started 2026-11-14T09:05:01.000Z, the bound passed 2026-11-14T09:10:01.000Z, still owed at the window's end",
		"seen-and-untaken task 1: the clock started 2026-11-14T09:00:01.000Z, the bound passed 2026-11-14T09:05:01.000Z, still owed at the window's end",
		"stranded-before-the-window task 1: the clock started 2026-11-14T09:00:00.000Z, the bound passed 2026-11-14T09:05:00.000Z, still owed at the window's end",
		"taken-back task 1: the clock started 2026-11-14T09:02:01.000Z, the bound passed 2026-11-14T09:07:01.000Z, still owed at the window's end",
		"taken-back-from-a-stopped-process task 1: the clock started 2026-11-14T09:03:01.000Z, the bound passed 2026-11-14T09:08:01.000Z, still owed at the window's end",
		"written-before-the-log task 1: the clock started 2026-11-14T09:02:01.000Z, the bound passed 2026-11-14T09:07:01.000Z, still owed at the window's end",
		"written-before-the-window task 1: the clock started 2026-11-14T09:01:00.000Z, the bound passed 2026-11-14T09:06:00.000Z, still owed at the window's end",
	}
	if !slices.Equal(cost.Unreached, want) {
		t.Fatalf("the numbers owed that no take reached within the first bound are\n%s\nwant\n%s",
			strings.Join(cost.Unreached, "\n"), strings.Join(want, "\n"))
	}
	var verdict []string
	for _, owed := range want {
		claim, _, _ := strings.Cut(owed, " task 1: ")
		_, passed, _ := strings.Cut(owed, "the bound passed ")
		passed, _, _ = strings.Cut(passed, ",")
		verdict = append(verdict, claim+" task 1: owed, and no take within the first bound, which passed "+passed)
	}
	if !slices.Equal(cost.TakenNotOncePerOwed, verdict) {
		t.Fatalf("the third verdict fails on\n%s\nwant\n%s", strings.Join(cost.TakenNotOncePerOwed, "\n"), strings.Join(verdict, "\n"))
	}
	text := cost.Text()
	for _, printed := range []string{
		"first bound 5m0s\n",
		"numbers taken: 10\nnumbers owed that no take reached within the first bound: 11\n  " + strings.Join(want, "\n  ") + "\nnumbers finished",
		"verdict: each number taken once for each time the record owed it: fail\n  " + strings.Join(verdict, "\n  ") + "\n",
	} {
		if !strings.Contains(text, printed) {
			t.Errorf("the reading prints\n%s\nwhich does not hold %q", text, printed)
		}
	}
}

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
			if p := h.pending(); !p.awaitsTurn() {
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

			cost, err := ReadTaskCost(strings.NewReader(log.String()), TaskCostArgs{From: epoch.Add(-time.Hour), To: h.clock.Now().Add(time.Second), Take: takeBound})
			if err != nil {
				t.Fatal(err)
			}
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

// A window that holds no daemon line, of the claim asked for, is evidence of nothing: an empty
// log, a window the log does not reach, a claim no line of it names, and a window after the log's
// last line that a number stranded before it runs through, which the reading lists as owed past
// its first bound: with no line in the window, nothing shows the daemon lived through it, so not
// even that fail is judged. The reading says it does not judge every verdict (EveryVerdictJudged)
// and prints each of R9's three verdicts as "no evidence", none as a pass, so check c9 cannot read
// a reading of nothing as three passes.
//
// The negative controls: the same window with one line of the claim in it has its verdicts judged,
// and each passes; and with judges taking a fail carried into a window with no line for evidence,
// the window after the log's last line fails its third verdict.
func TestTaskCostLogWithNoLineInTheWindowIsNoEvidence(t *testing.T) {
	const claim = "legion-infra-infra-2811-tester"
	b := costLines{t: t, at: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	line := b.written(time.Minute, claim, 1, 1, TaskValueQueued) + "\n"
	stranded := b.written(-10*time.Minute, claim, 1, 1, TaskValueQueued) + "\n" + b.seen(-10*time.Minute, claim, 1, 1) + "\n"
	window := TaskCostArgs{From: b.at, To: b.at.Add(time.Hour), Take: takeBound}
	dayBefore, otherClaim := window, window
	dayBefore.From, dayBefore.To = window.From.Add(-24*time.Hour), window.To.Add(-24*time.Hour)
	otherClaim.Claim = "legion-infra-infra-2811-implementer"
	read := func(log string, args TaskCostArgs) TaskCost {
		t.Helper()
		cost, err := ReadTaskCost(strings.NewReader(log), args)
		if err != nil {
			t.Fatal(err)
		}
		return cost
	}
	for _, tc := range []struct {
		name, log string
		args      TaskCostArgs
	}{
		{"an empty log", "", window},
		{"a window the log does not reach", line, dayBefore},
		{"a claim no line names", line, otherClaim},
		{"a window after the log's last line, with a number stranded before it", stranded, window},
	} {
		cost := read(tc.log, tc.args)
		text := cost.Text()
		if cost.EveryVerdictJudged() || !strings.Contains(text, "daemon lines in the window: 0\n") || !strings.HasSuffix(text, costVerdicts("no evidence")) {
			t.Errorf("%s: the reading judges every verdict %v and prints\n%s\nwant no verdict judged, no line, and each verdict %q", tc.name, cost.EveryVerdictJudged(), text, "no evidence")
		}
	}
	cost := read(line, window)
	if text := cost.Text(); !cost.EveryVerdictJudged() || !strings.HasSuffix(text, costVerdicts("pass")) {
		t.Errorf("one line in the window: the reading judges every verdict %v and prints\n%s\nwant its verdicts judged, each a pass", cost.EveryVerdictJudged(), text)
	}
}

// A window whose lines carry no task value written and no take holds no evidence for any of R9's
// three verdicts, which each read the takes and the value writes: the log of a daemon before this
// change, whose every turn names no cause and whose charges are the push path's ("acknowledged, and
// no turn started within 5s"), is such a window, and so is a daemon that launches no worker. Its
// reading must not print the verdicts as passes: check c9 read over the daemon's previous
// container (`kubectl logs --previous`) or over a daemon not yet rolled out would pass the very
// fleet the change was made to fix, with three charges and a retirement in it. A reading of
// nothing is the class review finding 13 named (TestTaskCostLogWithNoLineInTheWindowIsNoEvidence
// holds the empty window alone).
//
// The negative control is TestTaskCostLogWithNoLineInTheWindowIsNoEvidence's: a window holding a
// value write has its verdicts judged, each a pass.
func TestTaskCostLogWithNoTaskValueAndNoTakePrintsNoPass(t *testing.T) {
	const claim = "legion-infra-infra-3085-architect"
	const why = "acknowledged, and no turn started within 5s"
	b := costLines{t: t, at: time.Date(2026, 11, 12, 11, 28, 52, 0, time.UTC)}
	cost := b.readCost([]string{
		b.line(time.Second, DaemonStartedLine, ""),
		b.turn(time.Minute, turnStartedLine, claim),
		b.line(time.Minute+5*time.Second, promptFailureLine, claim, "why", why, "promptFailures", 1, "limit", 3),
		b.turn(time.Minute+35*time.Second, turnEndedLine, claim),
		b.turn(2*time.Minute, turnStartedLine, claim),
		b.line(2*time.Minute+5*time.Second, promptFailureLine, claim, "why", why, "promptFailures", 2, "limit", 3),
		b.turn(2*time.Minute+35*time.Second, turnEndedLine, claim),
		b.line(3*time.Minute+5*time.Second, promptRetiredLine, claim, "why", why, "promptFailures", 3, "relaunchCycle", 1, "limit", 2),
	})
	if text := cost.Text(); strings.Contains(text, ": pass\n") {
		t.Fatalf("a window with %d daemon lines, charges by wait %v and %d retirement, and no task value written and no take, prints\n%s\nwant no verdict printed as a pass: no take and no value write is no evidence for any of the three",
			cost.Lines, cost.ChargesByWait, cost.Retirements, text)
	}
}

// A window holds the take model's own evidence once it holds one task value written or one take
// answered, of the claim asked for, and judges each verdict then; without it, a fail the reading
// proves from the state carried into the window is evidence for its own verdict alone, and each
// verdict no such fail fails holds none, however many daemon lines, one or more, the window holds
// (TaskCost.judges). No evidence for any verdict: the log of a daemon from before the task value, a
// turn of no cause and a charge of the push path; a window whose value write and take of the claim
// fell before it, with only that take's turn in it; under --claim, a claim whose lines hold no
// write and no take while another claim's do; a number written and seen before the window, whose
// first bound passed before it, with only a turn of no cause in it, which stops the clock before a
// bound passes inside it; and a charge of a worker whose turn began within the second bound after
// its take, all before the window, with only that turn's end in it. A bound that passed, or a
// charge made, before From is no evidence. Each prints its daemon lines as it read them, then the
// line that says it judges no verdict, and each verdict as "no evidence". Judged by a carried fail
// alone, since the reading never hides a fail it can prove and never prints a pass it has no
// evidence for, each printing its daemon lines, then the line that says only such a fail is judged,
// the verdict that fail fails, and the other two as "no evidence": a charge inside the window of a
// worker whose turn began within the second bound after a take accepted before the window, which
// fails the first verdict; and the page's own sample read for the planner of INFRA-2812 up to
// 09:05:30, whose task 1 was written and seen before the window, charged inside it, and taken only
// after it (the review's repro), which fails the third. Judged whole, each printing no such line: a
// window whose one write or take of the claim is a take answered accepted, refused or dropped, of a
// number written before the window, or a value write, and the claim whose lines hold a write and a
// take, whose verdicts all pass; and the same sample read for the planner up to 10:00, whose take
// of task 1 inside the window judges the two verdicts its stranded task does not fail, each a pass.
//
// The negative controls: with the take model's own evidence back to a window that holds a daemon
// line, the windows with no evidence print their verdicts as passes; with own evidence that reads
// the value writes alone, the three windows judged by one take answer print "no evidence"; with no
// verdict judged by a carried fail, the two windows judged by one print three "no evidence"; with
// a carried fail taken for evidence of all three verdicts, those two print the other two as passes;
// and with a clock that runs at From left running from its seen report (enter), or a charge before
// the window counted, the window whose bound passed, or whose charge was made, before it is judged.
func TestTaskCostLogJudgesAWindowThatHoldsAValueWriteOrATakeAnswer(t *testing.T) {
	const claim, other = "legion-infra-infra-2811-tester", "legion-infra-infra-2811-implementer"
	const zero = "task values written and takes answered in the window: 0, so "
	b := costLines{t: t, at: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	answered := func(outcome TakeOutcome) []string {
		return []string{
			b.written(-2*time.Minute, claim, 1, 1, TaskValueQueued),
			b.seen(-time.Minute, claim, 1, 1),
			b.answered(time.Minute, claim, 1, outcome, ""),
		}
	}
	twoClaims := []string{
		b.written(time.Minute, claim, 1, 1, TaskValueQueued),
		b.seen(2*time.Minute, claim, 1, 1),
		b.answered(3*time.Minute, claim, 1, TakeOutcomeAccepted, ""),
		b.turn(4*time.Minute, turnStartedLine, other),
		b.turn(5*time.Minute, turnEndedLine, other),
	}
	takenBefore := []string{
		b.written(-3*time.Minute, claim, 1, 1, TaskValueQueued),
		b.seen(-2*time.Minute, claim, 1, 1),
		b.answered(-time.Minute, claim, 1, TakeOutcomeAccepted, ""),
	}
	sample := readSample(t)
	for _, tc := range []struct {
		name  string
		lines []string
		claim string
		// page is a window of the page's sample, which the row reads in place of lines.
		page *TaskCostArgs
		// counted is the count of the daemon lines a window without the take model's own evidence
		// prints, "" for a window that holds it.
		counted string
		// fails is the one verdict a window fails, with what fails it, "" for a window that fails
		// none: by a fail carried into it alone where the window holds no evidence of its own, whose
		// other two verdicts then read "no evidence", and with the other two a pass where it does.
		fails string
	}{
		{name: "a daemon from before the task value", lines: []string{
			b.line(time.Second, DaemonStartedLine, ""),
			b.turn(time.Minute, turnStartedLine, claim),
			b.line(time.Minute+5*time.Second, promptFailureLine, claim, "why", "acknowledged, and no turn started within 5s", "promptFailures", 1, "limit", 3),
			b.turn(time.Minute+35*time.Second, turnEndedLine, claim),
		}, counted: "daemon lines in the window: 4, the first at 2026-11-14T09:00:01.000Z, the last at 2026-11-14T09:01:35.000Z\n"},
		{name: "a write and a take before the window", lines: append(slices.Clone(takenBefore),
			b.line(time.Minute, turnStartedLine, claim, "generation", 1, "task", 1),
		), counted: "daemon lines in the window: 1, the first at 2026-11-14T09:01:00.000Z, the last at 2026-11-14T09:01:00.000Z\n"},
		{name: "a claim with no write and no take", lines: twoClaims, claim: other,
			counted: "daemon lines in the window: 2, the first at 2026-11-14T09:04:00.000Z, the last at 2026-11-14T09:05:00.000Z\n"},
		{name: "a bound that passed before the window", lines: []string{
			b.written(-10*time.Minute, claim, 1, 1, TaskValueQueued),
			b.seen(-10*time.Minute, claim, 1, 1),
			b.turn(time.Minute, turnStartedLine, claim),
		}, counted: "daemon lines in the window: 1, the first at 2026-11-14T09:01:00.000Z, the last at 2026-11-14T09:01:00.000Z\n"},
		{name: "a charge before the window", lines: []string{
			b.written(-4*time.Minute, claim, 1, 1, TaskValueQueued),
			b.seen(-3*time.Minute, claim, 1, 1),
			b.answered(-2*time.Minute, claim, 1, TakeOutcomeAccepted, ""),
			b.line(-time.Minute, turnStartedLine, claim, "generation", 1, "task", 1),
			b.line(-30*time.Second, promptFailureLine, claim, "wait", WaitTurnStart, "bound", int64(turnStartBound)),
			b.line(time.Minute, turnEndedLine, claim, "generation", 1, "task", 1),
		}, counted: "daemon lines in the window: 1, the first at 2026-11-14T09:01:00.000Z, the last at 2026-11-14T09:01:00.000Z\n"},
		{name: "a take answered accepted", lines: answered(TakeOutcomeAccepted)},
		{name: "a take answered refused", lines: answered(TakeOutcomeRefused)},
		{name: "a take answered dropped", lines: answered(TakeOutcomeDropped)},
		{name: "a value write", lines: []string{b.written(time.Minute, claim, 1, 1, TaskValueQueued)}},
		{name: "a claim with a write and a take", lines: twoClaims, claim: claim},
		{name: "a charge within the second bound of a take before the window", lines: append(slices.Clone(takenBefore),
			b.line(time.Minute, turnStartedLine, claim, "generation", 1, "task", 1),
			b.line(2*time.Minute, promptFailureLine, claim, "wait", WaitTurnStart, "bound", int64(turnStartBound)),
		), counted: "daemon lines in the window: 2, the first at 2026-11-14T09:01:00.000Z, the last at 2026-11-14T09:02:00.000Z\n",
			fails: "verdict: no charge of a worker whose turn began within the second bound after its take: fail\n  " + claim +
				" task 1: taken 2026-11-14T08:59:00.000Z, a turn started 2026-11-14T09:01:00.000Z, charged 2026-11-14T09:02:00.000Z (wait turn-start, bound 25m0s)\n"},
		{name: "a number stranded since before the window, in the page's sample", page: &TaskCostArgs{
			From: b.at, To: b.at.Add(5*time.Minute + 30*time.Second), Claim: "legion-infra-infra-2812-planner", Take: takeBound,
		}, counted: "daemon lines in the window: 1, the first at 2026-11-14T09:03:30.500Z, the last at 2026-11-14T09:03:30.500Z\n",
			fails: "verdict: each number taken once for each time the record owed it: fail\n" +
				"  legion-infra-infra-2812-planner task 1: owed, and no take within the first bound, which passed 2026-11-14T09:05:00.000Z\n"},
		{name: "a number stranded since before the window and taken inside it, in the page's sample", page: &TaskCostArgs{
			From: b.at, To: b.at.Add(time.Hour), Claim: "legion-infra-infra-2812-planner", Take: takeBound,
		}, fails: "verdict: each number taken once for each time the record owed it: fail\n" +
			"  legion-infra-infra-2812-planner task 1: owed, and no take within the first bound, which passed 2026-11-14T09:05:00.000Z\n"},
	} {
		var cost TaskCost
		if tc.page != nil {
			read, err := ReadTaskCost(strings.NewReader(string(sample)), *tc.page)
			if err != nil {
				t.Fatal(err)
			}
			cost = read
		} else {
			cost = b.readCostOf(tc.claim, tc.lines)
		}
		text := cost.Text()
		// under is the count of the window's lines and the line under it, "" for a window that
		// holds its own evidence; judged is what EveryVerdictJudged answers; and others is what
		// each verdict the window's fails do not fail reads.
		judged, others, under := true, "pass", ""
		if tc.counted != "" {
			so := "no verdict is judged\n"
			if tc.fails != "" {
				so = "only a fail carried into the window is judged\n"
			}
			judged, others, under = false, "no evidence", tc.counted+zero+so
		}
		verdicts := strings.HasSuffix(text, costVerdicts(others))
		if tc.fails != "" {
			verdicts = strings.Contains(text, tc.fails) && strings.Count(text, ": "+others+"\n") == 2
		}
		if cost.EveryVerdictJudged() != judged || !verdicts || !strings.Contains(text, under) || strings.Contains(text, zero) != (under != "") {
			t.Errorf("%s: the reading judges every verdict %v and prints\n%s\nwant every verdict judged %v, %q, each verdict it does not fail %q, and\n%s",
				tc.name, cost.EveryVerdictJudged(), text, judged, under, others, tc.fails)
		}
	}
}
