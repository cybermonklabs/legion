package supervise

// A shim's hello that does not say its backlog is whole (StreamHello.BacklogWhole): the shim
// dropped the oldest frames it held while no daemon was connected, so a turn's start, or a whole
// turn, may never reach the machine. What the machine read off the frames it saw stops standing
// for the turns that ran: its count of the process's turns (turnCount), the mark a report leaves
// for a start still to come (Machine.reportedAhead), and a restored take's turn read as not
// started by the stale-plugin relaunch decided behind that replay (turnAnswered).

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// reconnectLater is the claim's shim losing its connection, its next one still to come.
func (h *harness) reconnectLater() {
	h.t.Helper()
	h.must(StreamClosed{Claim: h.token})
	h.conns.Remove(h.token)
	h.conn = fake.NewConn()
}

// cutHello is the shim's hello on the claim's connection, saying nothing of its backlog being
// whole: it dropped frames while no daemon was connected.
func (h *harness) cutHello() {
	h.t.Helper()
	h.conns.Register(h.token, h.conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation()})
}

// A reconnect whose hello says the shim's backlog dropped frames loses the machine's count of the
// process's turns (turnCount.lose). A whole turn the backlog dropped — a run of the host's own, G,
// the first after the take, started and ended while no daemon was connected — leaves the count one
// turn short: the take's own turn, T, whose start the replay carries, is counted as G's sequence,
// and G's late report, naming no task, puts back the take whose turn T is running
// (notTheTakesTurn). Lost, the count reads no report's sequence, and only a report it can place
// makes it known again (learnTurns): T keeps the take, and T's report finishes the task once.
//
// The negative control is the head before this change (1d8f65eb), whose count stays known across
// the hello: G's late report puts back the take while T runs.
func TestABacklogCutAtAReconnectLosesTheTurnCountSoALateReportKeepsTheTakesTurn(t *testing.T) {
	h := newHarnessOf(t, queuedClaim(), currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	number := h.takeOf("implement the plan")
	g := h.worker.sequence + 1
	h.reconnectLater()
	h.cutHello()
	h.must(StreamTurnStart{Claim: h.token})
	h.wantTakesTurnStanding(number, "once T's replayed start came")

	h.reportOf(g, 0)

	h.wantTakesTurnStanding(number, "after G's late report")
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.reportOf(g+1, number)
	h.wantFinishedOnce(finishedByReport)
}

// A report of the take's turn that comes before the machine saw that turn start marks the next
// start as that turn's (startedUnseen, Machine.reportedAhead). Where the shim's backlog dropped
// frames, that start may be gone for good: the take's own turn, T, ran whole while no daemon was
// connected, and the backlog dropped its start and its end. Kept, the mark takes the next task's
// start, which leaves that task's take awaiting a turn it is running. So a hello that says the
// backlog dropped frames clears the mark, and while the count of the process's turns is not known
// after such a hello no report marks a start as still to come: the next task's start is its own,
// whether T's report came before the hello or after it, within one daemon's life or after a
// restart. A start of T that does come, behind its report, is read as any other: the task it ran
// is finished, so it is no task's start, and the next task's start is still its own.
//
// The negative control is the head before this change (1d8f65eb), where the mark stands through
// the hello and takes the next task's start in the first three cases, and names T's task in T's
// late start in the fourth.
func TestABacklogCutLeavesNoMarkSoTheNextTakeKeepsItsOwnTurnStart(t *testing.T) {
	disconnected := func(t *testing.T) (*harness, uint64) {
		t.Helper()
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.manualTakes()
		h.reach(StateReady)
		number := h.takeOf("implement the plan")
		h.reconnectLater()
		return h, number
	}
	t.Run("T's report before the hello", func(t *testing.T) {
		h, number := disconnected(t)
		h.wantLateReportFinishesOnce(number, h.worker.sequence+1)
		h.cutHello()
		h.wantNextTaskStartsItsOwnTurn()
	})
	t.Run("T's report after the hello", func(t *testing.T) {
		h, number := disconnected(t)
		h.cutHello()
		h.wantLateReportFinishesOnce(number, h.worker.sequence+1)
		h.wantNextTaskStartsItsOwnTurn()
	})
	t.Run("T's report after the first hello since a restart", func(t *testing.T) {
		h, number := takeAcrossARestart(t, nothingMore)
		h.conn = fake.NewConn()
		h.cutHello()
		h.wantLateReportFinishesOnce(number, h.worker.sequence+1)
		h.wantNextTaskStartsItsOwnTurn()
	})
	t.Run("T's start after its report", func(t *testing.T) {
		h, number := disconnected(t)
		h.cutHello()
		h.wantLateReportFinishesOnce(number, h.worker.sequence+1)
		starts := len(h.records.of(turnStartedLine))
		h.must(StreamTurnStart{Claim: h.token})
		if task, named := h.records.of(turnStartedLine)[starts].attr("task"); named {
			t.Fatalf("T's late start names task %v, want no task's: the task it ran is finished", task)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantNextTaskStartsItsOwnTurn()
	})
}

// The stale-plugin relaunch of a claim a daemon start restored is decided behind the frames its
// shim replayed after its hello: where its agent answers it is in no turn (turnAnswered), or at the
// reconnect deadline that hello armed again where its agent answers nothing (reconnectDeadline).
// A take whose turn no replayed frame showed is given back unstarted, with no note, where that
// hello said the backlog is whole and the agent answered: no turn of it ran. Where the hello did
// not say so, the backlog may have dropped the take's whole turn, run while no daemon was
// connected, so the take goes back with the note of a cut turn (interrupted), as a take whose
// report never came does (reportMissed), and the next process is told its turn on the task may
// have run. So does a take whose agent answered nothing since the hello, whatever the hello said:
// the view is not whole when the relaunch is decided (turnMayHaveRunUnseen, row 26 of the plan).
//
// The negative control is the head before this change (1d8f65eb), which gives the take back
// unstarted after either hello, at the answer and at the deadline alike: the next process runs a
// task whose turn may have run as one never begun, with no word of it.
func TestAStaleRelaunchBehindABacklogCutTakesTheTakeBackAsACutTurn(t *testing.T) {
	for _, decided := range []struct {
		name           string
		before, decide func(h *harness)
		viewCut        bool
	}{
		{"the agent answers it is in no turn", func(*harness) {}, func(h *harness) { h.answer(false) }, false},
		{"the agent answers nothing by the reconnect deadline",
			func(h *harness) { h.conn.FailGetState(errBoom) },
			func(h *harness) {
				h.advance(deadline)
				h.conn.FailGetState(nil)
			}, true},
	} {
		for _, tc := range []struct {
			name  string
			hello func(h *harness)
			cut   bool
		}{
			{"a hello that says the backlog is whole", (*harness).connect, false},
			{"a hello that does not", (*harness).cutHello, true},
		} {
			t.Run(decided.name+", "+tc.name, func(t *testing.T) {
				h := restoreStaleTake(t)
				number := h.pending().Number
				decided.before(h)
				tc.hello(h)
				h.wantCalls("Resume", 0)

				decided.decide(h)

				cut := tc.cut || decided.viewCut
				if p := h.pending(); p.Number != number || p.taken() || p.Interrupted != cut {
					t.Fatalf("after the relaunch the claim holds %+v, want task %d taken back, the note of a cut turn %t", p, number, cut)
				}
				h.relaunchedTakes()
				again := h.wantPrompts(1)[0]
				if again.Number != number {
					t.Fatalf("the new process took %+v, want task %d", again, number)
				}
				if told := strings.Contains(again.Message, saysTaskTurn); told != cut {
					t.Fatalf("the new process took %q, which tells of a cut turn %t, want %t", again.Message, told, cut)
				}
			})
		}
	}
}
