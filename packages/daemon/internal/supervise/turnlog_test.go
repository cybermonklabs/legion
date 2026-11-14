package supervise

import (
	"log/slog"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The two turn messages as an operator and a production check read them: whole, and not as the
// constants that carry them. A reword would move the constant and leave a test that read the
// constant green, while every search that counts a turn's start against its end went quiet.
const (
	startedLine = "supervise: a turn started"
	endedLine   = "supervise: a turn ended"
)

// turnLines is every record of line that is a turn line: the turn-outcome report writes the same
// end message with a sequence attribute of its own (turnoutcome.go), and a reader counting a
// turn's start against its end skips it, as the daemon's cost reading does (T14 of
// dispatch://INFRA-2811).
func turnLines(h *harness, line string) []loggedRecord {
	var kept []loggedRecord
	for _, r := range h.records.of(line) {
		if _, reported := r.attr("sequence"); !reported {
			kept = append(kept, r)
		}
	}
	return kept
}

// wantTurnLine is the one record of line, read from the records the handler kept rather than from
// the text the handler printed: what this holds is the attributes' own values, and one of them —
// the claim — is an attribute of the machine's logger rather than of the call site. task is the
// number of the turn's task where it has one; 0 means the line must carry no task key at all,
// which is how a turn that is no task's is told from one whose task is unnamed.
func wantTurnLine(t *testing.T, h *harness, line string, generation uint64, task uint64) {
	t.Helper()
	kept := turnLines(h, line)
	if len(kept) != 1 {
		t.Fatalf("%q was written %d times, want once", line, len(kept))
	}
	r := kept[0]
	if r.level != slog.LevelInfo {
		t.Fatalf("%q is %s, want INFO", line, r.level)
	}
	if claim, ok := r.attr("claim"); !ok || claim.String() != string(h.token) {
		t.Fatalf("%q carries claim %v (present %v), want %s", line, claim, ok, h.token)
	}
	if got, ok := r.attr("generation"); !ok || got.Uint64() != generation {
		t.Fatalf("%q carries generation %v (present %v), want %d", line, got, ok, generation)
	}
	got, ok := r.attr("task")
	switch {
	case task == 0 && ok:
		t.Fatalf("%q carries task %v; a turn that is no task's names none", line, got)
	case task != 0 && !ok:
		t.Fatalf("%q carries no task, want %d", line, task)
	case task != 0 && got.Uint64() != task:
		t.Fatalf("%q carries task %v, want %d", line, got, task)
	}
}

// A turn's start and its end are the pair an operator counts against each other to read what the
// daemon did between them, so each of the two paths that put a claim in a turn writes the start,
// under the one message, and the turn's end writes the close. The line goes before the write that
// records the turn, never after it: the decision these lines exist for is a turn start whose
// claim write failed, and a line written after the write would leave exactly that decision
// silent.
func TestTheTurnLogLinesAreWrittenOnEveryPathIntoATurn(t *testing.T) {
	// The first path: an agent_start this daemon read in a ready claim after the worker's take of
	// the task was accepted, which is the take's own turn, so the line names the task's number.
	// Its end closes the pair under the same task.
	t.Run("a turn of the task's own in a ready claim", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		number := h.pending().Number

		h.must(StreamTurnStart{Claim: testToken})

		h.wantState(StateWorking)
		wantTurnLine(t, h, startedLine, h.generation(), number)

		h.must(StreamTurnEnd{Claim: testToken})

		wantTurnLine(t, h, endedLine, h.generation(), number)
	})

	// A turn the worker took nothing for — an Envoy notice, a person's steer — is no task's, and
	// the line names none rather than naming an empty one.
	t.Run("a turn that is no task's names no task", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)

		h.must(StreamTurnStart{Claim: testToken})

		h.wantState(StateWorking)
		wantTurnLine(t, h, startedLine, h.generation(), 0)
	})

	// A turn start whose claim write the database did not take writes its line all the same, with
	// the owed line beside it: the pair is what a reader of the log follows from the failure to
	// the repair, and the start line is exactly what the measured incident was missing. When the
	// write lands, the repair's line closes that story.
	t.Run("a start whose claim write failed, and the lines that follow it", func(t *testing.T) {
		h, _ := owedTurnStart(t)

		wantTurnLine(t, h, startedLine, h.generation(), 0)
		owed := h.records.of("supervise: a claim write the database did not take")
		if len(owed) != 1 {
			t.Fatalf("the owed line was written %d times, want once", len(owed))
		}
		if owed[0].level != slog.LevelWarn {
			t.Fatalf("the owed line is %s, want WARN", owed[0].level)
		}
		if store, ok := owed[0].attr("store"); !ok || store.String() != "PutClaim" {
			t.Fatalf("the owed line carries store %v (present %v), want PutClaim", store, ok)
		}
		if _, ok := owed[0].attr("error"); !ok {
			t.Fatal("the owed line carries no error; the reason the write was refused is the whole of what it adds")
		}

		h.store.fail("PutClaim", nil)
		h.observe(runtime.Alive)

		repaired := h.records.of("supervise: a claim write the database did not take was made")
		if len(repaired) != 1 {
			t.Fatalf("the repaired line was written %d times, want once", len(repaired))
		}
		if repaired[0].level != slog.LevelInfo {
			t.Fatalf("the repaired line is %s, want INFO", repaired[0].level)
		}
		if store, ok := repaired[0].attr("store"); !ok || store.String() != "PutClaim" {
			t.Fatalf("the repaired line carries store %v (present %v), want PutClaim", store, ok)
		}
	})

	// A turn Oh My Pi starts under a compaction is no path into working — the claim is already
	// working for the compaction — but it is a turn all the same, and it writes the pair an
	// operator counts: the start where the compaction records the turn (turnStartedWhileBusy)
	// and the end where that turn ends (turnEnded). An end with no start before it says the
	// daemon never saw the turn begin, and the daemon saw this one.
	t.Run("a turn that runs under a compaction", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		release := h.conn.HoldCompact()
		compacted := make(chan error, 1)
		go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
		waitForCompacting(t, h)

		h.must(StreamTurnStart{Claim: testToken})
		h.mustQuiet(StreamTurnEnd{Claim: testToken})

		release()
		if err := <-compacted; err != nil {
			t.Fatalf("compact: %v", err)
		}
		h.m.Wait()
		if starts, ends := h.records.of(startedLine), h.records.of(endedLine); len(starts) != len(ends) {
			t.Fatalf("the turn under the compaction wrote %d start lines and %d end lines, want them paired",
				len(starts), len(ends))
		}
		wantTurnLine(t, h, startedLine, h.generation(), 0)
		wantTurnLine(t, h, endedLine, h.generation(), 0)
	})
}
