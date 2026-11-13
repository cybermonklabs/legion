package daemon

// A round over a pod its claims no longer record, as dispatch://INFRA-1583's implementer claim held
// one from 2026-11-12 (dispatch://INFRA-3276, R5). Round `pod:0a389176…` was ended by its own
// replacement, which posted the end to every member's machine. Four machines took it; the
// implementer's was in a relaunch, the daemon stopped before it took the post, and the post went
// with the daemon, since it lives in memory alone. The boot restored the round from the store,
// every claim came to record the pod that replaced the round's own, and that pod still drifted, so
// the sweep read the round as one to carry on. No answer could end it, it held the implementer's
// next task, and it held the one open-round slot `pod_update_max_open_rounds` gave the project,
// for 13 hours, until a person cleared it by hand.
//
// This file uses nothing its change adds, so it can be laid over the commit before that change
// and built there, where its test fails: the round stays open after the sweep and the issue
// waiting for the slot stays held.

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The round's pod went, the claims record the pod that replaced it, and that pod has drifted: one
// sweep ends the round on the one member the earlier end never reached, in its machine and in the
// store, and the task the round was holding back from that member goes to it. The round held no
// slot at that sweep, and the next sweep finds the issue that was waiting for the slot in a round
// of its own, with the issue whose round ended waiting behind it for its own next round, over the
// pod in its place.
func TestPodUpdateEndsARoundOverAPodItsClaimsNoLongerRecord(t *testing.T) {
	const (
		waiting    = "LEGION-485"
		waitingPod = "2e7c9a41-6b0d-4f58-a3c2-91d4e8b07f63"
		held       = "outbox:7"
	)
	rig := newPodUpdateRig(t,
		// The member the end never reached: it answered the round before the round's own
		// replacement, and its relaunch came up in the pod that replaced the round's own, which
		// it records, leaving the round as that pod left it (supervise's leftPodRoundAt).
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, contract: api.DaemonAPIVersion,
			pod: podUpdateReplacementPod, round: podUpdateRound, reasons: podUpdateDrift, signalled: true, left: podUpdateRound},
		// The members the end did reach: in no round, and in that same pod.
		podUpdateRole{role: claim.RoleTester, state: supervise.StateReady, session: true, contract: api.DaemonAPIVersion,
			pod: podUpdateReplacementPod},
		podUpdateRole{role: claim.RoleReviewer, state: supervise.StateReady, session: true, contract: api.DaemonAPIVersion,
			pod: podUpdateReplacementPod},
	)
	// The pod in the place of the round's own was built before the change the round was opened
	// for, so it has drifted as that pod had.
	rig.drifts(podUpdateDrift)
	rig.s.cfg.PodUpdateMaxOpenRounds = new(1)
	waitingToken := rig.driftingIssue(waiting, waitingPod)

	// The implementer's next phase task, which the round it answered holds back from it.
	implementer := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: implementer, ID: held,
		Task: "Carry the implementation on.", Phase: phase.Implementing, Generation: 1})
	rig.settle(claim.RoleImplementer)
	conn := rig.workers[claim.RoleImplementer]
	if sent := promptsOf(conn, held); len(sent) != 0 {
		t.Fatalf("the implementer was sent %+v while it stood in the round it answered, want the task held", sent)
	}

	rig.tick()

	// The one sweep ends the round: the end reaches the machine through its own queue, and the
	// machine writes the claim before it decides anything else.
	rig.awaitClaim(claim.RoleImplementer, "its round over the pod that went to end at the sweep that read the claims recording another pod",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })
	stored, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims the store holds: %v", err)
	}
	for _, c := range stored {
		if c.Issue == podUpdateIssue && c.UpdateRound != "" {
			t.Errorf("the store holds the %s in round %q after the sweep, want no claim of %s in a round",
				c.Role, c.UpdateRound, podUpdateIssue)
		}
	}
	rig.settle(claim.RoleImplementer)
	if sent := promptsOf(conn, held); len(sent) == 0 {
		t.Fatalf("the implementer was sent %+v once its round ended, want the task the round held back", conn.Prompts())
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Errorf("the issue was written %q; the one member of the round had answered before its pod went", messages)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Errorf("ReplacePod called %d times for a pod that is already gone", len(calls))
	}

	rig.tick()

	testwait.Eventually(t, waiting+" to hold the round over its own pod", func() bool {
		c := rig.machineClaim(waitingToken)
		return c.UpdateRound == supervise.PodRound(waitingPod) && c.UpdateTaskedAt != nil
	})
	rounds := rig.s.podUpdateRoundsView()
	if !slices.Equal(rounds.InRound, []string{waiting}) || !slices.Equal(rounds.WaitingOnMaxOpenRounds, []string{podUpdateIssue}) {
		t.Errorf("the next sweep recorded in a round %v and waiting on the open-round limit %v, want %s in its round and %s waiting for its own over the pod in its place",
			rounds.InRound, rounds.WaitingOnMaxOpenRounds, waiting, podUpdateIssue)
	}
}

// driftingIssue seeds an issue beside the rig's own: a tree of its own, with one ready
// implementer in pod, a pod the runtime answers drifted, supervised by the rig's daemon as a boot
// restores a claim. It is the issue the project's open-round limit holds back while another holds
// the one slot, and it answers its implementer's claim. The rig's own drifts scripts the rig's
// issue alone, so this issue stays drifted whatever a test scripts of that one.
func (r *podUpdateRig) driftingIssue(key, pod string) claim.Token {
	r.t.Helper()
	token := mustClaimToken(r.t, key, claim.RoleImplementer)
	seedWorkflowTree(r.t, r.pool.Config().ConnString(), podUpdateProject, record.Issue{
		Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: key, Title: "The issue a limit holds back", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
	}, supervise.Claim{
		Token: token, Project: podUpdateProject, Tree: key, Issue: key, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateReady, PluginContract: api.DaemonAPIVersion,
		Session: "ses_" + key, SessionFile: "/sessions/" + key + ".jsonl",
		Locator: podUpdateLocator(token, key, pod),
	})
	stored, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims of %s: %v", key, err)
	}
	seeded := slices.DeleteFunc(stored, func(c supervise.Claim) bool { return c.Token != token })
	if len(seeded) != 1 {
		r.t.Fatalf("the store holds %d claims of %s's implementer, want the one seeded", len(seeded), key)
	}
	if _, err := r.sup.restore(r.ctx, seeded); err != nil {
		r.t.Fatalf("restore the claim of %s: %v", key, err)
	}
	r.conns.Register(token, fake.NewConn())
	r.rt.ScriptPodDrift(token, podUpdateDrift)
	return token
}

// machineClaim is the claim of token as its machine holds it, for a claim of an issue the rig's
// roles do not name (driftingIssue).
func (r *podUpdateRig) machineClaim(token claim.Token) supervise.Claim {
	r.t.Helper()
	m, ok := r.sup.Machine(token)
	if !ok {
		r.t.Fatalf("%s is not supervised", token)
	}
	return m.Claim()
}
