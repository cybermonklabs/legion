package daemon

// One delivery at each move of the design gate (dispatch://INFRA-2040 R8, D16 of its plan). Each
// case drives the held-merger rig (newHeldMergerRig) over a real PostgreSQL: the daemon's fact
// handlers, which write each merger's delivery in the transaction of the fact that moves the gate,
// the outbox's executor and its fence, a real idle release, and the claim's machine asking the
// daemon's own answers before every send, the gate's note and message among them
// (workflowRuntime.gateNote). Each reads what the merger's session receives.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// heldMergerOpenedMessage is the opening message of the held-merger rig's tree: the person's
	// approval of the spec's version 2, which the live merger is sent as a role message and the
	// gate's own task carries at a send after the opening (workflow.MergerGateMessage).
	heldMergerOpenedMessage = "The design gate of LEGION-208 opened: a person approved version 2 of its spec document (" +
		heldMergerSpec + "), and every gated step that someone registered now stands open, so the design you are working to " +
		"is the approved one. A merge the closed gate held is released."
	// heldMergerGateTask is the task of the gate's own start of the rig's merger, which the gate's
	// message goes in front of at its send.
	heldMergerGateTask = "The design gate of LEGION-208 moved. The text at the start of this task gives the state of the gate " +
		"at the time the daemon sent you this task. Do what that text says."
	// heldMergerSettled is what the task of the CI wake of the rig's merger says of its cause.
	heldMergerSettled = "The wake is a CI settlement on your pull request's head: " + heldMergerHead
	// gateDeliveryLine is the engine's line for each merger's delivery of a move of the gate.
	gateDeliveryLine = `msg="workflow: a held merger's gate delivery"`
)

// roleTopicMessages is every message the outbox published to the rig's merger's role topic.
func (r *waitDeliveryRig) roleTopicMessages() []string {
	var messages []string
	for _, publish := range r.runner.notices.(*outboxPublisher).publishes() {
		if publish.topic == notify.RoleTopicPrefix+string(r.token) {
			messages = append(messages, publish.message)
		}
	}
	return messages
}

// queuedRoleMessages is every role message row the outbox holds for the rig's merger's topic.
func (r *waitDeliveryRig) queuedRoleMessages() int {
	r.t.Helper()
	var queued int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where kind = 'role_message' and issue = $1 and payload->>'role' = $2`,
		r.issue.Key, string(r.token)).Scan(&queued); err != nil {
		r.t.Fatalf("read the role messages queued for the merger: %v", err)
	}
	return queued
}

// wantGateMessage asserts the merger's session received sent opening with message, the gate's own
// message at the send, in front of the gate's own task, and nothing that says the other state.
func wantGateMessage(t *testing.T, sent fake.Prompt, message, says, why string) {
	t.Helper()
	if !strings.HasPrefix(sent.Message, message+"\n\n") || !strings.Contains(sent.Message, says) {
		t.Fatalf("the merger's session received %q, want %q in front of a task saying %q: %s", sent.Message, message, says, why)
	}
	if message == heldMergerOpenedMessage || message == heldMergerOpenNote {
		if strings.Contains(sent.Message, "The design gate of LEGION-208 closed") {
			t.Fatalf("the merger's session received %q, which says the gate is closed after it opened: %s", sent.Message, why)
		}
	}
}

// A merger with a live session receives the closing message as the move's role message, and its
// text holds the merge as every task of the hold does (R8): it names the closed gate, says to merge
// nothing and to take a queued pull request back out of the merge queue. No start is written for
// the live merger, and the delivery is logged as the role message.
func TestALiveMergerIsSentTheClosingMessageThatHoldsTheMerge(t *testing.T) {
	r := newHeldMergerRig(t)
	r.mergerWorked()
	sent := len(r.prompts())
	told := len(r.roleTopicMessages())

	r.closesTheGate()
	messages := r.roleTopicMessages()[told:]
	if len(messages) != 1 || messages[0] != heldMergerClosedNote {
		t.Fatalf("the live merger's role topic was sent %q at the close, want the closing message once: %q", messages, heldMergerClosedNote)
	}
	for _, says := range []string{"The design gate of LEGION-208 closed: version 2 of its spec document (" + heldMergerSpec + ")",
		"Merge nothing", "take a pull request you queued back out of the merge queue"} {
		if !strings.Contains(messages[0], says) {
			t.Fatalf("the live merger's closing message %q does not say %q", messages[0], says)
		}
	}
	if starts := entryStartRows(t, r.pool, r.issue.Key); len(starts) != 0 {
		t.Fatalf("the start rows after the close = %+v, want none: the live merger's delivery is the role message", starts)
	}
	if got := len(r.prompts()); got != sent {
		t.Fatalf("the live merger was sent %d prompts at the close, want none", got-sent)
	}
	if lines := r.lines(gateDeliveryLine, "issue="+r.issue.Key, "move=closed", `choice="role message"`); len(lines) != 1 {
		t.Fatalf("the live merger's delivery was logged %d times as the role message, want once; the log is:\n%s", len(lines), r.logs)
	}
}

// awayMerger is one state the rig's merger can stand in at a move of the gate with no live
// session a role message reaches.
type awayMerger struct {
	name string
	// away puts the merger, at work and between turns, into the state, at at.
	away func(r *waitDeliveryRig, at time.Time)
	// held says the merger holds a CI wake's task it has not taken: its first task, which the
	// gate's own start follows.
	held bool
	// queued says the CI wake's start is still queued: it carries a task and runs first, so it is
	// the move's one delivery and the gate writes no start of its own.
	queued bool
}

// awayMergers are the states R8 names a merger with no live session in: its pod released, its
// pod's release still in progress, its relaunch in flight with a task, its relaunch in flight with
// none (the resume of a cancelled idle release, and a relaunch after a death between turns), and
// its earlier wake not yet run.
func awayMergers() []awayMerger {
	return []awayMerger{
		{name: "its pod was released", away: func(r *waitDeliveryRig, at time.Time) { r.releasesAt(at) }},
		{name: "its pod's release is still in progress", away: func(r *waitDeliveryRig, _ time.Time) {
			r.markRelease()
			if !r.releaseMarked() {
				r.t.Fatal("the release is not marked, want it in progress")
			}
		}},
		{name: "its relaunch is in flight with a task", held: true, away: func(r *waitDeliveryRig, at time.Time) {
			r.releasesAt(at)
			r.applies("settled", heldMergerSettlement())
			r.drainAt(at.Add(time.Minute), record.OutboxKindSupervise)
			if got := r.machine.Claim(); r.live() || got.Pending == nil || !strings.Contains(got.Pending.Task, heldMergerSettled) {
				r.t.Fatalf("the merger after its CI wake ran = %s holding %+v, want its relaunch in flight with the wake's task", got.State, got.Pending)
			}
		}},
		{name: "its relaunch is in flight with none: the resume of a cancelled idle release", away: func(r *waitDeliveryRig, at time.Time) {
			r.markRelease()
			r.drainAt(at, record.OutboxKindIssueBundle, record.OutboxKindSupervise)
			if err := r.runner.cancelRelease(r.ctx, r.issue.Key, r.issue.Tree, workflow.IdleHoldBundleSave, "the case cancels the release"); err != nil {
				r.t.Fatalf("cancel the release: %v", err)
			}
			r.drainAt(at.Add(time.Minute), record.OutboxKindIssueRelease, record.OutboxKindSupervise)
			r.wantRelaunchWithNoTask("the resume of the cancelled release")
		}},
		{name: "its relaunch is in flight with none: a relaunch after a death between turns", away: func(r *waitDeliveryRig, _ time.Time) {
			r.handle(supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.machine.Claim().Locator,
				Kind: runtime.Gone, At: time.Now()}})
			r.wantRelaunchWithNoTask("the death between turns")
		}},
		{name: "its earlier wake has not reached it", queued: true, away: func(r *waitDeliveryRig, at time.Time) {
			r.releasesAt(at)
			r.applies("settled", heldMergerSettlement())
			if starts := entryStartRows(r.t, r.pool, r.issue.Key); len(starts) != 1 || starts[0].request.Phase != "" {
				r.t.Fatalf("the start rows after the CI wake = %+v, want its one start, not yet run", starts)
			}
		}},
	}
}

// live is whether the rig's merger runs a session a prompt reaches now: ready or idle.
func (r *waitDeliveryRig) live() bool {
	state := r.machine.Claim().State
	return state == supervise.StateReady || state == supervise.StateIdle
}

// wantRelaunchWithNoTask asserts the rig's merger is being relaunched, after what, and holds no
// task.
func (r *waitDeliveryRig) wantRelaunchWithNoTask(after string) {
	r.t.Helper()
	if got := r.machine.Claim(); r.live() || got.Released() || got.State == supervise.StateSuspended || got.Pending != nil {
		r.t.Fatalf("the merger after %s = %s by %s holding %+v, want its relaunch in flight with no task", after, got.State, got.SuspendedBy, got.Pending)
	}
}

// At the close and at the opening, a merger with no live session gets one delivery, written in the
// transaction of the fact that moved the gate, and its first task after the move gives the gate's
// state (R8). For each state of awayMergers and each move, the merger works, the gate closes first
// for the opening, the merger goes into the state and the gate moves:
//
//   - the move queues no role message for the merger, so there is nothing for a listener to report
//     a wake of, and the move's fact writes the gate's own start, unless the merger's earlier wake
//     is still queued, which is then its one delivery;
//   - the first task the merger's session receives opens with the gate's state at that send: the
//     gate's own message (the closing message, or the opening message), or the note of the held or
//     queued CI wake's task;
//   - a merger that holds the CI wake's task untaken gets that task first, and the gate's start
//     follows it with the gate's message;
//   - nothing else follows: no second task for the move, and at the opening no backstop is left.
//
// The opening's two cases of a pod released during the hold and of one whose release is in
// progress are c-gate-hold's case of the listener's wake report that was dropped: the opening's
// start is written in the fact that tells the tree, and its task reaches the merger with no wake
// report at all.
func TestEachMergerAwayFromItsSessionGetsOneGateDeliveryAtTheCloseAndAtTheOpening(t *testing.T) {
	for _, open := range []bool{false, true} {
		move, message, note := "closed", heldMergerClosedNote, heldMergerClosedNote
		if open {
			move, message, note = "opened", heldMergerOpenedMessage, heldMergerOpenNote
		}
		for _, state := range awayMergers() {
			t.Run(fmt.Sprintf("the gate %s: %s", move, state.name), func(t *testing.T) {
				r := newHeldMergerRig(t)
				r.mergerWorked()
				if open {
					r.closesTheGate()
				}
				at := time.Now().UTC().Add(time.Minute)
				state.away(r, at)
				sent, before := len(r.prompts()), len(entryStartRows(t, r.pool, r.issue.Key))
				told := len(r.roleTopicMessages())

				if open {
					r.opensTheGate()
				} else {
					r.closesTheGate()
				}
				starts := entryStartRows(t, r.pool, r.issue.Key)
				if queued := r.queuedRoleMessages(); queued != 0 {
					t.Fatalf("the move queued %d role messages for the merger, want none: no live session reads one", queued)
				}
				// The start's and the log's checks let the case go on, so a move that writes no
				// delivery also shows what the merger's session then receives.
				choice := `choice="gate start"`
				if state.queued {
					choice = `choice="queued start"`
					if len(starts) != before {
						t.Errorf("the start rows after the move = %+v, want only the earlier wake's: it is the move's one delivery", starts)
					}
				} else if len(starts) != before+1 || !starts[before].request.GateMessage || starts[before].request.Phase != "" {
					t.Errorf("the start rows after the move = %+v, want the gate's own start written in the move's fact", starts)
				}
				if lines := r.lines(gateDeliveryLine, "issue="+r.issue.Key, "move="+move, choice); len(lines) != 1 {
					t.Errorf("the merger's delivery was logged %d times as %s, want once: %q", len(lines), choice, r.lines(gateDeliveryLine))
				}
				if open {
					if armed := openBackstops(t, r.pool, r.issue.Key); len(armed) != 0 {
						t.Fatalf("the backstops after the opening = %+v, want none: the opening's delivery is the merger's one task for it", armed)
					}
				}

				later := at.Add(2 * time.Minute)
				r.drainAt(later, record.OutboxKindIssueBundle, record.OutboxKindSupervise, record.OutboxKindIssueRelease, record.OutboxKindRoleMessage)
				if got := r.machine.Claim(); got.Released() {
					t.Fatalf("the merger is still released after the gate %s and the outbox ran its rows: it sleeps, and no listener's wake report will bring it back", move)
				}
				if !r.live() {
					r.boot()
				}
				first := r.wantPrompts(sent+1, "the merger's first task after the move reaches its session")[sent]
				last := first
				if state.held || state.queued {
					wantGateMessage(t, first, note, heldMergerSettled, "the CI wake's task carries the gate's state at its send")
				} else {
					wantGateMessage(t, first, message, heldMergerGateTask, "the gate's own task carries the gate's message at its send")
				}
				if state.held {
					r.takeTask()
					r.endTurn(first.DeliveryID)
					later = later.Add(time.Hour)
					r.drainAt(later, record.OutboxKindSupervise)
					last = r.wantPrompts(sent+2, "the gate's own start follows the held task")[sent+1]
					wantGateMessage(t, last, message, heldMergerGateTask, "the gate's start that waited behind the held task")
				}
				r.takeTask()
				r.endTurn(last.DeliveryID)
				r.drainAt(later.Add(time.Hour), record.OutboxKindSupervise, record.OutboxKindRoleMessage)
				want := sent + 1
				if state.held {
					want++
				}
				r.wantPrompts(want, "no second task follows the move's one delivery")
				if got := r.roleTopicMessages()[told:]; len(got) != 0 {
					t.Fatalf("the merger's role topic was sent %q after the move, want nothing", got)
				}
			})
		}
	}
}

// After a person's approval over a backstop that came due during the hold, the merger receives
// exactly one task for the opening, and the backstop is cancelled (R1, R8). The merger works, the
// gate closes over it, the idle release takes its pod, and a wake tick past the backstop's time
// leaves the backstop due, since the gate holds the merger. The approval then writes the gate's
// own wake, which records the merger's one resume and leaves no backstop: the relaunched merger is
// sent the opening message, and once the turn of that task has ended and closed the resume, a
// wake tick past the backstop's time wakes nothing and no second task follows.
func TestAnApprovalOverABackstopDueDuringTheHoldGivesTheMergerOneTaskForTheOpening(t *testing.T) {
	r := newHeldMergerRig(t)
	backstop := heldBackstopArmed(t, r, true)
	if resumes := r.backstopsFired(backstop.dueAt); len(resumes) != 0 {
		t.Fatalf("the pod resumes after the wake tick during the hold = %v, want none", resumes)
	}
	sent := len(r.prompts())

	r.opensTheGate()
	if armed := openBackstops(t, r.pool, r.issue.Key); len(armed) != 0 {
		t.Fatalf("the backstops after the approval = %+v, want none: the approval's delivery is the merger's task for the opening", armed)
	}
	if resumes := r.resumes(); len(resumes) != 1 || resumes[0] != "merger:"+pods.WakeDesignGate {
		t.Fatalf("the pod resumes after the approval = %v, want the gate's one wake of the merger", resumes)
	}
	r.drainAt(backstop.dueAt.Add(time.Minute), record.OutboxKindSupervise)
	r.boot()
	opening := r.wantPrompts(sent+1, "the relaunched merger receives the opening's task")[sent]
	wantGateMessage(t, opening, heldMergerOpenedMessage, heldMergerGateTask, "the approval's one delivery")
	r.takeTask()
	if _, err := r.pool.Exec(r.ctx, `update pod_resumes set first_turn_at = now() where issue = $1 and first_turn_at is null`,
		r.issue.Key); err != nil {
		t.Fatalf("stamp the opening's resume with its first turn: %v", err)
	}
	r.endTurn(opening.DeliveryID)

	if resumes := r.backstopsFired(backstop.dueAt.Add(2 * time.Hour)); len(resumes) != 1 {
		t.Fatalf("the pod resumes after a wake tick past the opening = %v, want the opening's one", resumes)
	}
	r.drainAt(backstop.dueAt.Add(3*time.Hour), record.OutboxKindSupervise)
	r.wantPrompts(sent+1, "no second task for the opening follows the approval's one")
}
