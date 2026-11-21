package daemon

// A merger's wait under the design gate's hold (dispatch://INFRA-2040 R3, R7 and R8; t18 of its
// plan), end to end through the held-merger rig (newHeldMergerRig) over a real PostgreSQL: the
// wait route a merger declares through, the daemon's fact handlers that close and open the gate,
// end the wait and write each merger's delivery of a move, the outbox's executor and its fence, a
// real idle release, and the claim's machine asking the daemon's own answers before every send,
// the gate's note among them (workflowRuntime.gateNote). Each case reads what the merger's session
// receives.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// heldMergerClosedBy is what ends a held-merger rig's merger's wait at the gate's closing: the
// fact of the spec's version 2 (closesTheGate).
const heldMergerClosedBy = "dispatch/spec-version-2"

// mergerWaiting puts the held-merger rig's merger to work on its merging task while the gate
// stands open, declares its wait in that turn and ends the turn, and answers the wait.
func (r *waitDeliveryRig) mergerWaiting() record.ClaimWait {
	r.t.Helper()
	taken := r.working(1, heldMergerTask)
	waited := r.declareUntil("the merge queue's run on the head", time.Now().Add(2*time.Hour))
	r.endTurn(taken.ID)
	return waited
}

// wantTimeOfAnEndedWaitSendsNothing asserts the time of wait, which ended before it, sends the
// merger nothing: its wait_due row runs, writes no start, and the session receives no prompt past
// the sent it had.
func (r *waitDeliveryRig) wantTimeOfAnEndedWaitSendsNothing(wait record.ClaimWait, sent int) {
	r.t.Helper()
	ran := r.drainAt(wait.WakeAt, waitTimeKinds...)
	if len(ran) != 1 || ran[0].Kind != record.OutboxKindWaitDue {
		r.t.Fatalf("the outbox ran %+v at the time of the ended wait %s, want its wait_due row alone", ran, wait.ID)
	}
	r.wantPrompts(sent, "the time of a wait the gate's closing ended sends nothing")
}

// wantClosedGateOnly asserts sent opens with the closed gate's note, in front of a task saying
// says, and that nothing in it says the gate stands open or opened: the session reads one state of
// the gate, the one at the send.
func wantClosedGateOnly(t *testing.T, sent fake.Prompt, says, why string) {
	t.Helper()
	wantClosedNote(t, sent, says, why)
	for _, open := range []string{"The design gate of LEGION-208 stands open", "The design gate of LEGION-208 opened"} {
		if strings.Contains(sent.Message, open) {
			t.Fatalf("the merger's session received %q while the gate is closed, which says %q: %s", sent.Message, open, why)
		}
	}
}

// The design gate's closing ends a merger's wait (R3's seventh end) with the cause that the design
// gate closed, ended by the fact that closed it, whether the merger's pod runs or an idle release
// took it, and each move of the gate reaches the merger with its one delivery (R8). The merger
// takes its merging task while the gate stands open, declares a wait in that turn and ends the
// turn. Then:
//
//   - with a live pod, the close reaches the merger as the role message, which holds the merge,
//     and the opening as the role message that says the gate opened; neither writes a start or
//     sends a task;
//   - with a released pod, which the release left with its wait standing and no backstop, the
//     closing's own wake brings the merger back, and the first task its session receives names the
//     closed gate; the idle release takes the pod again during the hold, which arms the backstop,
//     and the opening's own wake brings the merger back with the opening message, its one task for
//     the opening, and leaves no backstop; neither move publishes a role message to the merger.
//
// In both, the time of the ended wait sends the merger nothing.
func TestTheDesignGatesClosingEndsAMergersWaitWithALiveOrAReleasedPod(t *testing.T) {
	t.Run("a live pod", func(t *testing.T) {
		r := newHeldMergerRig(t)
		waited := r.mergerWaiting()
		sent, told := len(r.prompts()), len(r.roleTopicMessages())

		r.closesTheGate()
		r.wantEnded(waited, workflow.WaitEndDesignGateClosed, heldMergerClosedBy)
		if got := r.roleTopicMessages()[told:]; len(got) != 1 || got[0] != heldMergerClosedNote {
			t.Fatalf("the live merger's role topic was sent %q at the close, want the closing message once: %q", got, heldMergerClosedNote)
		}

		r.opensTheGate()
		r.drainAt(time.Now().UTC(), record.OutboxKindRoleMessage, record.OutboxKindNotice)
		if got := r.roleTopicMessages()[told:]; len(got) != 2 || got[1] != heldMergerOpenedMessage {
			t.Fatalf("the live merger's role topic was sent %q by the two moves, want the closing message and then the opening message: %q",
				got, heldMergerOpenedMessage)
		}
		if starts := entryStartRows(t, r.pool, r.issue.Key); len(starts) != 0 {
			t.Fatalf("the start rows after the two moves = %+v, want none: the live merger's delivery of each is the role message", starts)
		}
		r.wantPrompts(sent, "the live merger's deliveries of the two moves are role messages, not tasks")
		r.wantTimeOfAnEndedWaitSendsNothing(waited, sent)
	})

	t.Run("a released pod", func(t *testing.T) {
		r := newHeldMergerRig(t)
		waited := r.mergerWaiting()
		released := time.Now().UTC().Add(time.Minute)
		r.releasesAt(released)
		r.wantStanding()
		if armed := openBackstops(t, r.pool, r.issue.Key); len(armed) != 0 {
			t.Fatalf("the backstops after the release of the waiting merger = %+v, want none", armed)
		}
		sent, told := len(r.prompts()), len(r.roleTopicMessages())

		r.closesTheGate()
		r.wantEnded(waited, workflow.WaitEndDesignGateClosed, heldMergerClosedBy)
		if resumes := r.resumes(); len(resumes) != 1 || resumes[0] != "merger:"+pods.WakeDesignGate {
			t.Fatalf("the pod resumes after the close = %v, want the closing's one wake of the merger", resumes)
		}
		r.drainAt(released.Add(time.Minute), record.OutboxKindSupervise)
		r.boot()
		closing := r.wantPrompts(sent+1, "the closing's wake reaches the merger once its pod is back")[sent]
		wantGateMessage(t, closing, heldMergerClosedNote, heldMergerGateTask, "the closing's own task names the closed gate")
		r.takeTask()
		r.endTurn(closing.DeliveryID)

		again := released.Add(2 * time.Minute)
		r.releasesAt(again)
		if armed := openBackstops(t, r.pool, r.issue.Key); len(armed) != 1 {
			t.Fatalf("the backstops after the release during the hold = %+v, want the merger's one: its wait has ended", armed)
		}
		r.opensTheGate()
		if armed := openBackstops(t, r.pool, r.issue.Key); len(armed) != 0 {
			t.Fatalf("the backstops after the opening = %+v, want none: the opening's delivery is the merger's one task for it", armed)
		}
		if resumes := r.resumes(); len(resumes) != 2 || resumes[1] != "merger:"+pods.WakeDesignGate {
			t.Fatalf("the pod resumes after the opening = %v, want the closing's wake and then the opening's", resumes)
		}
		r.drainAt(again.Add(time.Minute), record.OutboxKindSupervise, record.OutboxKindRoleMessage)
		r.boot()
		opening := r.wantPrompts(sent+2, "the opening's wake reaches the merger once its pod is back")[sent+1]
		wantGateMessage(t, opening, heldMergerOpenedMessage, heldMergerGateTask, "the opening's own task gives the opening message")
		r.takeTask()
		r.endTurn(opening.DeliveryID)

		r.drainAt(again.Add(time.Hour), record.OutboxKindSupervise, record.OutboxKindRoleMessage)
		r.wantPrompts(sent+2, "no second task follows the opening's one delivery")
		if got := r.roleTopicMessages()[told:]; len(got) != 0 {
			t.Fatalf("the released merger's role topic was sent %q by the two moves, want nothing: each move's delivery is its own wake", got)
		}
		r.wantTimeOfAnEndedWaitSendsNothing(waited, sent+2)
	})
}

// A merger's wait reaches its time and the daemon writes the wait's own task, and then the design
// gate closes before that task is sent: the session receives the task while the gate is closed,
// and the task opens with the closed gate's note (R7). The task names no phase, so the outbox's
// fence lets it through the hold (D14 of the plan), and its note is built at the send, not where
// the wait's time wrote the task (D15): when the time came the gate stood open.
//
//   - With a live pod, the wait's own start is queued at its time and runs after the close, and
//     the live merger's delivery of the close is the role message.
//   - With a released pod, the wait's time wakes the pod, and the close finds that wake's start
//     still queued: it carries a task and names no phase, so it is the close's one delivery, and
//     the close writes no start of its own.
func TestAMergersWaitsOwnTaskSentAfterTheGateClosedNamesTheClosedGate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		released bool
		// choice is the merger's delivery of the close, as the engine logs it.
		choice string
	}{
		{name: "a live pod", choice: `choice="role message"`},
		{name: "a released pod", released: true, choice: `choice="queued start"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHeldMergerRig(t)
			waited := r.mergerWaiting()
			if tc.released {
				r.releasesAt(time.Now().UTC().Add(time.Minute))
			}
			// The checks of the wait's own start and of the close's delivery let the case go on, so
			// a start the outbox would not deliver also shows what the merger's session receives.
			ran := r.drainAt(waited.WakeAt, record.OutboxKindWaitDue)
			starts := entryStartRows(t, r.pool, r.issue.Key)
			came := []string{fmt.Sprintf("wake=%t", tc.released)}
			if len(ran) != 1 || len(starts) != 1 {
				t.Errorf("at the wait's time the outbox ran %+v and holds the starts %+v, want the wait's row and then one start of its task", ran, starts)
			} else {
				came = append(came, fmt.Sprintf("row=%d", starts[0].row.ID))
			}
			r.wantTimeCame(waited, came...)
			sent := len(r.prompts())

			r.closesTheGate()
			if lines := r.lines(gateDeliveryLine, "issue="+r.issue.Key, "move=closed", tc.choice); len(lines) != 1 {
				t.Errorf("the merger's delivery of the close was logged %d times as %s, want once: %q", len(lines), tc.choice, r.lines(gateDeliveryLine))
			}
			if after := entryStartRows(t, r.pool, r.issue.Key); len(after) != len(starts) || len(after) == 1 && after[0].row.ID != starts[0].row.ID {
				t.Errorf("the start rows after the close = %+v, want the wait's own start alone: the close writes no start of its own", after)
			}
			r.drainAt(waited.WakeAt.Add(time.Minute), record.OutboxKindSupervise)
			if !r.live() {
				r.boot()
			}
			first := r.wantPrompts(sent+1, "the session receives the wait's own task while the gate is closed")[sent]
			wantClosedGateOnly(t, first, "The time of your wait "+waited.ID+" came.",
				"the wait's own task is sent after the gate closed, and its note is built at the send")
			wantWaitTask(t, first, waited)
			if lines := r.lines(gateNoteSentLine, "issue="+r.issue.Key, "delivery="+first.DeliveryID, "state=closed", "step=spec",
				"version=2"); len(lines) != 1 {
				t.Fatalf("the send of the wait's own task was logged %d times as carrying the closed gate, want once; the log is:\n%s", len(lines), r.logs)
			}
		})
	}
}
