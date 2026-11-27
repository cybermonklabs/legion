package daemon

// Who is told of an update round a hold keeps past its bound (tellUpdateStalled): the
// controller, whichever holder holds the round. The test drives the rig's real sweep to the
// round's bound, then runs the rows it wrote through the outbox's own executors, so what it reads
// is the topic each notice was published on.

import (
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

// suspensionHeldRoundRig is a round over the rig's issue, a tree root, whose implementer suspendedBy holds
// suspended, beside a tester that answers and the issue's own architect, ready. The round is
// carried to its bound and swept there once.
func suspensionHeldRoundRig(t *testing.T, suspendedBy supervise.SuspendSource) *podUpdateRig {
	t.Helper()
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleArchitect, state: supervise.StateReady, session: true, inPod: true,
			contract: api.DaemonAPIVersion()},
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true,
			contract: api.DaemonAPIVersion(), suspendedBy: suspendedBy},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.awaitRound(claim.RoleArchitect, podUpdateRound)
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
// controller topic (controllerNotice). It answers the update-stalled notices published, by topic.
func (r *podUpdateRig) stallDelivery() map[string][]record.Notice {
	r.t.Helper()
	listener := &holderPublisher{}
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

// toldOn holds told to one update-stalled notice, published on topic and on no other, and answers
// it.
func toldOn(t *testing.T, told map[string][]record.Notice, topic string) record.Notice {
	t.Helper()
	if len(told) != 1 || len(told[topic]) != 1 {
		t.Fatalf("the update-stalled notices were published on %v, want one, on %s alone", slices.Sorted(maps.Keys(told)), topic)
	}
	return told[topic][0]
}

// A round held past its bound by the operator's or the controller's suspension is told to the
// controller alone, though the issue's architect is ready: the operator's hold is the controller's
// to raise with the operator, and its own is the controller's to resume. The notice names the held
// agent and its holder, and lists every claim the round asks.
//
// The negative control is the hold told to the owning architect, which writes an architect's
// notice and no controller notice.
func TestPodUpdateTellsTheControllerAloneOfItsOwnOrTheOperatorsHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		by    supervise.SuspendSource
		words string
	}{
		{by: supervise.SuspendedByOperator, words: "An operator suspended implementer (suspended). Only the operator can resume it."},
		{by: supervise.SuspendedByController, words: "The controller suspended implementer (suspended). The controller can resume it."},
	} {
		t.Run(string(tc.by), func(t *testing.T) {
			rig := suspensionHeldRoundRig(t, tc.by)

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
