package daemon

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The shim's delivery dedupe, which answered a prompt under a delivery id it already carried from
// its own record, no longer exists: the daemon pushes no prompt, and a worker takes its task. These
// cases hold the take's own rule in its place. A take of a number the record holds as its accepted,
// unfinished take is a take sent again after its answer was lost, and the record answers it the
// text it kept (supervise's answeredFromRecord), across a daemon restart too. So the text the
// worker runs is the text of the accepted take, never a rendering the daemon made later. The copy
// that take carried is written at the accept and counts as a credit once the take's turn starts
// (20261122160000_design_news_credit_at_turn_start). Each case asserts three things: at the
// reopening, before X's turn, the record says version 3 with the opening owed, and the repeated
// take's answer carries no opening; after X's turn and its clean report it says version 4 with the
// opening still owed; and the next take, queued once X's turn has run, leads with the opening, and
// its finish leaves version 4 with nothing owed.

// repeatTake is the worker of machine's claim sending its take of the pending task again, as its
// plugin does after the answer was lost: the take of the same number, answered from the record.
func repeatTake(t *testing.T, r *newsPathRig, machine *supervise.Machine, taken supervise.Delivery) {
	t.Helper()
	held := machine.Claim()
	answer, err := machine.Take(r.ctx, supervise.RequestTake{Claim: held.Token, Generation: held.Generation, Session: held.Session,
		Number: taken.Number})
	if err != nil || answer.Outcome != supervise.TakeOutcomeAccepted || answer.Text != taken.TakenText {
		t.Fatalf("the take of task %d sent again = %+v, %v; want it answered the text its accepted take was, %q", taken.Number, answer, err, taken.TakenText)
	}
	if strings.Contains(answer.Text, carriedOpened) {
		t.Fatalf("the take of task %d sent again was answered %q, want no opening: the take's text carried none", taken.Number, answer.Text)
	}
	machine.Wait()
}

// theReopeningIsToldByTheNextTake runs the accepted take's turn and its clean report, which
// finishes the task and makes its credit final, so the record says version 4 taken and the opening
// that take's text did not carry still owed. The sweep then queues the reconcile of what is owed,
// which queues the reviewer's news-only start; the reviewer's take of that task leads with the
// opening, and the finish of that take's turn leaves nothing owed.
func theReopeningIsToldByTheNextTake(t *testing.T, r *newsPathRig, machine *supervise.Machine, conn *newsConn, token claim.Token) {
	t.Helper()
	r.startTurn(machine)
	endTurn(r.ctx, t, machine, token)
	if held := machine.Claim().Pending; held != nil {
		t.Fatalf("the reviewer holds %+v after its take's report, want the news task finished", held)
	}
	r.wantTold("after X's turn and its clean report", pathRoot, record.AudienceHolder, 4, true)

	before := len(conn.Prompts())
	r.sweepNews()
	r.queueNewsTask(pathRoot)
	told := r.delivered(machine)
	taken := promptsAfter(conn, before)
	if len(taken) != 1 || taken[0].DeliveryID != told.ID || !strings.Contains(taken[0].Message, carriedOpened) {
		t.Fatalf("the takes after the sweep = %+v, want the news task %s leading with the opening", taken, told.ID)
	}
	r.startTurn(machine)
	endTurn(r.ctx, t, machine, token)
	if held := machine.Claim().Pending; held != nil {
		t.Fatalf("the reviewer holds %+v after the opening task's report, want it finished", held)
	}
	r.wantTold("after the finish of the take that told the opening", pathRoot, record.AudienceHolder, 4, false)
}

// Path A: a restart. The reviewer's take of its news task X, the change list of version 4 and no
// opening, was accepted, and the daemon stops before X's turn starts. The take's copy is no credit
// yet, so when the gate closes and opens again the reviewer is still owed that news and its row
// owes the opening. The booted daemon restores the take with the text it kept, and the worker's
// take of X sent again is answered that same text, with no opening. X's turn and its report make
// X's credit final, version 4 taken and the opening owed, and the next take tells the opening.
func TestARestartsResendTheShimAnswersFromItsRecordRecordsNoNewsTheAgentNeverRead(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	reviewer, conn, token := r.reviewerBetweenTurns(pathRoot)
	before := len(conn.Prompts())
	r.queueNewsTask(pathRoot)
	first := r.delivered(reviewer)
	if taken := promptsAfter(conn, before); len(taken) != 1 || taken[0].DeliveryID != first.ID || strings.Contains(taken[0].Message, carriedOpened) {
		t.Fatalf("the takes = %+v, want the news task %s, without an opening", taken, first.ID)
	}

	r.closeAndReopen()
	r.wantTold("at the reopening, before X's turn", pathRoot, record.AudienceHolder, 3, true)
	r.restartDaemon()
	restored, restoredConn := r.reconnect(pathRoot, claim.RoleReviewer, false)
	r.hello(restored)

	repeatTake(t, r, restored, first)
	r.wantTold("after the take sent again across the restart", pathRoot, record.AudienceHolder, 3, true)
	theReopeningIsToldByTheNextTake(t, r, restored, restoredConn, token)
}

// Path B: one daemon. The reviewer's take of its news task X, the change list of version 4 and no
// opening, is accepted, and its answer is lost on the way back to the worker. The take's copy is
// no credit yet, so when the gate closes and opens again the reviewer is still owed that news and
// its row owes the opening. The worker sends its take of X again, and the record answers it the
// text it kept, with no opening. X's turn and its report make X's credit final, version 4 taken and
// the opening owed, and the next take tells the opening.
func TestAResendAfterALostAnswerTheShimAnswersFromItsRecordRecordsNoNewsTheAgentNeverRead(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	reviewer, conn, token := r.reviewerBetweenTurns(pathRoot)
	r.queueNewsTask(pathRoot)
	first := r.delivered(reviewer)
	if strings.Contains(first.TakenText, carriedOpened) {
		t.Fatalf("the reviewer's take of %s was answered %q, want no opening: the case's premise", first.ID, first.TakenText)
	}

	r.closeAndReopen()
	r.wantTold("at the reopening, before X's turn", pathRoot, record.AudienceHolder, 3, true)

	repeatTake(t, r, reviewer, first)
	r.wantTold("after the take sent again after the lost answer", pathRoot, record.AudienceHolder, 3, true)
	theReopeningIsToldByTheNextTake(t, r, reviewer, conn, token)
}
