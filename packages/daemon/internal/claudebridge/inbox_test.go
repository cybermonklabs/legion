package claudebridge

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// takePending asks the bridge whether its take holds a task, as claude-envoy's Stop and
// PostToolUse hooks do (src/legion-inbox.ts): one take-pending line down a connection of its own,
// whose answer must be ok and carry the bridge's answer.
func (r *rig) takePending() TakePendingAnswer {
	r.t.Helper()
	answer := r.handLine(InboxDelivery{ID: "take-pending", Kind: InboxTakePending})
	if answer.ID != "take-pending" || !answer.OK || answer.Error != "" || answer.TakePending == nil {
		line, _ := json.Marshal(answer)
		r.t.Fatalf("the inbox answered the take-pending query %s, want ok with whether a take is pending", line)
	}
	return *answer.TakePending
}

// pendingIs holds that the bridge answers a take-pending query with want; when is the state the
// case has brought the bridge to.
func (r *rig) pendingIs(when string, want TakePendingAnswer) {
	r.t.Helper()
	if got := r.takePending(); got != want {
		r.t.Fatalf("%s the bridge answered the take-pending query %+v, want %+v", when, got, want)
	}
}

// A take-pending query is answered at once by the bridge's loop: whether the claim's take holds a
// task — an owed value not yet asked about, a take in flight, an accepted take whose turn has not
// started, or a take of the held value refused in-turn during the turn running — and the number of
// the owed task the turn running did not take, asked about or not. claude-envoy's Stop hook blocks
// no stop while a take is pending (R6), and its PostToolUse hook names the waiting number in R1's
// note.
func TestInboxTakePendingAnswersWhetherTheTakeHoldsATask(t *testing.T) {
	t.Run("no value", func(t *testing.T) {
		d := newTaskDaemon(t)
		r := newTakingRig(t, d, "", daemonCalls{})
		r.handReady()

		r.pendingIs("idle with no value,", TakePendingAnswer{})
		r.turn("ev1", "wait r1")
		r.expect("agent_start", nil)
		r.pendingIs("in a card's turn with no value,", TakePendingAnswer{})
		r.release("r1")
		r.expect("agent_end", nil)
		r.quiet()
	})

	// The take asked at the card's turn's end is refused by another role's turn, so the value is
	// asked about again only at its next revision: the next card's turn starts, and the value,
	// owed and not taken, is still waiting in it, though no take of it is pending.
	t.Run("an owed value during a turn", func(t *testing.T) {
		d := newTaskDaemon(t)
		d.answerTake = func(claim.TakeRequest) (int, any) {
			return http.StatusOK, claim.TakeResponse{Outcome: takeRefused, Hold: "issue-in-turn"}
		}
		r := newTakingRig(t, d, "", daemonCalls{})
		r.handReady()

		r.turn("ev1", "wait r1")
		r.expect("agent_start", nil)
		r.handTask(testTask, 1)
		r.pendingIs("with task 7 handed during a card's turn,", TakePendingAnswer{Pending: true, Waiting: testTask})

		r.release("r1")
		r.expect("agent_end", nil)
		r.logged("the daemon refused the take of task 7 (hold issue-in-turn)")
		r.pendingIs("idle, the take refused by another role's turn,", TakePendingAnswer{})
		r.turn("ev2", "wait r2")
		r.expect("agent_start", nil)
		r.pendingIs("in the next card's turn, the take of that revision refused,", TakePendingAnswer{Waiting: testTask})
		r.release("r2")
		r.expect("agent_end", nil)
		r.quiet()
		if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
			t.Fatalf("the bridge asked to take %q, want task 7 once", takes)
		}
	})

	t.Run("a take in flight", func(t *testing.T) {
		d := newTaskDaemon(t)
		released := make(chan struct{})
		release := sync.OnceFunc(func() { close(released) })
		t.Cleanup(release)
		d.answerTake = func(claim.TakeRequest) (int, any) {
			<-released
			return accepted("say the task")
		}
		r := newTakingRig(t, d, "", daemonCalls{})
		r.handReady()

		r.handTask(testTask, 1)
		r.waitFor("the take to reach the daemon", func() bool { return len(d.takesMade()) == 1 })
		r.pendingIs("idle, with the take in flight,", TakePendingAnswer{Pending: true})

		release()
		r.expect("agent_start", nil)
		r.untilTurnEnds()
		r.pendingIs("once the task's turn has ended,", TakePendingAnswer{})
		r.quiet()
	})

	// The take is asked between a card's turn and the turn Claude Code starts by itself after it,
	// and accepted during that turn (TestATakeAcceptedDuringARunStartsNothingInIt).
	t.Run("a held task", func(t *testing.T) {
		d := newTaskDaemon(t)
		answered := make(chan struct{})
		answer := sync.OnceFunc(func() { close(answered) })
		t.Cleanup(answer)
		d.answerTake = func(claim.TakeRequest) (int, any) {
			<-answered
			return accepted("wait t1")
		}
		r := newTakingRig(t, d, "", daemonCalls{})
		r.handReady()
		inbox := r.dialInbox()

		inbox.deliver("ev1", "wait r1")
		r.expect("agent_start", nil)
		r.handTask(testTask, 1)
		r.runFollowedByOneOfItsOwn(d, inbox)
		r.waitFor("the take to reach the daemon", func() bool { return len(d.takesMade()) == 1 })
		r.pendingIs("in a turn Claude Code started, with the take in flight,", TakePendingAnswer{Pending: true, Waiting: testTask})

		answer()
		r.logged("the daemon accepted the take of task 7 during a turn")
		r.pendingIs("with the accepted take's text held,", TakePendingAnswer{Pending: true})
		r.open()
		r.expect("agent_end", nil)
		r.expect("agent_start", nil)
		// The turn is the task's own only once Claude Code has re-emitted its text, which comes
		// after agent_start; every hook of the turn asks after it.
		r.logged("Claude Code took task 7's text")
		r.pendingIs("in the task's own turn,", TakePendingAnswer{})
		r.release("t1")
		r.untilTurnEnds()
		r.quiet()
	})

	// The take is asked between a card's turn and the turn Claude Code starts by itself after it,
	// and refused in-turn during that turn (row S19a). The daemon writes the value again at that
	// turn's end, so the take counts as pending until the turn's result, or until a newer value is
	// held, and not after either.
	t.Run("refused in-turn during a turn", func(t *testing.T) {
		refusedDuringATurn := func(t *testing.T) (*taskDaemon, *rig) {
			t.Helper()
			d := newTaskDaemon(t)
			answered := make(chan struct{})
			answer := sync.OnceFunc(func() { close(answered) })
			t.Cleanup(answer)
			d.answerTake = func(claim.TakeRequest) (int, any) {
				<-answered
				return http.StatusOK, claim.TakeResponse{Outcome: takeRefused, Hold: holdInTurn}
			}
			r := newTakingRig(t, d, "", daemonCalls{})
			r.handReady()
			inbox := r.dialInbox()

			inbox.deliver("ev1", "wait r1")
			r.expect("agent_start", nil)
			r.handTask(testTask, 1)
			r.runFollowedByOneOfItsOwn(d, inbox)
			r.waitFor("the take to reach the daemon", func() bool { return len(d.takesMade()) == 1 })

			answer()
			r.logged("the daemon refused the take of task 7 (hold in-turn)")
			r.pendingIs("in the turn the take was refused in-turn during,", TakePendingAnswer{Pending: true, Waiting: testTask})
			return d, r
		}

		t.Run("until the turn's result", func(t *testing.T) {
			d, r := refusedDuringATurn(t)
			r.open()
			r.expect("agent_end", nil)
			r.pendingIs("after that turn's result,", TakePendingAnswer{})
			r.quiet()
			if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
				t.Fatalf("the bridge asked to take %q, want task 7 once", takes)
			}
		})

		t.Run("until a newer value", func(t *testing.T) {
			_, r := refusedDuringATurn(t)
			r.handTask(0, 2)
			r.pendingIs("in that turn, with a newer value that holds no task,", TakePendingAnswer{})
			r.open()
			r.expect("agent_end", nil)
			r.quiet()
		})
	})

	// claude-envoy writes each delivery down one connection, whose answers come in the order its
	// lines did, and a delivery's answer waits for Claude Code to take it. Its query goes down a
	// connection of its own, and the loop answers it at once.
	t.Run("behind a waiting delivery", func(t *testing.T) {
		d := newTaskDaemon(t)
		r := newTakingRig(t, d, "", daemonCalls{})
		r.handReady()

		r.turn("p1", "wait r1")
		r.expect("agent_start", nil)
		r.handTask(testTask, 1)
		inbox := r.dialInbox()
		inbox.deliver("ev1", "say the card")
		r.logged("Envoy delivery ev1 waits for the next step")
		r.pendingIs("with a delivery waiting for the next step,", TakePendingAnswer{Pending: true, Waiting: testTask})
		inbox.noAnswer()

		r.release("r1")
		if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
			t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
		}
		r.expect("agent_end", nil)
		r.expect("agent_start", nil)
		r.untilTurnEnds()
		r.quiet()
	})
}

// The running task's own number can reach the bridge at a newer revision during its own turn: the
// daemon writes a value again wherever the bucket holds anything but its own last write (valueRead,
// internal/supervise/taskvalue.go). The bridge takes nothing for it, since that turn's end holds
// the number finished, so no take of it is pending, and claude-envoy's Stop hook must still block
// that turn's stop with its reminder (R6) rather than let it end for a take that never comes.
func TestInboxTakePendingIsFalseForTheRunningTasksOwnNumberWrittenAgain(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerTake = func(claim.TakeRequest) (int, any) { return accepted("wait t1") }
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	r.handTask(testTask, 1)
	r.expect("agent_start", nil)
	// The turn is the task's own only once Claude Code has re-emitted its text, which comes after
	// agent_start; every hook of the turn asks after it.
	r.logged("Claude Code took task 7's text")
	r.pendingIs("in the task's own turn,", TakePendingAnswer{})
	r.handTask(testTask, 2)
	r.pendingIs("in the task's own turn, with its number written again at a newer revision,", TakePendingAnswer{})
	r.release("t1")
	r.untilTurnEnds()
	r.quiet()
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
		t.Fatalf("the bridge asked to take %q, want task 7 once", takes)
	}
}

// The running task's own number written again during a turn whose text Claude Code never
// re-emitted (the bridge started task 7's turn and Claude Code dropped its text): that turn's end
// marks nothing finished (its result names task 0), so the bridge takes the newer revision the
// moment the turn ends. That take is the next turn, so the take is pending in the turn (R6), and a
// Stop hook told otherwise blocks the stop with a reminder ahead of the take.
func TestInboxTakePendingCountsTheRunningTasksNumberWrittenAgainWhenItsTextWasDropped(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerTake = func(take claim.TakeRequest) (int, any) {
		if take.Revision == 1 {
			return accepted("drop t1")
		}
		return accepted("say the task taken again")
	}
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	r.handTask(testTask, 1)
	r.expect("agent_start", nil)
	r.handTask(testTask, 2)
	got := r.takePending()
	r.release("t1")
	r.untilTurnEnds()
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.quiet()
	takes := pairsOf(t, d.takesMade())
	if !slices.Equal(takes, []string{"7 at 1", "7 at 2"}) {
		t.Fatalf("the bridge asked to take %q, want 7 at 1 and, at the dropped turn's end, 7 at 2", takes)
	}
	if !got.Pending {
		t.Fatalf("in task 7's turn whose text Claude Code dropped, with 7 written again at revision 2, the bridge answered the take-pending query %+v; want it pending, since that turn's end takes 7 at 2 at once (%q)", got, takes)
	}
}

// In task 7's turn whose text Claude Code dropped, the turn did not take the text, so it is no
// task's turn: the newer revision of 7 written during it waits as a value would during a turn of no
// task. The take-pending answer names 7 waiting, which claude-envoy's PostToolUse hook names in its
// note (R1), as well as pending.
func TestInboxTakePendingNamesTheRunningTasksNumberWaitingWhenItsTextWasDropped(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerTake = func(take claim.TakeRequest) (int, any) {
		if take.Revision == 1 {
			return accepted("drop t1")
		}
		return accepted("say the task taken again")
	}
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	r.handTask(testTask, 1)
	r.expect("agent_start", nil)
	r.handTask(testTask, 2)
	r.pendingIs("in task 7's turn whose text Claude Code dropped, with 7 written again at revision 2,", TakePendingAnswer{Pending: true, Waiting: testTask})
	r.release("t1")
	r.untilTurnEnds()
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.quiet()
}
