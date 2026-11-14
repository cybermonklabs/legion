package supervise

// The lines R7 of dispatch://INFRA-2811 asks for (task T14 of its plan): every turn line names its
// cause, every value write names its number, its claim, its task's origin and whether the claim
// had finished the last task written to it, the take's answer, its take-back and its finish are
// each a line, and every message an operator and the cost reading search for is pinned.

import (
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The task lines as an operator, the production check and the cost reading read them: whole, and
// not as the constants that carry them, for the reason startedLine and endedLine are spelled out
// (turnlog_test.go).
const (
	writtenLine       = "supervise: a task value was written"
	deletedLine       = "supervise: a task value was deleted"
	answeredLine      = "supervise: a take was answered"
	takenBackLiteral  = "supervise: a task was taken back"
	finishedLine      = "supervise: a task was finished"
	promptFailed      = "supervise: prompt failure"
	retiredAfterLimit = "supervise: retired after prompt failures"
)

// wantAttr fails the test unless r carries key with the value want, compared as the text handler
// would print it.
func wantAttr(t *testing.T, r loggedRecord, key, want string) {
	t.Helper()
	got, ok := r.attr(key)
	if !ok || got.String() != want {
		t.Fatalf("%q carries %s = %v (present %v), want %s", r.message, key, got, ok, want)
	}
}

// A turn line names its cause: the number of the accepted take the turn started for, or none at
// all. The take's turn names its task when it starts and when it ends; a turn after it that no
// take started — an Envoy notice, the worker's own — carries no task key on either line, and no
// turn line names a delivery any more.
func TestTurnLineNamesTheTakesNumberOrNoCause(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	number := h.pending().Number

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v, want the take's task finished by its report", p)
	}
	h.must(StreamTurnStart{Claim: testToken})
	h.mustQuiet(StreamTurnEnd{Claim: testToken})

	for _, line := range []string{startedLine, endedLine} {
		kept := turnLines(h, line)
		if len(kept) != 2 {
			t.Fatalf("%q was written %d times, want twice", line, len(kept))
		}
		for i, want := range []uint64{number, 0} {
			r := kept[i]
			wantAttr(t, r, "claim", string(testToken))
			if _, named := r.attr("delivery"); named {
				t.Fatalf("turn %d's %q names a delivery, want the task number alone", i+1, line)
			}
			got, named := r.attr("task")
			switch {
			case want == 0 && named:
				t.Fatalf("turn %d's %q names task %v, want no cause: no take started it", i+1, line, got)
			case want != 0 && (!named || got.Uint64() != want):
				t.Fatalf("turn %d's %q names task %v (present %v), want the take's %d", i+1, line, got, named, want)
			}
		}
	}
}

// Each value write is logged with its number, its claim, the origin of its task, whether the claim
// had finished the last task written to it in the session it runs, that session, the write's
// epoch and revision, and its cause. A claim's first task is written to a claim that had finished
// nothing; the next one, once the first is finished in the same session, to a claim whose phase
// waited.
func TestTaskValueLogNamesNumberClaimOriginAndFinished(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:1", Row: 1,
		Phase: phase.Implementing, Generation: 1})
	first := h.pending().Number
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	h.must(RequestDeliver{Claim: testToken, Task: "carry on with the plan", ID: "outbox:2", Row: 2,
		Phase: phase.Implementing, Generation: 1, Origin: OriginResume})
	second := h.pending().Number

	writes := h.records.of(writtenLine)
	if len(writes) != 2 {
		t.Fatalf("logged %d value writes, want one for each task's queue", len(writes))
	}
	for i, want := range []struct {
		task     uint64
		origin   string
		finished string
	}{{first, OriginPhaseStart, "false"}, {second, OriginResume, "true"}} {
		r := writes[i]
		if r.level != slog.LevelInfo {
			t.Fatalf("write %d is logged at %s, want INFO", i+1, r.level)
		}
		wantAttr(t, r, "claim", string(testToken))
		wantAttr(t, r, "task", slog.Uint64Value(want.task).String())
		wantAttr(t, r, "origin", want.origin)
		wantAttr(t, r, "finished", want.finished)
		wantAttr(t, r, "session", session)
		wantAttr(t, r, "epoch", testEpoch)
		wantAttr(t, r, "cause", TaskValueQueued)
		if revision, ok := r.attr("revision"); !ok || revision.Uint64() == 0 {
			t.Fatalf("write %d carries revision %v (present %v), want the bucket's", i+1, revision, ok)
		}
	}

	// What finished means, of the claim as the write finds it.
	for _, tc := range []struct {
		name  string
		claim Claim
		want  bool
	}{
		{"no task written yet", Claim{Session: session}, false},
		{"its last task finished in this session", Claim{Session: session, TaskWritten: 3, TaskFinished: 3, TaskFinishedSession: session}, true},
		{"its last task not finished", Claim{Session: session, TaskWritten: 3, TaskFinished: 2, TaskFinishedSession: session}, false},
		{"its last task finished in a session it left", Claim{Session: "ses_fresh", TaskWritten: 3, TaskFinished: 3, TaskFinishedSession: session}, false},
	} {
		if got := tc.claim.finishedLastTask(); got != tc.want {
			t.Errorf("%s: finished = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The silence repair's start (workflow's repairSilence) names its own origin in both of its
// branches, and the write of its task says so: origin silence, never resume, even where the task
// is the phase's resume task and the claim had finished its last task in its session — the worker
// the repair exists for — so the cost reading counts it apart from a resume's.
func TestTaskValueLogOfTheSilenceRepairNamesOriginSilence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request RequestDeliver
	}{
		{"the claim level with its entry, sent the phase's resume task", RequestDeliver{Claim: testToken, ID: "outbox:2", Row: 2,
			Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing.", Phase: phase.Implementing, Generation: 1,
			Origin: OriginSilence}},
		{"the claim behind its entry, sent that entry's work", RequestDeliver{Claim: testToken, ID: "outbox:3", Row: 3,
			Task: "implement the plan's second part", Phase: phase.Implementing, Generation: 1, Entry: 2,
			Origin: OriginSilence}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:1", Row: 1,
				Phase: phase.Implementing, Generation: 1, Entry: 1})
			h.must(StreamTurnStart{Claim: testToken})
			h.must(StreamTurnEnd{Claim: testToken})

			h.must(tc.request)

			writes := h.records.of(writtenLine)
			if len(writes) != 2 {
				t.Fatalf("logged %d value writes, want the first task's and the repair's", len(writes))
			}
			repair := writes[1]
			wantAttr(t, repair, "claim", string(testToken))
			wantAttr(t, repair, "task", slog.Uint64Value(h.pending().Number).String())
			wantAttr(t, repair, "origin", OriginSilence)
			wantAttr(t, repair, "finished", "true")
		})
	}
}

// The messages an operator, the production check and the cost reading search for are the ones the
// machine writes, word for word, and a reword of any of them fails here before it silences every
// search: the constants are pinned to the words, and one walk of a claim writes each of them under
// those words.
func TestTurnLogPinnedMessagesAreUnchanged(t *testing.T) {
	for constant, words := range map[string]string{
		turnStartedLine:   startedLine,
		turnEndedLine:     endedLine,
		valueWrittenLine:  writtenLine,
		valueDeletedLine:  deletedLine,
		takeAnsweredLine:  answeredLine,
		takenBackLine:     takenBackLiteral,
		taskFinishedLine:  finishedLine,
		promptFailureLine: promptFailed,
		promptRetiredLine: retiredAfterLimit,
	} {
		if constant != words {
			t.Errorf("the pinned message %q reads %q", words, constant)
		}
		if words != valueDeletedLine && !costedLines[words] {
			t.Errorf("the cost reading does not read %q", words)
		}
	}

	h := waitsHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	h.must(RequestDeliver{Claim: testToken, Task: "carry on"})
	for range testLimits().PromptFailures {
		h.advance(turnStartBound)
	}
	for _, words := range []string{startedLine, endedLine, writtenLine, deletedLine, answeredLine, takenBackLiteral,
		finishedLine, promptFailed, retiredAfterLimit} {
		if len(h.records.of(words)) == 0 {
			t.Errorf("the walk wrote no %q", words)
		}
	}
	// The turn-outcome report writes the end message too, with a sequence of its own, which the
	// cost reading passes over: the words are the same constant's.
	reports := 0
	for _, r := range h.records.of(endedLine) {
		if _, ok := r.attr("sequence"); ok {
			reports++
		}
	}
	if reports != 1 {
		t.Errorf("the report of the turn wrote %d %q lines with a sequence, want one", reports, endedLine)
	}
}

// Both charge lines name the wait the charge is for, beside the bound that wait waited: the first
// wait's and the second's, and none for a refused adoption, which is charged at the shim's answer.
func TestChargeLogNamesItsWaitAndBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		walk  func(h *harness)
		wait  string
		bound time.Duration
	}{
		{"a seen value taken by nobody", func(h *harness) {
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			h.seen()
			h.advance(takeBound)
		}, WaitTake, takeBound},
		{"an accepted take whose turn never starts, to the retirement", func(h *harness) {
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			for range testLimits().PromptFailures {
				h.advance(turnStartBound)
			}
		}, WaitTurnStart, turnStartBound},
		{"a refused adoption", func(h *harness) {
			h.reach(StateReady)
			refuseAdoptions(h)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing, Generation: 7})
		}, WaitAdoption, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := waitsHarness(t)
			tc.walk(h)
			charges := append(h.records.of(promptFailed), h.records.of(retiredAfterLimit)...)
			if len(charges) == 0 {
				t.Fatal("the walk charged nothing")
			}
			for _, r := range charges {
				wantAttr(t, r, "claim", string(testToken))
				wantAttr(t, r, "wait", tc.wait)
				wantAttr(t, r, "bound", tc.bound.String())
			}
		})
	}
}

// A turn-outcome report can name the accepted take's number before the stream's start of that
// take's turn reaches the daemon: the report comes over HTTP and the start over the worker
// stream. The turn ran all the same, so the report runs the start's effects for the take first —
// the turn is the take's, the prompt budgets go to zero — and then finishes the task, which earns
// the claim its serving credit: the run and the entry the task carried are the claim's, and the
// entry is not handed back as owed. The start frame that arrives after is that turn's: its line
// and its end's name the task, and it starts no second turn of it.
func TestTurnOutcomeBeforeItsTurnStartStillServesTheTask(t *testing.T) {
	h := waitsHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:7", Row: 7,
		Phase: phase.Implementing, Generation: 3, Entry: 2})
	h.seen()
	h.advance(takeBound)
	h.wantBudgets(Budgets{PromptFailures: 1})
	h.mustTake()
	number := h.pending().Number

	if err := h.reportTurn(number, nil); err != nil {
		t.Fatalf("report the turn of task %d: %v", number, err)
	}

	c := h.claim()
	if c.Pending != nil {
		t.Fatalf("pending %+v, want the task finished by its report", c.Pending)
	}
	if c.TaskFinished != number || c.TaskFinishedSession != session {
		t.Fatalf("the claim finished task %d in %q, want %d in %q", c.TaskFinished, c.TaskFinishedSession, number, session)
	}
	if c.ServingGeneration != 3 || c.ServedEntry != 2 || c.GivenEntry != 2 {
		t.Fatalf("the claim serves run %d, entry %d, given %d; want run 3 and entry 2 served, not handed back as owed",
			c.ServingGeneration, c.ServedEntry, c.GivenEntry)
	}
	if stored := h.store.load(testToken); stored.ServingGeneration != 3 || stored.ServedEntry != 2 {
		t.Fatalf("the stored claim serves run %d, entry %d, want the finish written with its credit",
			stored.ServingGeneration, stored.ServedEntry)
	}
	h.wantBudgets(Budgets{})
	h.wantState(StateReady)

	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)

	wantTurnLine(t, h, startedLine, h.generation(), number)
	wantTurnLine(t, h, endedLine, h.generation(), number)
	if finished := h.records.of(finishedLine); len(finished) != 1 {
		t.Fatalf("logged %d finishes, want the report's one", len(finished))
	} else {
		wantAttr(t, finished[0], "by", "report")
	}
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 3 || c.ServedEntry != 2 {
		t.Fatalf("after the late start the claim holds %+v and serves run %d, entry %d; want nothing pending and the credit kept",
			c.Pending, c.ServingGeneration, c.ServedEntry)
	}
	if _, armed := h.m.timers[TimerFinish]; armed {
		t.Fatal("the end-frame bound is armed after the late turn, want no task left to finish")
	}
}
