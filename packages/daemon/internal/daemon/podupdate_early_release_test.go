package daemon

// The early release (dispatch://INFRA-2981 R5, R9, verification's c-early-release): an answer a
// role gives stands for its round through any turn after it, an answered claim is handed no task
// until the round ends, and an answer brings the round's end forward while silence delays nothing
// past the bound. These cases drive the pod-update rig of podupdate_test.go, with the bound of
// podupdate_bound_test.go.

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// Every claim answers, and then one claim runs a turn the daemon did not prompt: an Envoy notice
// delivered into its session, or a person's steer. The answer stands through that turn, so the
// round waits for the turn and for nothing else: no ask is published again, and the pod is
// replaced once that turn ends, before the round's bound.
//
// The negative control is the base branch's take-back, which withdraws an answer when such a turn
// starts: the round then waits for an answer nobody is asked for again, the pod waits to the
// bound, and the replacement here comes at the bound rather than before it.
func TestPodUpdateEarlyReleaseAnAnswerStandsThroughATurnTheDaemonDidNotPrompt(t *testing.T) {
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	clocks := rig.replacementClocks()
	rig.drifts(podUpdateDrift)

	rig.tick()
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
	}
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	asked := len(rig.asks())

	for _, at := range []time.Duration{time.Minute, 10 * time.Minute, podUpdateGrace - time.Minute} {
		rig.clockAt(podUpdateStart.Add(at))
		rig.tick()
		rig.tick()
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("at %s the pod was replaced %d times with the implementer's turn running", rig.clock.Now(), len(calls))
		}
		if c := rig.claimOf(claim.RoleImplementer); c.State != supervise.StateWorking || c.UpdateSignalledAt == nil {
			t.Fatalf("at %s the implementer is %s answered=%t, want it in its turn with its answer standing",
				rig.clock.Now(), c.State, c.UpdateSignalledAt != nil)
		}
	}
	if n := len(rig.asks()); n != asked {
		t.Fatalf("the sweep published %d asks while the turn ran, want none: the answer stands, so nobody is asked again", n-asked)
	}

	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})

	rig.awaitReplacement(1, "once the turn the daemon did not prompt ended")
	for _, at := range clocks() {
		if !at.Before(podUpdateBound) {
			t.Fatalf("the replacement was made at %s, want it before the bound %s: every answer stood through the turn", at, podUpdateBound)
		}
	}
	if lines := loggedLines(t, rig.logs.String(), podUpdateBoundLine); len(lines) != 0 {
		t.Fatalf("the sweep logged a bounded end %+v for a round its answers ended", lines)
	}
}

// An answered claim is handed no task until its round ends (sendPending's answeredOpenRound): a
// task queued for it waits, through the sweeps of the open round, and no prompt reaches its
// worker. The round ends with the pod's replacement once the other claim answers, and the task
// the round held is then sent.
//
// The negative control is the hold dropped: the task is then prompted into the worker at once,
// and the first assertion fails.
func TestPodUpdateEarlyReleaseAnAnsweredClaimIsHandedNoTaskUntilTheRoundEnds(t *testing.T) {
	const task = "review the pull request"
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	conn := rig.workers[claim.RoleImplementer]
	before := len(conn.Prompts())

	rig.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: rig.tokens[claim.RoleImplementer], Task: task})
	for _, at := range []time.Duration{time.Minute, 10 * time.Minute} {
		rig.clockAt(podUpdateStart.Add(at))
		rig.tick()
		rig.settle(claim.RoleImplementer)
		if sent := conn.Prompts()[before:]; len(sent) != 0 {
			t.Fatalf("at %s the answered implementer was sent %+v, want nothing until its round ends", rig.clock.Now(), sent)
		}
		if p := rig.claimOf(claim.RoleImplementer).Pending; p == nil || p.Task != task || !p.ConfirmedAt.IsZero() {
			t.Fatalf("at %s the implementer holds %+v, want the task %q held and unconfirmed", rig.clock.Now(), p, task)
		}
	}

	rig.signal(claim.RoleTester, "e4f5a6b")
	rig.awaitReplacement(1, "once the tester answered too")
	rig.awaitSwept(claim.RoleImplementer, "its round to end with the pod", func(c supervise.Claim) bool { return c.UpdateRound == "" })

	testwait.Eventually(t, "the held task to reach the implementer once its round ended", func() bool {
		for _, sent := range conn.Prompts()[before:] {
			if sent.Message == task {
				return true
			}
		}
		return false
	})
}

// A claim that answers brings the round's end forward, and a claim that says nothing delays
// nothing past the bound. Two claims, neither in a turn: where both answer ten minutes in, the pod
// is replaced then; where one says nothing, the pod waits for it a minute before the bound and is
// replaced at the bound.
func TestPodUpdateEarlyReleaseAnAnswerReleasesTheRoundAndSilenceDelaysNothingPastTheBound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []claim.Role
		// at is the clock the replacement is made at.
		at time.Time
	}{
		{name: "every claim answers", answers: []claim.Role{claim.RoleImplementer, claim.RoleTester},
			at: podUpdateStart.Add(10 * time.Minute)},
		{name: "one claim says nothing", answers: []claim.Role{claim.RoleImplementer}, at: podUpdateBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			clocks := rig.replacementClocks()
			rig.drifts(podUpdateDrift)
			rig.tick()
			rig.awaitRound(claim.RoleImplementer, podUpdateRound)
			rig.awaitRound(claim.RoleTester, podUpdateRound)

			rig.clockAt(podUpdateStart.Add(10 * time.Minute))
			for _, role := range tc.answers {
				rig.signal(role, "a1b2c3d")
			}
			if tc.at.After(rig.clock.Now()) {
				rig.clockAt(tc.at.Add(-time.Minute))
				rig.tick()
				rig.tick()
				if calls := rig.replacements(); len(calls) != 0 {
					t.Fatalf("at %s, before the bound %s, the pod was replaced %d times with a claim that has not answered",
						rig.clock.Now(), podUpdateBound, len(calls))
				}
				rig.clockAt(tc.at)
			}

			rig.awaitReplacement(1, "at "+tc.at.Format(time.RFC3339))
			for _, at := range clocks() {
				if !at.Equal(tc.at) {
					t.Fatalf("the replacement was made at %s, want %s", at, tc.at)
				}
			}
		})
	}
}
