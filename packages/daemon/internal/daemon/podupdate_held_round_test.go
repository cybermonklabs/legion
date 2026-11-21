package daemon

// Who is told of an update round a hold keeps past its bound (tellUpdateStalled): the architect
// that owns the held claim for a hold an architect placed, and the controller for every other
// hold, and for an architect's hold no architect can be told of. Each test drives the rig's real
// sweep to the round's bound, then runs the rows it wrote through the outbox's own executors, so
// what each test reads is the topic each notice was published on.

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// heldRoundRig is a round over the rig's issue, a tree root, whose implementer suspendedBy holds
// suspended, beside a tester that answers and the issue's own architect in state architect. The
// round is carried to its bound and swept there once.
func heldRoundRig(t *testing.T, suspendedBy supervise.SuspendSource, architect supervise.ClaimState) *podUpdateRig {
	t.Helper()
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleArchitect, state: architect, session: true, inPod: architect != supervise.StateFailed,
			contract: api.DaemonAPIVersion},
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true,
			contract: api.DaemonAPIVersion, suspendedBy: suspendedBy},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	if architect != supervise.StateFailed {
		rig.awaitRound(claim.RoleArchitect, podUpdateRound)
	}
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.clockAt(podUpdateBound)
	rig.tick()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("at the bound %s ReplacePod was called %d times under a %s suspension", podUpdateBound, len(calls), suspendedBy)
	}
	wantNoRefusedNotice(t, rig)
	return rig
}

// stallDelivery runs every notice and controller-notice row the sweep wrote through the outbox's
// own executors, each row leased as the outbox leases it: a notice is routed by the owner walk
// over the rig's machines (deliverNotice), and a controller notice is published to the project's
// controller topic (controllerNotice). A role topic in absent has no live holder, as the listener
// refuses it (notify.ErrNoHolder). It answers the update-stalled notices published, by topic.
func (r *podUpdateRig) stallDelivery(absent ...string) map[string][]record.Notice {
	r.t.Helper()
	publisher := &holderPublisher{}
	publisher.setAbsent(absent...)
	return r.stallDeliveryThrough(publisher)
}

// stallListener is the listener stallDeliveryThrough publishes through, and what it delivered.
type stallListener interface {
	notify.Publisher
	snapshot() (int, []outboxPublish)
}

// stallDeliveryThrough is stallDelivery through listener.
func (r *podUpdateRig) stallDeliveryThrough(listener stallListener) map[string][]record.Notice {
	r.t.Helper()
	runner := &outbox{log: quietLogger(), pool: r.pool, records: r.records, notices: listener, supervisor: r.sup,
		project: podUpdateProject, dispatchProject: "LEGION", now: time.Now}
	for _, kind := range []record.OutboxKind{record.OutboxKindNotice, record.OutboxKindControllerNotice} {
		for _, row := range r.rows(kind) {
			row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
			if err := runner.execute(r.ctx, row); err != nil {
				r.t.Fatalf("run the %s row %d: %v", kind, row.ID, err)
			}
		}
	}
	_, delivered := listener.snapshot()
	byTopic := map[string][]record.Notice{}
	for _, published := range delivered {
		if notice, ok := published.payload.(record.Notice); ok && notice.Kind == record.UpdateStalledNotice {
			byTopic[published.topic] = append(byTopic[published.topic], notice)
		}
	}
	return byTopic
}

// refusingListener is holderPublisher where every publish to a topic in refused is answered that
// topic's refusal, as the listener refuses a holder that does not advertise the delivery mode
// (notify.ErrRefusedByHolder) or a forward its own NATS grant denies (notify.ErrPublishDenied),
// or as a listener of the deployed, older build answers a delivery the session did not confirm
// (notify.ErrNotDelivered, receipt_timeout), on every attempt.
type refusingListener struct {
	*holderPublisher
	refused map[string]error
}

func (p refusingListener) Publish(ctx context.Context, topic, message string, payload any, key string) error {
	if refusal, ok := p.refused[topic]; ok {
		return refusal
	}
	return p.holderPublisher.Publish(ctx, topic, message, payload, key)
}

func (p refusingListener) PublishKept(ctx context.Context, topic, message string, payload any, key string) (bool, error) {
	if refusal, ok := p.refused[topic]; ok {
		return false, refusal
	}
	return p.holderPublisher.PublishKept(ctx, topic, message, payload, key)
}

// toldOn holds told to one update-stalled notice, published on topic and on no other, and answers
// it.
func toldOn(t *testing.T, told map[string][]record.Notice, topic string) record.Notice {
	t.Helper()
	if len(told) != 1 || len(told[topic]) != 1 {
		t.Fatalf("the update-stalled notices were published on %v, want one, on %s alone", slices.Sorted(maps.Keys(told)), topic)
	}
	return told[topic][0]
}

// A round held past its bound by an architect's suspension alone is told to the architect that
// owns the held claim, on that architect's role topic, and not to the controller: the hold is that
// architect's to resume, and the controller has nothing of its own to do about it. The notice
// carries the round, its pod and every claim the round asks, and names the held claim and its
// holder. It is told once: a sweep an hour later tells nobody again.
//
// The negative control is the routing undone, every hold told to the controller, which writes a
// controller notice and no architect's.
func TestPodUpdateTellsAnArchitectsHoldToTheArchitectThatOwnsTheHeldClaim(t *testing.T) {
	rig := heldRoundRig(t, supervise.SuspendedByArchitect, supervise.StateReady)

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v, want nothing: the round's one hold is an architect's, and that architect can be told", notices)
	}
	notice := toldOn(t, rig.stallDelivery(), rig.roleTopic(claim.RoleArchitect))
	if notice.Update == nil || notice.Update.Round != podUpdateRound || notice.Update.Pod != podUpdatePod {
		t.Fatalf("the architect's notice carries %+v, want round %q of pod %q", notice.Update, podUpdateRound, podUpdatePod)
	}
	for _, want := range []string{
		"An architect suspended implementer (suspended). An architect that owns the issue, or the controller, can resume it.",
		"The daemon does not replace the pod until a resume lifts each of these suspensions.",
	} {
		if !strings.Contains(notice.Reason, want) {
			t.Errorf("the architect's notice says %q, want it to say %q", notice.Reason, want)
		}
	}
	wantWaiting(t, record.ControllerNotice(notice),
		map[claim.Role]bool{claim.RoleArchitect: false, claim.RoleImplementer: false, claim.RoleTester: true})

	rig.clockAt(podUpdateBound.Add(time.Hour))
	rig.tick()
	if rows, notices := rig.rows(record.OutboxKindNotice), rig.notices(); len(rows) != 1 || len(notices) != 0 {
		t.Fatalf("an hour past the bound the round has %d architect notices and %d controller notices, want the one architect notice it was told at the bound", len(rows), len(notices))
	}
}

// A round held past its bound by the operator's or the controller's suspension is told to the
// controller alone, even where the issue's architect could be told: the operator's hold is the
// controller's to raise with the operator, and its own is the controller's to resume.
//
// The negative control is every hold told to the owning architect, whatever its holder, which
// writes an architect's notice and no controller notice.
func TestPodUpdateTellsTheControllerAloneOfItsOwnOrTheOperatorsHold(t *testing.T) {
	for _, tc := range []struct {
		by    supervise.SuspendSource
		words string
	}{
		{by: supervise.SuspendedByOperator, words: "An operator suspended implementer (suspended). Only the operator can resume it."},
		{by: supervise.SuspendedByController, words: "The controller suspended implementer (suspended). The controller can resume it."},
	} {
		t.Run(string(tc.by), func(t *testing.T) {
			rig := heldRoundRig(t, tc.by, supervise.StateReady)

			if rows := rig.rows(record.OutboxKindNotice); len(rows) != 0 {
				t.Fatalf("the sweep wrote %d architect notices, want none: the round's one hold is the %s's", len(rows), tc.by)
			}
			notice := toldOn(t, rig.stallDelivery(), notify.ControllerTopic(podUpdateProject))
			if !strings.Contains(notice.Reason, tc.words) {
				t.Errorf("the controller's notice says %q, want it to say %q", notice.Reason, tc.words)
			}
			wantWaiting(t, record.ControllerNotice(notice),
				map[claim.Role]bool{claim.RoleArchitect: false, claim.RoleImplementer: false, claim.RoleTester: true})
		})
	}
}

// A round held past its bound by an architect's suspension, where the architect the owner walk
// names has failed, is told to the controller: no session holds that architect's role to read
// the notice, so it would finish undelivered (noticeAnswered's architectEnded), and the
// controller can resume an architect's suspension too. The notice names the architect's hold.
//
// The negative control is an architect's hold told to the walk's architect whatever its claim's
// state, which writes an architect's notice and no controller notice.
func TestPodUpdateTellsTheControllerOfAnArchitectsHoldWhenTheOwningArchitectFailed(t *testing.T) {
	rig := heldRoundRig(t, supervise.SuspendedByArchitect, supervise.StateFailed)

	if rows := rig.rows(record.OutboxKindNotice); len(rows) != 0 {
		t.Fatalf("the sweep wrote %d architect notices, want none: the architect that owns the held claim has failed", len(rows))
	}
	notice := toldOn(t, rig.stallDelivery(), notify.ControllerTopic(podUpdateProject))
	if want := "An architect suspended implementer (suspended). An architect that owns the issue, or the controller, can resume it."; !strings.Contains(notice.Reason, want) {
		t.Errorf("the controller's notice says %q, want it to say %q", notice.Reason, want)
	}
	wantWaiting(t, record.ControllerNotice(notice), map[claim.Role]bool{claim.RoleImplementer: false, claim.RoleTester: true})
}

// A round held past its bound by an architect's suspension is told to the architect that owns the
// held claim, and where that architect's claim fails after the sweep queued its notice and before
// the outbox runs it, the row finishes undelivered (noticeAnswered's architectEnded) and the
// controller is told the round in its place, with the architect's hold named: the sweep marked the
// round told when it queued the row, so no later sweep would tell anyone. The relayed notice does
// not list the failed architect as a claim the round waits on: a failed claim runs no turn on the
// pod, so the round does not wait for it.
//
// The negative controls are the relay undone (relayUpdateStalled), which publishes no
// update-stalled notice at all, and the relayed waiting list left as the sweep wrote it, which
// lists the failed architect in the state it was in before it failed.
func TestPodUpdateTellsTheControllerOfAnArchitectsHoldWhoseArchitectFailedBeforeItsNotice(t *testing.T) {
	rig := heldRoundRig(t, supervise.SuspendedByArchitect, supervise.StateReady)
	if rows, notices := rig.rows(record.OutboxKindNotice), rig.notices(); len(rows) != 1 || len(notices) != 0 {
		t.Fatalf("the sweep wrote %d architect notices and %d controller notices, want the one architect notice", len(rows), len(notices))
	}

	// The architect's turn ends in a provider error that cannot clear, which fails its claim.
	const task = "outbox:7"
	architect := rig.tokens[claim.RoleArchitect]
	rig.handle(claim.RoleArchitect, supervise.RequestDeliver{Claim: architect, Task: "Carry the tree on.", ID: task})
	rig.awaitClaim(claim.RoleArchitect, "its task to be acknowledged", func(c supervise.Claim) bool {
		return c.Pending != nil && !c.Pending.DeliveredAt.IsZero()
	})
	rig.handle(claim.RoleArchitect, supervise.StreamTurnStart{Claim: architect})
	rig.handle(claim.RoleArchitect, supervise.StreamTurnEnd{Claim: architect})
	rig.handle(claim.RoleArchitect, supervise.StreamPromptResult{Claim: architect, DeliveryID: task, AgentInvoked: true,
		Error: "invalid_request_error", HTTPStatus: 400})
	rig.awaitClaim(claim.RoleArchitect, "its claim to fail", func(c supervise.Claim) bool { return c.State == supervise.StateFailed })

	notice := toldOn(t, rig.stallDelivery(rig.roleTopic(claim.RoleArchitect)), notify.ControllerTopic(podUpdateProject))
	if notice.Update == nil || notice.Update.Round != podUpdateRound || notice.Update.Pod != podUpdatePod {
		t.Fatalf("the controller's notice carries %+v, want round %q of pod %q", notice.Update, podUpdateRound, podUpdatePod)
	}
	if want := "An architect suspended implementer (suspended). An architect that owns the issue, or the controller, can resume it."; !strings.Contains(notice.Reason, want) {
		t.Errorf("the controller's notice says %q, want it to say %q", notice.Reason, want)
	}
	wantWaiting(t, record.ControllerNotice(notice), map[claim.Role]bool{claim.RoleImplementer: false, claim.RoleTester: true})
}

// A round held past its bound by an architect's suspension is told to the architect that owns the
// held claim, and where the listener refuses that notice on every attempt — the architect's
// session does not advertise the delivery mode, or the listener's own NATS grant denies the
// forward — the row finishes undelivered (noticeAnswered) and the controller is told the round in
// its place, once, with the architect's hold named: the sweep marked the round told when it
// queued the row, so no later sweep would tell anyone. The architect's claim still answers, so
// the relayed notice lists it as the sweep did. The architect's row run again tells the
// controller nothing more.
//
// The negative control is the relay undone in those two answers (relayUpdateStalled), which
// publishes no update-stalled notice at all.
func TestPodUpdateTellsTheControllerOfAnArchitectsHoldWhoseNoticeTheListenerRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal error
	}{
		{name: "a holder that does not advertise the delivery mode", refusal: notify.ErrRefusedByHolder{Holder: "ses_architect"}},
		{name: "a forward the listener's grant denies", refusal: notify.ErrPublishDenied{Message: "permissions violation for publish"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := heldRoundRig(t, supervise.SuspendedByArchitect, supervise.StateReady)
			if rows, notices := rig.rows(record.OutboxKindNotice), rig.notices(); len(rows) != 1 || len(notices) != 0 {
				t.Fatalf("the sweep wrote %d architect notices and %d controller notices, want the one architect notice", len(rows), len(notices))
			}
			listener := refusingListener{holderPublisher: &holderPublisher{},
				refused: map[string]error{rig.roleTopic(claim.RoleArchitect): tc.refusal}}

			notice := toldOn(t, rig.stallDeliveryThrough(listener), notify.ControllerTopic(podUpdateProject))
			if notice.Update == nil || notice.Update.Round != podUpdateRound || notice.Update.Pod != podUpdatePod {
				t.Fatalf("the controller's notice carries %+v, want round %q of pod %q", notice.Update, podUpdateRound, podUpdatePod)
			}
			if want := "An architect suspended implementer (suspended). An architect that owns the issue, or the controller, can resume it."; !strings.Contains(notice.Reason, want) {
				t.Errorf("the controller's notice says %q, want it to say %q", notice.Reason, want)
			}
			wantWaiting(t, record.ControllerNotice(notice),
				map[claim.Role]bool{claim.RoleArchitect: false, claim.RoleImplementer: false, claim.RoleTester: true})

			rig.stallDeliveryThrough(listener)
			if notices := rig.notices(); len(notices) != 1 {
				t.Fatalf("the outbox holds %d controller notices after the architect's row ran again, want the one it was told", len(notices))
			}
		})
	}
}

// A round held past its bound by an architect's suspension is told to the architect that owns the
// held claim, and where that architect's session confirms none of its copies — a listener of the
// deployed, older build answers each publish 504 receipt_timeout — the notice is queued again
// until it has been queued again the most times a notice is, the last copy's answer finishes it
// undelivered (reholdNotice), and the controller is told the round in its place, once, with the
// architect's hold named: the sweep marked the round told when it queued the row, so no later
// sweep would tell anyone. The architect's claim still answers, so the relayed notice lists it as
// the sweep did. The last copy run again tells the controller nothing more.
//
// The negative control is the relay undone in that last answer (reholdNotice's
// relayUpdateStalled), which publishes no update-stalled notice at all.
func TestPodUpdateTellsTheControllerOfAnArchitectsHoldWhoseNoticeItsArchitectNeverConfirms(t *testing.T) {
	rig := heldRoundRig(t, supervise.SuspendedByArchitect, supervise.StateReady)
	if rows, notices := rig.rows(record.OutboxKindNotice), rig.notices(); len(rows) != 1 || len(notices) != 0 {
		t.Fatalf("the sweep wrote %d architect notices and %d controller notices, want the one architect notice", len(rows), len(notices))
	}
	listener := refusingListener{holderPublisher: &holderPublisher{},
		refused: map[string]error{rig.roleTopic(claim.RoleArchitect): confirmedAnswer(notify.ReasonReceiptTimeout, "ses_stopped")}}

	// The notice and each copy it is queued again as run in turn, each finished once it has run,
	// as the outbox finishes a row whose run returned (settle): every answer before the last
	// queues the next copy and tells nobody. The last copy is left in the outbox to run again.
	var told map[string][]record.Notice
	for resends := 0; resends <= len(noticeReholdDelays); resends++ {
		rows := rig.rows(record.OutboxKindNotice)
		if len(rows) != 1 {
			t.Fatalf("before copy %d the outbox holds %d architect notices, want one", resends, len(rows))
		}
		var queued record.Notice
		if err := json.Unmarshal(rows[0].Payload, &queued); err != nil {
			t.Fatalf("decode the architect notice %d: %v", rows[0].ID, err)
		}
		if queued.Kind != record.UpdateStalledNotice || queued.Resends != resends {
			t.Fatalf("the architect notice is a %s queued again %d times, want an update-stalled one queued again %d times", queued.Kind, queued.Resends, resends)
		}
		told = rig.stallDeliveryThrough(listener)
		if resends == len(noticeReholdDelays) {
			break
		}
		if len(told) != 0 {
			t.Fatalf("copy %d told %v, want nobody told before the last copy", resends, slices.Sorted(maps.Keys(told)))
		}
		if _, err := rig.pool.Exec(rig.ctx, "delete from outbox where id = $1", rows[0].ID); err != nil {
			t.Fatalf("finish the architect notice %d: %v", rows[0].ID, err)
		}
	}
	if rows := rig.rows(record.OutboxKindNotice); len(rows) != 1 {
		t.Fatalf("the outbox holds %d architect notices after the last copy, want the last copy alone and no copy of it", len(rows))
	}

	notice := toldOn(t, told, notify.ControllerTopic(podUpdateProject))
	if notice.Update == nil || notice.Update.Round != podUpdateRound || notice.Update.Pod != podUpdatePod {
		t.Fatalf("the controller's notice carries %+v, want round %q of pod %q", notice.Update, podUpdateRound, podUpdatePod)
	}
	if want := "An architect suspended implementer (suspended). An architect that owns the issue, or the controller, can resume it."; !strings.Contains(notice.Reason, want) {
		t.Errorf("the controller's notice says %q, want it to say %q", notice.Reason, want)
	}
	wantWaiting(t, record.ControllerNotice(notice),
		map[claim.Role]bool{claim.RoleArchitect: false, claim.RoleImplementer: false, claim.RoleTester: true})

	rig.stallDeliveryThrough(listener)
	if notices := rig.notices(); len(notices) != 1 {
		t.Fatalf("the outbox holds %d controller notices after the last copy ran again, want the one it was told", len(notices))
	}
}
