package supervise

// A relaunch of a claim holding a take runs that take's task once (dispatch://INFRA-2811). The
// take-back of a task a process holds is made where the process is let go and not before — at a
// launch that runs now, before it lets go; at a parked launch, at the wake's turn (replaceNow,
// runWake) — and a take whose turn is over is settled by its claim's contract: finished by its
// end frame where the end frame finishes a task, taken back with the note a cut turn gets where
// only a report naming the number does (interrupted, settle).

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// migratedTake is a claim restored in state on a plugin below TaskTakeContract — the harness's
// own contract — holding the take the migration's fill makes of an acknowledged, unconfirmed
// prompt of the release before: confirmed, its text the prompt's, its turn not started.
func migratedTake(state ClaimState) Claim {
	c := fixture(state)
	c.PluginContract = 24
	c.Pending.ConfirmedAt, c.Pending.TakenText, c.Pending.TurnStarted, c.TaskTaken = epoch, c.Pending.Task, false, 1
	return c
}

// restoreMigrated is a harness over the migrated take, its process's shim reconnected, paced at
// the shipped rate with the burst already spent when paced, so the next launch parks. The old
// process's plugin takes nothing (it is the release before the take's), so the harness's worker
// asks nothing until relaunchedTakes hands the claim to the process a relaunch starts.
func restoreMigrated(t *testing.T, paced bool) *harness {
	t.Helper()
	h := newBareHarness(t)
	if paced {
		pacedDeps(wakeRate)(&h.deps)
	}
	h.manualTakes()
	h.restore(migratedTake(StateReady))
	h.connect()
	if paced {
		// A place the fresh pacer lets out at its join spends the burst's one turn.
		h.deps.WakePace.Pacer.Join(wakepace.Second)
	}
	return h
}

// relaunchedTakes walks the process a relaunch started to ready, its plugin taking as the
// current one does.
func (h *harness) relaunchedTakes() {
	h.t.Helper()
	h.worker.manual = false
	h.relaunched()
}

// The migrated take's task runs once whichever way its claim is relaunched.
//
//   - A parked relaunch with the old turn in the wait: the take stands over the wait, so the
//     turn the old process starts there is the take's; the wake's turn cuts it off and takes the
//     task back with its note, and the process the launch starts takes it again behind that note.
//     Negative control: the take-back made at the park (replaceNow's interrupted ahead of the
//     launch) — the old turn is then no take's, and the new process is handed the task with no
//     word of the turn that ran it.
//   - A cut turn: the take's turn runs when a parked relaunch is decided and ends in the wait;
//     the end frame finishes it, and the new process is handed nothing. Negative control: the
//     same take-back at the park — the task is then taken back from a turn that runs whole, and
//     the new process runs it a second time.
//   - A relaunch held for the old turn's end: the stale-plugin lane waits out the take's turn,
//     the end frame finishes the task, and the relaunch that follows hands the new process
//     nothing. Negative control: the end frame finishing no task (TakesNoTask false) — the
//     task then stands unfinished and the relaunch never runs.
//   - A sweep's relaunch inside the end-frame bound: the take's turn ended and its bound runs when
//     the relaunch lets the process go; the task is finished, not taken back. Negative control:
//     a take whose turn is over taken back whatever the contract (interrupted's gate) — the new
//     process runs it again.
//   - A relaunch before any turn: the take is given back before the process is let go, and the
//     new process's take is decided afresh. The agent has not answered the ask its shim's hello
//     made, so the turn may have run unseen, and the take goes back as a cut turn with its note
//     (turnMayHaveRunUnseen). Negative control: no take-back at a launch that runs now
//     (launchReplacing's takeBack) — the new process is answered the old take from the record.
func TestRelaunchOfAMigratedTakeRunsItOnce(t *testing.T) {
	t.Run("a parked relaunch with the old turn in the wait", func(t *testing.T) {
		h := restoreMigrated(t, true)
		h.observe(runtime.StaleAddress)
		waiting := h.wantWaitingWake(WakeLaunch)
		if p := h.pending(); !p.awaitsStart() {
			t.Fatalf("the task after the park is %+v, want the migrated take standing", p)
		}
		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.startRecorded() {
			t.Fatalf("the turn in the wait left %+v, want it the take's turn", p)
		}

		h.advance(waiting.At.Sub(h.clock.Now()))

		if p := h.pending(); p.taken() || !p.Interrupted {
			t.Fatalf("the task after the wake's turn is %+v, want it taken back from the turn the wake cut off", p)
		}
		h.relaunchedTakes()
		again := h.wantPrompts(1)[0]
		wantSays(t, again.Message, saysTaskTurn)
		if !strings.HasSuffix(again.Message, "the task") {
			t.Fatalf("the new process took %q, want the task behind the word of its cut turn", again.Message)
		}
	})
	t.Run("a cut turn", func(t *testing.T) {
		h := restoreMigrated(t, true)
		h.must(StreamTurnStart{Claim: h.token})
		h.observe(runtime.StaleAddress)
		waiting := h.wantWaitingWake(WakeLaunch)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportActivity()

		h.advance(waiting.At.Sub(h.clock.Now()))
		h.relaunchedTakes()

		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the relaunch, want the task its turn ran finished", p)
		}
		h.wantPrompts(0)
	})
	t.Run("a relaunch held for the old turn's end", func(t *testing.T) {
		h := restoreMigrated(t, false)
		h.must(StreamTurnStart{Claim: h.token})
		h.deps.PluginContract, h.m.deps.PluginContract, h.m.deps.PluginRelaunchable = 25, 25, true
		h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
		h.wantCalls("Resume", 0)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportActivity()

		h.advance(testRPC)
		h.relaunchedTakes()

		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the relaunch, want the task its turn ran finished", p)
		}
		h.wantCalls("Resume", 1)
		h.wantPrompts(0)
	})
	t.Run("a sweep's relaunch inside the end-frame bound", func(t *testing.T) {
		h := restoreMigrated(t, false)
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportActivity()

		h.observe(runtime.StaleAddress)
		h.relaunchedTakes()

		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the relaunch, want the task its turn ran finished", p)
		}
		h.wantPrompts(0)
	})
	t.Run("a relaunch before any turn", func(t *testing.T) {
		h := restoreMigrated(t, false)
		h.reportActivity()

		h.observe(runtime.StaleAddress)
		if p := h.pending(); p.taken() || !p.Interrupted {
			t.Fatalf("the task after the relaunch is %+v, want the take given back as a cut turn: no answer followed the hello, so its turn may have run unseen", p)
		}
		h.relaunchedTakes()

		again := h.wantPrompts(1)[0]
		wantSays(t, again.Message, saysTaskTurn)
		if !strings.HasSuffix(again.Message, "the task") {
			t.Fatalf("the new process took %+v, want the task behind the word of its cut turn", again)
		}
		answered := h.records.of(answeredLine)
		wantAttr(t, answered[len(answered)-1], "repeat", "false")
	})
}

// A take whose turn is over, on a plugin that names its task's number, is taken back with the
// note a cut turn gets when a relaunch lets its process go: only a report naming the number
// finishes such a task, and the end of a run says nothing of whose run it was. The process the
// relaunch starts takes it again behind that note. The relaunch is a repoint whose observation
// says the daemon's own API moved, which no report can reach, so it is not held for the report
// as every other stale address is after the take's turn ended (rule two of T3, holdReplacement).
//
// The negative control is the end frame finishing it whatever the contract (interrupted's gate
// before this change): the task is finished, and the new process is handed nothing.
func TestRelaunchTakesBackACurrentPluginsTakeWhoseTurnIsOverWithItsNote(t *testing.T) {
	h := newHarnessOf(t, queuedClaim(), currentPlugin)
	h.reach(StateReady)
	h.takesTurnEndsWithItsReportToCome()
	number := h.pending().Number
	h.reportActivity()

	h.observeMovedDaemonAPI()

	if p := h.pending(); p.taken() || !p.Interrupted || p.Number != number {
		t.Fatalf("the task after the relaunch is %+v, want task %d taken back with the note of a cut turn", p, number)
	}
	h.relaunched()
	again := h.wantPrompts(2)[1]
	if again.Number != number {
		t.Fatalf("the new process took %+v, want task %d", again, number)
	}
	wantSays(t, again.Message, saysTaskTurn)
	if !strings.HasSuffix(again.Message, "review the pull request") {
		t.Fatalf("the new process took %q, want task %d behind the word of its cut turn", again.Message, number)
	}
}

// restoreStaleMigrated is a daemon start over the migrated take on a plugin the stale-plugin lane
// replaces: this daemon speaks contract 25 and can relaunch onto it, the claim recorded 24. The
// boot's pass has posted the claim its relaunch before its shim said hello, as the daemon's start
// does (refreshStalePlugins after the restored machines are built), and that post relaunched
// nothing.
func restoreStaleMigrated(t *testing.T) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.PluginContract, h.deps.PluginRelaunchable = 25, true
	h.manualTakes()
	h.restore(migratedTake(StateReady))
	h.must(StalePlugin{Claim: h.token, Trigger: "boot"})
	h.wantCalls("Resume", 0)
	return h
}

// answer is the agent's word on whether it is in a turn, as the worker stream delivers it: behind
// every frame the shim replayed after its hello (StreamTurnAnswered, the stream's dispatch).
func (h *harness) answer(streaming bool) {
	h.t.Helper()
	h.must(StreamTurnAnswered{Claim: h.token, Streaming: streaming})
}

// wantFinishedOnceAndRelaunchedEmpty is the end every ordering that ran the take's turn reaches:
// the task finished once, by its end frame (the claim's plugin names no number), nothing taken
// back, the relaunch made, and the process it starts handed nothing to take again.
func (h *harness) wantFinishedOnceAndRelaunchedEmpty() {
	h.t.Helper()
	h.relaunchedTakes()
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("pending %+v after the relaunch, want the task its turn ran finished", p)
	}
	h.wantFinishedOnce(finishedByEndFrame)
	if back := h.records.of(takenBackLiteral); len(back) != 0 {
		h.t.Fatalf("%q was logged %d times, want the task never taken back", takenBackLiteral, len(back))
	}
	h.wantCalls("Resume", 1)
	h.wantPrompts(0)
}

// After a daemon start, the stale-plugin lane relaunches a claim restored ready or idle only once
// its agent has said whether it is in a turn, in the decision that answer arrives in, which the
// worker stream delivers behind every frame the shim replayed after its hello — or once the
// reconnect deadline has passed (dispatch://INFRA-2811, T3's rule three). The claim is the
// migrated take of a stale plugin: accepted and awaiting its turn, which the old process runs as
// the acknowledged prompt it is. The fake shim, as the real one does, replays the turn's frames
// only after the daemon has answered its hello.
//
//   - A replayed agent_start before an answer of a turn: the claim is working, the relaunch is
//     held for that turn's end, and the task is finished once.
//   - An answer of a turn whose agent_start the backlog dropped: the answer makes the claim
//     working and the take's turn started, and the same follows.
//   - A replayed start and end before an answer of no turn: the relaunch is made, and it finds
//     the task's turn over, so the task is finished by its contract and nothing is taken back.
//   - A connection that broke between its ask and the agent's answer: the next connection's hello
//     asks again on that connection, the replayed start makes the claim working, and that
//     connection's own answer of a turn holds the relaunch for the turn's end: the task is
//     finished once. The relaunch waits for the answer to the ask of the connection the hello
//     came on. That the first connection's answer, replayed on the next ahead of the take's
//     agent_start, reaches the machine as no answer, since that connection never asked it, is the
//     stream's to hold (Conn.stateAnswered), and stream's conn_test is what catches it.
//   - An ask the agent does not answer: not asked again at the next sweep (row 31 of the plan's
//     state table, v121), and relaunched at the reconnect deadline armed at the hello, by the lane,
//     charged nothing.
//   - A hello whose connection closed before the daemon asked on it, from a shim that cannot
//     reach the daemon again: no ask is made, and the claim is relaunched at the same deadline,
//     the same way. Negative control: askRestoredTurn arming no bound with no connection — the
//     hello disarmed the boot's reconnect deadline, and the relaunch never comes.
//   - No hello: relaunched at the reconnect deadline, as departure 7's never_reconnected.
//
// The negative controls are the boot's relaunch made before the hello (refreshPlugin with no
// askTurn guard) and made at the hello itself (askRestoredTurn deciding on the answer it is
// returned): in both the relaunch reads the take as one no turn ran, and it runs before the hello
// or before the replayed turn.
func TestBootRelaunchOfAStaleClaimWaitsForTheTurnOverAnswer(t *testing.T) {
	t.Run("a replayed agent_start before an answer of a turn", func(t *testing.T) {
		h := restoreStaleMigrated(t)
		h.conn.SetStreaming(true)
		h.connect()
		h.wantCalls("Resume", 0)
		h.must(StreamTurnStart{Claim: h.token})
		h.answer(true)

		h.wantState(StateWorking)
		h.wantCalls("Resume", 0)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.advance(testRPC)
		h.wantFinishedOnceAndRelaunchedEmpty()
	})
	t.Run("an answer of a turn whose agent_start the backlog dropped", func(t *testing.T) {
		h := restoreStaleMigrated(t)
		h.conn.SetStreaming(true)
		h.connect()
		h.answer(true)

		h.wantState(StateWorking)
		if p := h.pending(); !p.startRecorded() || p.TakenText != "the task" {
			t.Fatalf("the task after the answer is %+v, want its take's turn started", p)
		}
		h.wantCalls("Resume", 0)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.advance(testRPC)
		h.wantFinishedOnceAndRelaunchedEmpty()
	})
	t.Run("a replayed start and end before an answer of no turn", func(t *testing.T) {
		h := restoreStaleMigrated(t)
		h.connect()
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportActivity()
		h.wantCalls("Resume", 0)
		h.answer(false)

		h.wantFinishedOnceAndRelaunchedEmpty()
	})
	t.Run("a connection that broke before its answer: the relaunch waits for the next connection's own answer", func(t *testing.T) {
		h := restoreStaleMigrated(t)
		// The first connection breaks between the hello's ask and the agent's answer, which
		// stays in the shim's backlog with the take's agent_start behind it.
		h.conn.FailGetState(errBoom)
		h.connect()
		h.must(StreamClosed{Claim: h.token})
		h.conn = fake.NewConn()
		h.conn.SetStreaming(true)
		h.connect()
		if unanswered := h.logs.lines("supervise: the agent could not say whether it is in a turn"); len(unanswered) != 1 {
			t.Fatalf("the asks the agent left unanswered are %q, want the first connection's alone", unanswered)
		}
		if ask := h.conn.LastAsk(); ask != "ask-1" {
			t.Fatalf("the next connection was asked up to %q, want its one ask", ask)
		}
		h.wantCalls("Resume", 0)
		// The replay: the first connection's answer of no turn is no event of the next
		// connection's, so the replayed agent_start is the first thing the machine reads.
		h.must(StreamTurnStart{Claim: h.token})
		h.answer(true)

		h.wantState(StateWorking)
		h.wantCalls("Resume", 0)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.advance(testRPC)
		h.wantFinishedOnceAndRelaunchedEmpty()
	})
	t.Run("an unanswered ask", func(t *testing.T) {
		h := restoreStaleMigrated(t)
		h.conn.FailGetState(errBoom)
		// The hello comes half a deadline after the daemon's start, so a deadline counted from
		// the start would come due half a deadline early.
		h.advance(deadline / 2)
		h.connect()
		h.advance(deadline / 2)
		h.observe(runtime.Alive)
		if asks := h.logs.lines("supervise: the agent could not say whether it is in a turn"); len(asks) != 1 {
			t.Fatalf("the asks the agent left unanswered are %q, want the hello's alone: no sweep asks its connection again", asks)
		}
		h.wantRelaunchedUnchargedAtTheDeadline()
	})
	t.Run("a hello whose connection closed before its ask", func(t *testing.T) {
		h := restoreStaleMigrated(t)
		// The stream queued the hello and then the close of its connection, which went before
		// the decision that reads the hello could ask on it; the shim cannot reach the daemon
		// again, and the process it runs lives.
		h.advance(deadline / 2)
		h.must(StreamHello{Claim: h.token, Generation: h.generation()})
		h.must(StreamClosed{Claim: h.token})
		h.advance(deadline / 2)
		h.observe(runtime.Alive)
		if asks := h.logs.lines("supervise: the agent could not say whether it is in a turn"); len(asks) != 0 {
			t.Fatalf("the asks made are %q, want none: no connection was there to ask on", asks)
		}
		h.wantRelaunchedUnchargedAtTheDeadline()
	})
	t.Run("no hello", func(t *testing.T) {
		h := restoreStaleMigrated(t)
		h.advance(deadline - time.Second)
		h.wantCalls("Resume", 0)
		h.advance(time.Second)

		h.wantCalls("Resume", 1)
		if cause := h.claim().PendingCause; cause != CauseNeverReconnected {
			t.Fatalf("the relaunch is pending cause %q, want %q", cause, CauseNeverReconnected)
		}
	})
}

// wantRelaunchedUnchargedAtTheDeadline advances the clock to the reconnect deadline counted from a
// hello the claim's shim said half a deadline after the daemon's start, and holds that the lane
// relaunches the claim there and not a second before, charged nothing.
func (h *harness) wantRelaunchedUnchargedAtTheDeadline() {
	h.t.Helper()
	h.advance(deadline - deadline/2 - time.Second)
	h.wantCalls("Resume", 0)
	h.advance(time.Second)

	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if cause := h.claim().PendingCause; cause != "" {
		h.t.Fatalf("the relaunch is pending cause %q, want none: the lane's relaunch is charged nothing", cause)
	}
}
