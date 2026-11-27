package supervise

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A turn's report can reach the daemon ahead of that turn's start frame, and it finishes the task
// then (startedUnseen); the mark of the start still to come (Machine.reportedAhead) is memory only.
// A daemon that restarts before it reads that turn's frames loses the mark, and the shim's replay
// then carries the turn's start and end to a machine that may hold a newer task's take: the
// restarted machine can read the replayed turn as that take's. The newer task's own turn then runs,
// and its report naming the task finishes it. Each task must be taken once and finished once, by
// its report, with nobody charged, nothing taken back or written again and no launch, also once R5's
// second bound has passed while the newer take's own turn ran: that turn's start disarms the wait for
// the report that the replayed end armed. The orders are where the newer take, and its own report,
// stand against the replay.
//
// The architect asked for this proof in test round 12 (implement handoff v18's openQuestions,
// "reportedAhead"); the tester's real-stack drive runs the same three orders (steps a10l, a10m and
// a10n). Its negative control is the head ba2fbe5f with reportWaitNow's in-turn check removed
// (reportwait.go:221-226): the bound passing while the newer take's own turn runs takes that task
// back, and the first two orders fail.
func TestTakeAfterADaemonRestartOverTheReplayOfATurnReportedAheadRunsEachTaskOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		// run is the restarted machine's part: from the restart up to the newer task finished by
		// its report. It answers the newer task's number.
		run func(t *testing.T, h *harness) uint64
	}{
		{"the newer task taken before the replay", func(t *testing.T, h *harness) uint64 {
			second := h.takeOf("the newer task")
			h.helloOf(true, false)
			h.must(StreamTurnStart{Claim: h.token})
			t.Logf("the replayed start of the first task's turn left the newer take %+v", h.pending())
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.answerAsk(false)
			h.must(StreamTurnStart{Claim: h.token})
			h.advance(turnStartBound + time.Second)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.reportOf(2, second)
			return second
		}},
		{"the newer task taken inside the replayed turn", func(t *testing.T, h *harness) uint64 {
			h.helloOf(true, false)
			h.must(StreamTurnStart{Claim: h.token})
			h.must(RequestDeliver{Claim: h.token, Task: "the newer task"})
			if answer := h.take(); answer.Outcome == TakeOutcomeAccepted {
				t.Logf("the take inside the replayed turn was accepted into it")
			} else {
				t.Logf("the take inside the replayed turn was answered %+v", answer)
			}
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			if !h.pending().taken() {
				h.mustTake()
			}
			second := h.pending().Number
			h.must(StreamTurnStart{Claim: h.token})
			h.advance(turnStartBound + time.Second)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.reportOf(2, second)
			return second
		}},
		{"the newer task's report ahead of the replay of both turns", func(t *testing.T, h *harness) uint64 {
			second := h.takeOf("the newer task")
			h.reportOf(2, second)
			h.helloOf(true, false)
			h.must(StreamTurnStart{Claim: h.token})
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.must(StreamTurnStart{Claim: h.token})
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.answerAsk(false)
			return second
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := boundsHarness(t)
			first := h.takeOf("the first task")
			h.reportOf(1, first)
			if p := h.claim().Pending; p != nil {
				t.Fatalf("the report of task %d's turn, ahead of its start frame, left %+v pending, want the task finished", first, p)
			}
			spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))
			h.restart()
			h.conns.Remove(h.token)
			h.conn = fake.NewConn()

			second := tc.run(t, h)

			if p := h.claim().Pending; p != nil {
				t.Fatalf("after the report of task %d's own turn the claim holds %+v, want it finished", second, p)
			}
			h.advance(2 * turnStartBound)
			for _, number := range []uint64{first, second} {
				takes := 0
				for _, took := range h.prompts() {
					if took.Number == number {
						takes++
					}
				}
				if takes != 1 {
					t.Errorf("task %d was taken %d times (%+v), want once", number, takes, h.prompts())
				}
				finished := h.recordsNaming(finishedLine, number)
				if len(finished) != 1 {
					t.Errorf("task %d was finished %d times, want once", number, len(finished))
				} else {
					wantAttr(t, finished[0], "by", finishedByReport)
				}
				if back := h.recordsNaming(takenBackLine, number); len(back) != 0 {
					t.Errorf("task %d was taken back: %v", number, back)
				}
			}
			if value, held := h.values.value(h.token); held {
				t.Errorf("the bucket holds %+v after both tasks finished, want no value", value)
			}
			h.wantCharges()
			h.wantBudgets(Budgets{})
			h.wantCalls("Spawn", spawns)
			h.wantCalls("Resume", resumes)
		})
	}
}
