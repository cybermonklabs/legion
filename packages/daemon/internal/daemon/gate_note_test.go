package daemon

// The design gate's state in every task of a held merger, built at the send (dispatch://INFRA-2040
// R7, D15 of its plan). Each case drives the held-merger rig (newHeldMergerRig) over a real
// PostgreSQL: the daemon's fact handlers, the outbox's executor and its fence, a real idle
// release, and the claim's machine asking the daemon's own answers before every send, the gate's
// note among them (workflowRuntime.gateNote). Each reads the merger's first task after the move
// as its session receives it: the prompt the relaunched or live process was sent.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	// heldMergerClosedNote is the note a task of the held-merger rig's merger carries while the
	// spec's version 2 is with its reviewer, and heldMergerOpenNote the one it carries once the
	// gate stands open (workflow.MergerGateNote).
	heldMergerClosedNote = "The design gate of LEGION-208 closed: version 2 of its spec document (" + heldMergerSpec +
		") is with its reviewer. Merge nothing, enqueue nothing, and take a pull request you queued back out of the merge queue."
	heldMergerOpenNote = "The design gate of LEGION-208 stands open: every gated step that someone registered is approved."
	// gateNoteSentLine is the machine's line for each send of a merger's task that carries a note.
	gateNoteSentLine = `msg="supervise: the merger's task carries the design gate's state"`
)

// wantClosedNote asserts the merger's session received sent opening with the closed gate's note:
// the closed gate, to merge nothing, and to take a queued pull request back out of the merge
// queue, and then what the task says (says).
func wantClosedNote(t *testing.T, sent fake.Prompt, says string, why string) {
	t.Helper()
	if !strings.HasPrefix(sent.Message, heldMergerClosedNote+"\n\n") || !strings.Contains(sent.Message, says) {
		t.Fatalf("the merger's session received %q, want the closed gate's note in front of a task saying %q: %s", sent.Message, says, why)
	}
}

// The merger's first task after a move of the design gate gives the gate's state as it stands at
// the send, whenever the task was queued (R7). Each order wakes a merger whose pod the idle
// release took, whose start names no phase and so passes the outbox's fence, and reads the first
// task the relaunched session receives:
//
//  1. the gate closes before the outbox runs a CI wake's row: the task names the closed gate;
//  2. the outbox has run that row, so the task is queued and the relaunch is in flight, and then
//     the gate closes: the task names the closed gate all the same, since its note is built at the
//     send and not where the outbox queued the task;
//  3. the wake comes during the hold, the outbox runs its row, and the gate opens while the
//     relaunch is in flight: the task says the gate stands open, and nothing in it says closed;
//     the session then receives the opening message, which the gate's own start carries (R8):
//     the opening wrote it behind the task the merger held and had not taken;
//  4. the gate closes over the released merger, the outbox runs the row of the closing's own wake,
//     and the gate opens while that relaunch is in flight: the closing wake's task, whose message is
//     built at its send, gives the opening message, and the opening's start follows it with the
//     opening message.
func TestAHeldMergersFirstTaskGivesTheDesignGateAsItStandsAtItsSend(t *testing.T) {
	const settled = "The wake is a CI settlement on your pull request's head: " + heldMergerHead
	for _, tc := range []struct {
		name string
		// closedBeforeWake closes the gate before the settlement wakes the merger.
		closedBeforeWake bool
		// moves is what happens between the settlement's wake and the relaunched session's ready:
		// the gate's move and the outbox's run of the wake's row, in the case's order.
		moves func(r *waitDeliveryRig, at time.Time)
		// want asserts the first task the relaunched session received.
		want func(t *testing.T, r *waitDeliveryRig, first fake.Prompt)
		// after, when set, ends the first task's turn and asserts what the session receives after
		// it, the prompts before numbering sent, with the outbox's rows run at at.
		after func(t *testing.T, r *waitDeliveryRig, first fake.Prompt, sent int, at time.Time)
	}{
		{
			name: "the gate closes before the outbox runs the wake's row",
			moves: func(r *waitDeliveryRig, at time.Time) {
				r.closesTheGate()
				r.drainAt(at, record.OutboxKindSupervise)
			},
			want: func(t *testing.T, r *waitDeliveryRig, first fake.Prompt) {
				wantClosedNote(t, first, settled, "the gate was closed when the task was sent")
			},
		},
		{
			name: "the gate closes while the wake's relaunch is in flight",
			moves: func(r *waitDeliveryRig, at time.Time) {
				r.drainAt(at, record.OutboxKindSupervise)
				queued := r.machine.Claim()
				if queued.State == supervise.StateReady || queued.State == supervise.StateIdle || queued.Pending == nil ||
					!strings.Contains(queued.Pending.Task, settled) || !queued.Pending.DeliveredAt.IsZero() {
					r.t.Fatalf("the merger after the outbox ran the wake's row = %s holding %+v, want its relaunch in flight with the wake's task queued and not sent",
						queued.State, queued.Pending)
				}
				r.closesTheGate()
			},
			want: func(t *testing.T, r *waitDeliveryRig, first fake.Prompt) {
				wantClosedNote(t, first, settled, "the note is built at the send, after the gate closed, not where the outbox queued the task")
				if lines := r.lines(gateNoteSentLine, "issue="+r.issue.Key, "delivery="+first.DeliveryID, "state=closed", "step=spec",
					"version=2"); len(lines) != 1 {
					t.Fatalf("the send was logged %d times as carrying the closed gate, want once; the log is:\n%s", len(lines), r.logs)
				}
			},
		},
		{
			name: "the gate opens while the relaunch of a wake during the hold is in flight", closedBeforeWake: true,
			moves: func(r *waitDeliveryRig, at time.Time) {
				r.drainAt(at, record.OutboxKindSupervise)
				if got := r.machine.Claim(); got.State == supervise.StateReady || got.State == supervise.StateIdle || got.Pending == nil {
					r.t.Fatalf("the merger after the outbox ran the wake's row = %s holding %+v, want its relaunch in flight with the wake's task",
						got.State, got.Pending)
				}
				r.opensTheGate()
			},
			want: func(t *testing.T, r *waitDeliveryRig, first fake.Prompt) {
				if !strings.HasPrefix(first.Message, heldMergerOpenNote+"\n\n") || !strings.Contains(first.Message, settled) ||
					strings.Contains(first.Message, "The design gate of LEGION-208 closed") {
					t.Fatalf("the merger's session received %q, want the open gate's note in front of the wake's task, and nothing that says the gate is closed",
						first.Message)
				}
			},
			after: func(t *testing.T, r *waitDeliveryRig, first fake.Prompt, sent int, at time.Time) {
				r.takeTask()
				r.endTurn(first.DeliveryID)
				r.drainAt(at, record.OutboxKindSupervise)
				opening := r.wantPrompts(sent+2, "the opening's start follows the wake's task")[sent+1]
				wantGateMessage(t, opening, heldMergerOpenedMessage, heldMergerGateTask, "the gate's own start carries the opening message")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHeldMergerRig(t)
			r.mergerWorked()
			if tc.closedBeforeWake {
				r.closesTheGate()
			}
			released := time.Now().UTC().Add(time.Minute)
			r.releasesAt(released)
			r.applies("settled", heldMergerSettlement())
			if resumes := r.resumes(); len(resumes) != 1 || resumes[0] != "merger:"+pods.WakeChecks {
				t.Fatalf("the pod resumes after the settlement = %v, want its one wake of the merger", resumes)
			}
			sent := len(r.prompts())

			tc.moves(r, released.Add(time.Minute))
			r.boot()
			prompts := r.wantPrompts(sent+1, "the relaunched session receives the wake's task")
			tc.want(t, r, prompts[sent])
			if tc.after != nil {
				tc.after(t, r, prompts[sent], sent, released.Add(time.Hour))
			}
		})
	}

	t.Run("the closing's wake runs and the gate opens while its relaunch is in flight", func(t *testing.T) {
		r := newHeldMergerRig(t)
		r.mergerWorked()
		released := time.Now().UTC().Add(time.Minute)
		r.releasesAt(released)
		sent := len(r.prompts())
		r.closesTheGate()
		if resumes := r.resumes(); len(resumes) != 1 || resumes[0] != "merger:"+pods.WakeDesignGate {
			t.Fatalf("the pod resumes after the close = %v, want the closing's one wake of the merger", resumes)
		}
		r.drainAt(released.Add(time.Minute), record.OutboxKindSupervise)
		if got := r.machine.Claim(); r.live() || got.Pending == nil || !strings.Contains(got.Pending.Task, heldMergerGateTask) || !got.Pending.DeliveredAt.IsZero() {
			t.Fatalf("the merger after the outbox ran the closing wake's row = %s holding %+v, want its relaunch in flight with the gate's task queued and not sent",
				got.State, got.Pending)
		}
		r.opensTheGate()
		r.boot()
		first := r.wantPrompts(sent+1, "the relaunched session receives the closing wake's task")[sent]
		wantGateMessage(t, first, heldMergerOpenedMessage, heldMergerGateTask, "the closing wake's message is built at its send, after the opening")
		r.takeTask()
		r.endTurn(first.DeliveryID)
		r.drainAt(released.Add(time.Hour), record.OutboxKindSupervise)
		opening := r.wantPrompts(sent+2, "the opening's start follows the closing wake's task")[sent+1]
		wantGateMessage(t, opening, heldMergerOpenedMessage, heldMergerGateTask, "the opening's own start carries the opening message")
	})
}

// Every task a held merger's session receives from the daemon names the closed gate (R7), not
// only a wake's: the gate closes over the merger, and then
//
//   - its process dies in the turn of its merging task, and the relaunched session is sent that
//     task again, taken back from the turn;
//   - the update round opens over its pod and the round's ask is published to its role, a role
//     message and no task, which opens with the same note;
//   - an operator delivers it a task through the operator's route.
func TestEveryTaskAHeldMergersSessionReceivesNamesTheClosedGate(t *testing.T) {
	t.Run("the held task sent again after a death during the hold", func(t *testing.T) {
		r := newHeldMergerRig(t)
		taken := r.working(1, heldMergerTask)
		r.closesTheGate()
		sent := len(r.prompts())
		r.handle(supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.machine.Claim().Locator,
			Kind: runtime.Gone, At: time.Now()}})
		r.boot()
		resent := r.wantPrompts(sent+1, "the relaunched session is sent the task its death took back")[sent]
		wantClosedNote(t, resent, heldMergerTask, "the task taken back from the turn is sent again during the hold")
		if held := r.machine.Claim().Pending; held == nil || held.ID != resent.DeliveryID || held.ID == taken.ID || held.Task != heldMergerTask {
			t.Fatalf("the merger holds %+v, want the task taken back under a new id, as it was queued, with no note stored", held)
		}
	})

	t.Run("the update round's ask", func(t *testing.T) {
		r := newHeldMergerRig(t)
		r.mergerWorked()
		r.closesTheGate()
		const (
			round = "pod:held-merger-round"
			ask   = "The daemon must replace the pod that runs this issue's roles. The reasons are: image a, now b."
		)
		r.handle(supervise.PodUpdateOpen{Claim: r.token, Round: round, Reasons: "image a, now b"})
		if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
			return r.records.Enqueue(r.ctx, tx, mustOutboxRow(t, r.issue.Key,
				record.UpdateAsk{Role: r.role, Round: round, Pod: "held-merger-pod", Text: ask}, time.Now()))
		}); err != nil {
			t.Fatalf("queue the round's ask: %v", err)
		}
		r.drainAt(time.Now().UTC(), record.OutboxKindUpdateAsk)
		key := record.UpdateAskKey(r.token, round, r.machine.Claim().UpdateOpenedAt)
		var asks []outboxPublish
		for _, publish := range r.runner.notices.(*outboxPublisher).publishes() {
			if publish.topic == notify.RoleTopicPrefix+string(r.token) && publish.key == key {
				asks = append(asks, publish)
			}
		}
		if want := heldMergerClosedNote + "\n\n" + ask; len(asks) != 1 || asks[0].message != want {
			t.Fatalf("the round's ask went to the merger's role as %+v, want it once, as %q", asks, want)
		}
	})

	t.Run("an operator's task", func(t *testing.T) {
		r := newHeldMergerRig(t)
		r.mergerWorked()
		r.closesTheGate()
		sent := len(r.prompts())
		const operator = "Say where the merge stands."
		recorder := r.request("/legion/v1/operator/claims/"+string(r.token)+"/deliver", api.DeliverRequest{Task: operator}, waitOperatorToken)
		if recorder.Code != http.StatusOK {
			t.Fatalf("the operator's deliver = %d %s, want 200", recorder.Code, recorder.Body)
		}
		r.machine.Wait()
		delivered := r.wantPrompts(sent+1, "the operator's task reaches the live merger")[sent]
		if want := heldMergerClosedNote + "\n\n" + operator; delivered.Message != want {
			t.Fatalf("the merger's session received %q, want %q", delivered.Message, want)
		}
	})
}
