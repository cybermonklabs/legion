package claudebridge

// A take's text and the turns it does not run in (the plan's decision 9): a take accepted while
// Claude Code is in a turn starts nothing in it, its text is held and runs once Claude Code is
// idle, a text held through a turn, or one Claude Code did not re-emit, is taken again at a newer
// revision and runs once, and a text already written hands nothing more at a newer revision. The
// stand-in daemon and the taking rig are take_test.go's.

import (
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// runFollowedByOneOfItsOwn is the turn of the card "wait r1", which the case holds open, whose
// next step is a card that replays a recorded stream: that stream ends the card's turn and then has
// Claude Code start a turn of its own (backgroundTurn), whose result the gate holds back. A value
// the case hands during the card's turn is asked for once that turn ends, and the turn Claude Code
// starts by itself begins while that take waits for its answer.
func (r *rig) runFollowedByOneOfItsOwn(d *taskDaemon, inbox *inboxClient) {
	r.t.Helper()
	inbox.deliver("ev2", backgroundTurn)
	r.logged("Envoy delivery ev2 waits for the next step")
	r.release("r1")
	r.expect("agent_end", nil)
	if report := d.reportOf(1); report.Task != 0 {
		r.t.Fatalf("the card's turn was reported %+v, want a turn of no task", report)
	}
	r.expect("agent_start", nil)
	r.logged("Claude Code started a turn of its own")
}

// A take the daemon accepts while Claude Code is in a turn starts nothing in it (the plan's
// decision 9): the take, asked for between two turns, is answered `accepted` during the turn
// Claude Code started by itself after it, which the daemon gave the take as the first to start
// after its accept. The text is held, that turn is reported as no task's and as holding the
// take's text unrun (held) — which puts the take back to awaiting its turn on the daemon, also
// where its count of the process's turns is not known —, and the text starts a turn of its own
// once Claude Code is idle, whose report names the task and holds nothing. A report before the
// take's answer came holds nothing either.
func TestATakeAcceptedDuringARunStartsNothingInIt(t *testing.T) {
	d := newTaskDaemon(t)
	answered := make(chan struct{})
	answer := sync.OnceFunc(func() { close(answered) })
	t.Cleanup(answer)
	d.answerTake = func(claim.TakeRequest) (int, any) {
		<-answered
		return accepted("say the task")
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
	r.logged("the daemon accepted the take of task 7 during a turn")
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	if report := d.reportOf(2); report.Task != 0 || report.Held != testTask {
		t.Fatalf("the turn Claude Code started by itself was reported %+v, want a turn of no task holding task %d", report, testTask)
	}
	if report := d.reportOf(1); report.Held != 0 {
		t.Fatalf("the card's turn, before the take's answer, was reported %+v, want it holding nothing", report)
	}
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(3); report.Task != testTask || report.Held != 0 {
		t.Fatalf("the turn after it was reported %+v, want the turn of task %d holding nothing", report, testTask)
	}
	r.quiet()
	if texts := r.userTexts(); !slices.Equal(texts, []string{"wait r1", backgroundTurn, "say the task"}) {
		t.Fatalf("Claude Code read %q, want the two cards and then the task's text", texts)
	}
}

// The lost-answer case (the plan's departure 7 and decision 9): the take's first attempt is lost
// to a reset connection, and while it waits to be sent again Claude Code starts a turn of its own;
// the daemon answers the take sent again `accepted` — a take of the accepted number is answered
// the same text until the task finishes — during that turn. The text is held, that turn is
// reported as no task's, and the text runs next, once.
func TestALostAnswersRepeatAcceptedDuringARunIsHeldAndRunsNextOnce(t *testing.T) {
	d := newTaskDaemon(t)
	answered := make(chan struct{})
	answer := sync.OnceFunc(func() { close(answered) })
	t.Cleanup(answer)
	d.answerTake = func(claim.TakeRequest) (int, any) {
		<-answered
		return accepted("say the task")
	}
	// Quick waits between attempts, and an attempt bound longer than the case: the attempt the
	// daemon holds until the case answers it is the one that is answered.
	calls := quickCalls
	calls.answerTimeout = time.Minute
	r := newTakingRig(t, d, "", calls)
	r.handReady()
	inbox := r.dialInbox()

	inbox.deliver("ev1", "wait r1")
	r.expect("agent_start", nil)
	r.handTask(testTask, 1)
	r.waitForSeen(d, testTask, 1)
	reset := resetConnection(t)
	d.setFault(func(w http.ResponseWriter, req *http.Request) {
		d.setFault(nil)
		reset(w, req)
	})
	r.runFollowedByOneOfItsOwn(d, inbox)
	r.logged("claims/take of task 7 got no answer from the daemon (the connection was reset)")
	r.waitFor("the take sent again to reach the daemon", func() bool { return len(d.takesMade()) == 1 })

	answer()
	r.logged("the daemon accepted the take of task 7 during a turn")
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	if report := d.reportOf(2); report.Task != 0 {
		t.Fatalf("the turn Claude Code started by itself was reported %+v, want a turn of no task", report)
	}
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(3); report.Task != testTask {
		t.Fatalf("the turn after it was reported %+v, want the turn of task %d", report, testTask)
	}
	r.quiet()
	if texts := r.userTexts(); !slices.Equal(texts, []string{"wait r1", backgroundTurn, "say the task"}) {
		t.Fatalf("Claude Code read %q, want the two cards and then the task's text once", texts)
	}
}

// The plan's decision 9: a number held as accepted is a new offer at a newer revision of it only
// while its text has not reached Claude Code. A text held through a turn the bridge did not start
// was never written to it: where the daemon writes the number's value again meanwhile — it took the
// task back, or only wrote the value again —, that turn's end starts nothing of the held text, the
// take is made at the newer revision once Claude Code is idle, and the text that answer carries
// runs, once.
func TestTakeOfATextHeldThroughATurnIsMadeAgainAtANewerRevisionAndRunsOnce(t *testing.T) {
	d := newTaskDaemon(t)
	answered := make(chan struct{})
	answer := sync.OnceFunc(func() { close(answered) })
	t.Cleanup(answer)
	d.answerTake = func(take claim.TakeRequest) (int, any) {
		if take.Revision == 1 {
			<-answered
			return accepted("say the task")
		}
		return accepted("say the task taken again")
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
	r.logged("the daemon accepted the take of task 7 during a turn")

	// The daemon writes the number's value again while the text is held: the value is reported
	// seen at once, and asked about only once the turn has ended.
	r.handTask(testTask, 2)
	r.waitForSeen(d, testTask, 2)
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	if report := d.reportOf(2); report.Task != 0 {
		t.Fatalf("the turn Claude Code started by itself was reported %+v, want a turn of no task", report)
	}
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(3); report.Task != testTask {
		t.Fatalf("the turn after it was reported %+v, want the turn of task %d", report, testTask)
	}
	r.quiet()
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1", "7 at 2"}) {
		t.Errorf("the bridge asked to take %q, want task 7 at revision 1 and again at revision 2", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"wait r1", backgroundTurn, "say the task taken again"}) {
		t.Fatalf("Claude Code read %q, want the two cards and then the newer answer's text once", texts)
	}
}

// Once the text is written to Claude Code a newer revision of its number changes nothing, also
// while Claude Code has not taken the text yet: the bridge starts the task's turn as it writes the
// text (startTask), asks nothing while that turn runs, and holds the number finished from the
// turn's end (the run-end mark). The text runs once (a guard keyed on Claude Code having begun the
// turn would hand the text a second time here).
func TestTakeOfANewerRevisionBeforeClaudeCodeTakesTheTextHandsNothingMore(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerTake = func(claim.TakeRequest) (int, any) { return accepted("held r1") }
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	r.handTask(testTask, 1)
	r.expect("agent_start", nil)
	r.handTask(testTask, 2)
	r.waitForSeen(d, testTask, 2)
	r.quiet()

	r.release("r1")
	r.untilTurnEnds()
	if report := d.reportOf(1); report.Task != testTask {
		t.Fatalf("the turn was reported %+v, want the turn of task %d", report, testTask)
	}
	r.quiet()
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
		t.Errorf("the bridge asked to take %q, want task 7 once", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"held r1"}) {
		t.Fatalf("Claude Code read %q, want the task's text once", texts)
	}
}

// The plan's decision 9 in the bridge: the task's text has reached Claude Code only once Claude
// Code re-emits it (--replay-user-messages), as an Envoy delivery has. A task's turn that ends with
// no echo of the task's own text is a text Claude Code dropped: that turn is reported naming no
// task and naming the task's number held, since the text never ran in it, which puts the take back
// on the daemon (decision 8) also where the daemon's count of the process's turns is not known;
// nothing is held finished, and the number is taken again at its value's next revision, whose
// accepted text is written once, at idle, and runs once, re-emitted, its turn's report naming the
// number and nothing held. An Envoy delivery's echo in the dropped text's turn is no echo of the
// task's.
func TestATaskTextClaudeCodeDoesNotReEmitIsTakenAgainAndRunsOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		card  bool
		texts []string
	}{
		{name: "no echo of the text", texts: []string{"drop r1", "say the task taken again"}},
		{name: "an Envoy delivery's echo only", card: true, texts: []string{"drop r1", "say the card", "say the task taken again"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			d.answerTake = func(take claim.TakeRequest) (int, any) {
				if take.Revision == 1 {
					return accepted("drop r1")
				}
				return accepted("say the task taken again")
			}
			r := newTakingRig(t, d, "", daemonCalls{})
			r.handReady()
			inbox := r.dialInbox()

			r.handTask(testTask, 1)
			r.expect("agent_start", nil)
			if tc.card {
				inbox.deliver("ev1", "say the card")
				r.logged("Envoy delivery ev1 waits for the next step")
			}
			r.release("r1")
			if tc.card {
				if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
					t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
				}
			}
			r.untilTurnEnds()
			if report := d.reportOf(1); report.Task != 0 || report.Held != testTask {
				t.Fatalf("the turn whose text Claude Code never re-emitted was reported %+v, want a turn of no task holding task %d", report, testTask)
			}
			r.logged("Claude Code ended task 7's turn without re-emitting its text")
			r.quiet()

			// The daemon writes the number's value again, as it does once it has taken the task
			// back: the take is made at that revision, and its text runs, once.
			r.handTask(testTask, 2)
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			if report := d.reportOf(2); report.Task != testTask || report.Held != 0 {
				t.Fatalf("the turn of the text taken again was reported %+v, want the turn of task %d holding nothing", report, testTask)
			}
			// That turn's report finishes the task: a later revision of its number runs nothing.
			r.handTask(testTask, 3)
			r.quiet()
			if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1", "7 at 2"}) {
				t.Errorf("the bridge asked to take %q, want task 7 at revision 1 and again at revision 2", takes)
			}
			if texts := r.userTexts(); !slices.Equal(texts, tc.texts) {
				t.Fatalf("Claude Code read %q, want %q", texts, tc.texts)
			}
		})
	}
}
