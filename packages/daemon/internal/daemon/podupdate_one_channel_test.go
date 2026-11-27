package daemon

// The update round's ask reaches each role on one channel: the sweep publishes it once per claim
// per round, as a role-message outbox row (record.UpdateAsk) to that claim's own Envoy topic, and
// no claim is queued or sent a task for the round (dispatch://INFRA-2981 R1, verification's
// c-one-channel). "Once" counts the asks that reached a session: the row marks its claim asked
// when its publish reached a session, and an ask whose topic had no live holder is asked again
// (podupdate_undelivered_ask_test.go). These cases drive the pod-update rig of podupdate_test.go.

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// asks is every update-ask row the sweep enqueued, decoded, oldest first, each on the rig's issue:
// the rows the outbox holds now and every row the rig's lane has finished and deleted since
// (runLane), which this read kept the first time it saw them.
func (r *podUpdateRig) asks() []record.UpdateAsk {
	r.t.Helper()
	for _, row := range r.rows(record.OutboxKindUpdateAsk) {
		if row.Issue != podUpdateIssue {
			r.t.Fatalf("update ask %d is on %s, want %s", row.ID, row.Issue, podUpdateIssue)
		}
		var ask record.UpdateAsk
		if err := json.Unmarshal(row.Payload, &ask); err != nil {
			r.t.Fatalf("decode update ask %d: %v", row.ID, err)
		}
		r.asked[row.ID] = ask
	}
	ids := make([]int64, 0, len(r.asked))
	for id := range r.asked {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	asks := make([]record.UpdateAsk, len(ids))
	for i, id := range ids {
		asks[i] = r.asked[id]
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
// issue's architect, a failed claim and a claim that has left the round's pod. Over ten sweeps,
// the role-message lane running after each, each asked claim gets exactly one update-ask row, the
// architect's included, each naming its own role, the round, the pod and the round's one text:
// the reasons the round opened with and the moment it ends, its opening plus
// pod_update_grace_minutes. Each row reaches its role's topic once and marks its claim asked. The
// failed claim and the claim that left the pod are asked nothing, although each records the round
// with no publish mark. No claim is sent a prompt for the round: the machine queues no task.
//
// The negative controls are the ones c-one-channel names. A task queued beside the publish is a
// prompt on a role's connection. A publish at every sweep is ten rows a role, not one.
func TestPodUpdateAsksEachRoleOnceOnItsOwnTopicOverTenSweeps(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateWorking, session: true, inPod: true, contract: api.DaemonAPIVersion()},
		podUpdateRole{role: claim.RoleTester, state: supervise.StateIdle, session: true, inPod: true, contract: api.DaemonAPIVersion()},
		readyWorker(claim.RoleArchitect),
		podUpdateRole{role: claim.RolePlanner, state: supervise.StateFailed, session: true, contract: api.DaemonAPIVersion(),
			round: podUpdateRound, reasons: podUpdateDrift, unasked: true},
		podUpdateRole{role: claim.RoleReviewer, state: supervise.StateSuspended, session: true, contract: api.DaemonAPIVersion(),
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
		rig.runLane()
		if sweep == 0 {
			for _, role := range asked {
				rig.awaitRound(role, podUpdateRound)
			}
		}
	}

	rows := map[claim.Role]int{}
	for _, ask := range rig.asks() {
		rows[ask.Role]++
		text := supervise.UpdateTask(ask.Role, podUpdateRound, podUpdateDrift, podUpdateStart.Add(podUpdateGrace))
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
	delivered := map[string]int{}
	for _, publish := range rig.listener.delivered {
		delivered[publish.topic]++
	}
	for _, role := range asked {
		if n := delivered[rig.roleTopic(role)]; n != 1 {
			t.Errorf("the %s's topic was delivered %d asks, want one", role, n)
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

// The reviewer creates no commit and pushes nothing (its role part), so the ask a pod round sends
// it tells it to push nothing, and asks it to commit nothing a subagent made. A role that commits
// is still told to push its commits and what its subagents made: the implementer, and the merger,
// which makes and pushes the merge of the base forward in merging (roles/merger.md), whose
// unpushed merge commit the pod's replacement would lose.
func TestPodUpdateTellsOnlyARoleThatCommitsToCommitAndPush(t *testing.T) {
	t.Parallel()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleReviewer, claim.RoleMerger}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleReviewer), readyWorker(claim.RoleMerger))
	rig.drifts(podUpdateDrift)
	rig.tick()
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
	}
	rig.clock.advance(time.Minute)
	rig.tick()
	told := map[claim.Role]string{}
	for _, ask := range rig.asks() {
		told[ask.Role] = ask.Text
	}
	for _, role := range roles {
		text, commits := told[role], role != claim.RoleReviewer
		if text == "" {
			t.Fatalf("the %s was asked nothing, want one ask", role)
		}
		for _, says := range []string{"1. Push every commit that you want to keep to this issue's branch.", "Then commit and push what it made."} {
			if strings.Contains(text, says) != commits {
				t.Errorf("the ask to the %s holds %q: %t, want %t", role, says, !commits, commits)
			}
		}
		if says := "1. You create no commit, so you push nothing."; strings.Contains(text, says) == commits {
			t.Errorf("the ask to the %s holds %q: %t, want %t", role, says, commits, !commits)
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
	t.Parallel()
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
	asks := rig.asks()
	if len(asks) != len(roles) {
		t.Fatalf("the sweeps enqueued %d asks, want one for each of the %d roles: %+v", len(asks), len(roles), asks)
	}
	for _, ask := range asks {
		if text := supervise.UpdateTask(ask.Role, podUpdateRound, podUpdateDrift, podUpdateStart.Add(podUpdateGrace)); ask.Text != text {
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
// is replaced once every role the round asks has answered and no turn or claim's activity report
// holds the round (roundEnd.holding).
func TestPodUpdateWithNoOutboxPublishesNothingAndStillRunsTheRound(t *testing.T) {
	t.Parallel()
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

// An operator's spawn of another project's issue runs in a pod of this daemon's, which the
// update-round sweep reads as it reads every pod, and the round over that pod asks its role as
// every round does. But the ask is an outbox row keyed by that issue, which only that project's
// daemon runs (claim.KeyOfProject), so the sweep's enqueue writes none (supervision.enqueue): the
// round asks nothing, logs no Warn and marks the claim asked at no sweep, while the round itself
// is recorded and runs on. The daemon's records are wrapped as the daemon wraps them
// (projectRecords), whose refusal of such a key is a writer's defect. Before the fix the ask
// reached that refusal, which logged its Warn at every sweep's ask, and the sweep then logged the
// ask enqueued; the no-Warn check below is what fails without the fix, since #4337 left the ask's
// mark to the outbox's delivery (supervise's Machine.PublishUpdateAsk).
func TestAnUpdateRoundOverASpawnOfAnotherProjectsIssueAsksItNothing(t *testing.T) {
	t.Parallel()
	const (
		other    = "OTHERPROJECT-9"
		otherPod = "5b1e7d20-3c64-4a8f-b2d9-8e0f6a1c4d37"
	)
	rig := newPodUpdateRig(t)
	rig.s.enqueue = (&workflowRuntime{pool: rig.pool, records: projectRecords{Store: rig.records, project: "LEGION", log: rig.log},
		dispatchProject: "LEGION", log: rig.log}).enqueueOutbox
	token := mustClaimToken(t, other, claim.RoleArchitect)
	if err := rig.st.PutClaim(rig.ctx, supervise.Claim{Token: token, Project: podUpdateProject, Tree: other, Issue: other,
		Role: claim.RoleArchitect, Generation: 1, State: supervise.StateReady, PluginContract: api.DaemonAPIVersion(),
		Session: "ses_other", SessionFile: "/sessions/other.jsonl", Locator: podUpdateLocator(token, other, otherPod)}); err != nil {
		t.Fatalf("seed the operator's spawn of %s: %v", other, err)
	}
	stored, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	if _, err := rig.sup.restore(rig.ctx, slices.DeleteFunc(stored, func(c supervise.Claim) bool { return c.Token != token })); err != nil {
		t.Fatalf("restore the spawn of %s: %v", other, err)
	}
	rig.conns.Register(token, fake.NewConn())
	rig.rt.ScriptPodDrift(token, podUpdateDrift)
	round := supervise.PodRound(otherPod)
	since := len(rig.logs.String())

	rig.tick()
	testwait.Eventually(t, "the round over the spawn's pod to be recorded on its claim", func() bool {
		return rig.machineClaim(token).UpdateRound == round
	})
	for range 3 {
		rig.clock.advance(time.Minute)
		rig.tick()
	}

	var asks []int64
	for _, row := range rig.rows(record.OutboxKindUpdateAsk) {
		asks = append(asks, row.ID)
	}
	if len(asks) != 0 {
		t.Errorf("the sweeps queued update asks %v, want none: no ask of %s is this daemon's to write", asks, other)
	}
	if c := rig.machineClaim(token); c.UpdateRound != round || c.UpdateTaskedAt != nil {
		t.Errorf("the spawn = round %q asked at %v, want the round recorded and no ask marked: nothing asked it", c.UpdateRound, c.UpdateTaskedAt)
	}
	for line := range strings.Lines(rig.logs.String()[since:]) {
		if !strings.Contains(line, other) {
			continue
		}
		if strings.Contains(line, `"level":"WARN"`) {
			t.Errorf("the sweeps logged %s, want no Warn: a spawn of another project's issue in a round is no defect", line)
		}
	}
}
