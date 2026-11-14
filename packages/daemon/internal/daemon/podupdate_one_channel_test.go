package daemon

// The update round's ask reaches each role on one channel: the sweep publishes it once per claim
// per round, as a role-message outbox row (record.UpdateAsk) to that claim's own Envoy topic, and
// no claim is queued or sent a task for the round (dispatch://INFRA-2981 R1, verification's
// c-one-channel). These cases drive the pod-update rig of podupdate_test.go.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// podUpdateRescue is the issue's one rescue branch, as the ask names it: the issue's branch and
// "-rescue" (claim.RescueBranch). It is written out here, so an ask that names any other branch fails
// the comparison of its text.
const podUpdateRescue = "legion/LEGION-484-rescue"

// asks is every update-ask row the sweep enqueued, decoded, oldest first, each on the rig's issue.
func (r *podUpdateRig) asks() []record.UpdateAsk {
	r.t.Helper()
	var asks []record.UpdateAsk
	for _, row := range r.rows(record.OutboxKindUpdateAsk) {
		if row.Issue != podUpdateIssue {
			r.t.Fatalf("update ask %d is on %s, want %s", row.ID, row.Issue, podUpdateIssue)
		}
		var ask record.UpdateAsk
		if err := json.Unmarshal(row.Payload, &ask); err != nil {
			r.t.Fatalf("decode update ask %d: %v", row.ID, err)
		}
		asks = append(asks, ask)
	}
	return asks
}

// prompts is how many prompts every role's connection has received, by role.
func (r *podUpdateRig) prompts() map[claim.Role]int {
	r.t.Helper()
	sent := map[claim.Role]int{}
	for role, conn := range r.workers {
		sent[role] = len(conn.Prompts())
	}
	return sent
}

// One pod round, over the five kinds of claim a pod holds: a role in a turn, an idle role, the
// issue's architect, a failed claim and a claim that has left the round's pod. Over ten sweeps
// each asked claim gets exactly one update-ask row, the architect's included, each naming its own
// role, the round, the pod and the round's one text: the reasons the round opened with and the
// moment it ends, its opening plus pod_update_grace_minutes. The failed claim and the claim that
// left the pod are asked nothing, although each records the round with no publish mark. No claim
// is sent a prompt for the round: the machine queues no task.
//
// The negative controls are the ones c-one-channel names. A task queued beside the publish is a
// prompt on a role's connection. A publish at every sweep is ten rows a role, not one.
func TestPodUpdateAsksEachRoleOnceOnItsOwnTopicOverTenSweeps(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateWorking, session: true, inPod: true, contract: api.DaemonAPIVersion},
		podUpdateRole{role: claim.RoleTester, state: supervise.StateIdle, session: true, inPod: true, contract: api.DaemonAPIVersion},
		readyWorker(claim.RoleArchitect),
		podUpdateRole{role: claim.RolePlanner, state: supervise.StateFailed, session: true, contract: api.DaemonAPIVersion,
			round: podUpdateRound, reasons: podUpdateDrift, unasked: true},
		podUpdateRole{role: claim.RoleReviewer, state: supervise.StateSuspended, session: true, contract: api.DaemonAPIVersion,
			suspendedBy: supervise.SuspendedByDaemon, round: podUpdateRound, reasons: podUpdateDrift, left: podUpdateRound, unasked: true},
	)
	rig.drifts(podUpdateDrift)
	asked := []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester}
	before := rig.prompts()

	for sweep := range 10 {
		if sweep > 0 {
			rig.clock.advance(time.Minute)
		}
		rig.tick()
		if sweep == 0 {
			for _, role := range asked {
				rig.awaitRound(role, podUpdateRound)
			}
		}
	}

	text := supervise.UpdateTask(podUpdateRound, podUpdateDrift, podUpdateStart.Add(podUpdateGrace), podUpdateRescue)
	rows := map[claim.Role]int{}
	for _, ask := range rig.asks() {
		rows[ask.Role]++
		if ask.Round != podUpdateRound || ask.Pod != podUpdatePod || ask.Text != text {
			t.Errorf("the ask to the %s = %+v, want round %s of pod %s with the round's text:\n%s", ask.Role, ask, podUpdateRound, podUpdatePod, text)
		}
	}
	for _, role := range asked {
		if rows[role] != 1 {
			t.Errorf("the %s was asked %d times over ten sweeps, want once", role, rows[role])
		}
		if c := rig.claimOf(role); c.UpdateRound != podUpdateRound || c.UpdateTaskedAt == nil {
			t.Errorf("the %s = round %q asked at %v, want the round recorded and the ask marked", role, c.UpdateRound, c.UpdateTaskedAt)
		}
	}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleReviewer} {
		if rows[role] != 0 {
			t.Errorf("the %s was asked %d times, want never: a failed claim and a claim that left the pod are asked nothing", role, rows[role])
		}
		if c := rig.claimOf(role); c.UpdateTaskedAt != nil {
			t.Errorf("the %s records an ask at %v, want none", role, c.UpdateTaskedAt)
		}
	}
	if len(rows) != len(asked) {
		t.Errorf("asks went to %v, want the %v alone", rows, asked)
	}

	for role := range rig.workers {
		rig.settle(role)
	}
	after := rig.prompts()
	for role, n := range after {
		if n != before[role] {
			t.Errorf("the %s was sent %d prompts while the round ran, want none: the round's ask is no task", role, n-before[role])
		}
	}
	for _, role := range asked {
		if p := rig.claimOf(role).Pending; p != nil {
			t.Errorf("the %s holds %+v, want no task queued for the round", role, p)
		}
	}
}

// The round's terms are fixed when it opens (dispatch://INFRA-2981 R9, verification's
// c-early-release terms case). A later sweep that reads other drift for the same pod writes no
// new reasons over the open round, and the ask the sweep publishes names the reasons the round
// opened with: the ask a role reads and the record an operator reads name one thing for one
// round id. The late-round notice names them too: an operator's suspension holds the round past
// its bound while the pod still reads the later drift, and the notice names the reasons recorded
// at the round's open, not that drift.
func TestPodUpdateALaterSweepThatReadsOtherDriftKeepsTheRoundsTerms(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig.drifts(podUpdateDrift)

	rig.tick()
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
	}

	// The pod drifts again under the open round: a second key moves.
	rig.drifts(podUpdateDrift, "disk")
	for range 3 {
		rig.clock.advance(time.Minute)
		rig.tick()
	}
	for _, role := range roles {
		rig.settle(role)
		if c := rig.claimOf(role); c.UpdateRound != podUpdateRound || c.UpdateReasons != podUpdateDrift {
			t.Errorf("the %s records round %q for %q, want %q: a later sweep writes no new reasons over an open round",
				role, c.UpdateRound, c.UpdateReasons, podUpdateDrift)
		}
	}
	text := supervise.UpdateTask(podUpdateRound, podUpdateDrift, podUpdateStart.Add(podUpdateGrace), podUpdateRescue)
	asks := rig.asks()
	if len(asks) != len(roles) {
		t.Fatalf("the sweeps enqueued %d asks, want one for each of the %d roles: %+v", len(asks), len(roles), asks)
	}
	for _, ask := range asks {
		if ask.Text != text {
			t.Errorf("the ask to the %s says:\n%s\nwant the round's own terms:\n%s", ask.Role, ask.Text, text)
		}
	}

	rig.suspendByOperator(claim.RoleTester)
	rig.clockAt(podUpdateBound)
	rig.tick()
	notices := rig.notices()
	if len(notices) != 1 || notices[0].Update == nil {
		t.Fatalf("at the bound %s the controller was told %+v, want one update-stalled notice", rig.clock.Now(), notices)
	}
	if got := notices[0].Update.Reasons; got != podUpdateDrift {
		t.Errorf("the late-round notice names reasons %q, want the %q the round opened with", got, podUpdateDrift)
	}
}

// A daemon with no outbox (no workflow, so supervision's enqueue is nil) publishes no ask and
// marks no claim, and its round still runs: the claims record it, an answer is taken, and the pod
// is replaced once every role has answered.
func TestPodUpdateWithNoOutboxPublishesNothingAndStillRunsTheRound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.s.enqueue = nil
	rig.drifts(podUpdateDrift)
	before := rig.prompts()

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	for range 3 {
		rig.clock.advance(time.Minute)
		rig.tick()
	}

	if rows := rig.rows(record.OutboxKindUpdateAsk); len(rows) != 0 {
		t.Fatalf("a daemon with no outbox wrote %d update asks, want none", len(rows))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound || c.UpdateTaskedAt != nil {
		t.Fatalf("the implementer = round %q asked at %v, want the round recorded and no ask marked", c.UpdateRound, c.UpdateTaskedAt)
	}
	rig.settle(claim.RoleImplementer)
	if after := rig.prompts(); after[claim.RoleImplementer] != before[claim.RoleImplementer] {
		t.Fatalf("the implementer was sent %d prompts, want none", after[claim.RoleImplementer]-before[claim.RoleImplementer])
	}

	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.tick()

	rig.awaitReplacement(1, "once the one role of a round with no outbox answered")
}
