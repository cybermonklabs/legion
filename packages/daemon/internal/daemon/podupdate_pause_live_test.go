package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// dispatch://INFRA-4109, the tester's round at fc21b037: a round over a pod with a process in it
// keeps the pod for the launch that a project's pause still owes, whether or not that launch
// still records its role's session. The pause stopped the implementer, suspended released, and
// its resume ended the hold of the pod a minute before the round opened (recordPause), so the
// pause still owes the implementer a launch (podHolds.owes). The tester runs in the pod. The
// round opens over the pod and asks the tester. The implementer's held start runs, and its launch
// stops inside the runtime with no process recorded:
//
//   - with its session: the resume of the implementer's session holds.
//   - with its session forgotten: no session store holds the implementer's session, so the launch
//     forgets it and spawns a fresh agent in the same try (supervise's startFresh). The runtime
//     refuses that spawn, which is a charged launch failure, and the retry of the spawn holds.
//     The claim is then launching with no session and no process, as it is for the whole backoff
//     between the two tries (launchBackoffFirst), when the claim holds no launch turn of the pod.
//
// The tester answers, and the sweeps run. In both cases the pause owes the implementer a launch
// and no process of the implementer runs in the pod, so the pod is the pause's to keep for the
// implementer's work (decision 12 of dispatch://INFRA-3759). The round asks no claim with no
// session (roundAsks), so a keep read only through the round's sets keeps nothing for the second
// case, and the replacement through the tester's process deletes the pod that the pause keeps.
// No read of the workspace is made either, since the round passed over no claim with a session
// (passedOverClaims), so the issue is not told what the implementer left on the disk.
func TestALiveRoundKeepsThePodForTheLaunchAPauseOwesWhetherOrNotItKeepsItsSession(t *testing.T) {
	t.Parallel()
	const stopped = pauseStoppedRole
	for _, forgot := range []bool{false, true} {
		name := "with its session"
		if forgot {
			name = "with its session forgotten"
		}
		t.Run(name, func(t *testing.T) {
			rig := newPodUpdateRig(t, suspendedRole(stopped, supervise.SuspendedByReleased), readyWorker(claim.RoleTester))
			rig.recordPause(store.PausePodHoldResumed, stopped)
			rig.drifts(podUpdateDrift)
			token := rig.tokens[stopped]
			hold, holding := holdInRuntime(t)
			if forgot {
				rig.scriptForgottenSession(stopped, hold)
			} else {
				rig.rt.OnResume = hold
			}

			rig.tick()
			rig.awaitAsked(claim.RoleTester, podUpdateRound)
			rig.awaitRound(stopped, podUpdateRound)

			m, _ := rig.sup.Machine(token)
			rig.heldStartUntilHeld(stopped, holding)
			if c := m.View().Claim; c.State != supervise.StateLaunching || c.Locator != nil || (c.Session == "") != forgot {
				t.Fatalf("the %s is %s recording session %q and process %+v, want launching with no process and its session forgotten: %t",
					stopped, c.State, c.Session, c.Locator, forgot)
			}

			rig.signal(claim.RoleTester, "a1b2c3d")
			tester, _ := rig.sup.Machine(rig.tokens[claim.RoleTester])
			for range 3 {
				tester.Wait()
				testwait.Eventually(t, "the tester to be deciding nothing", func() bool { return tester.Deciding() == nil })
				rig.tick()
			}
			if calls := rig.replacements(); len(calls) != 0 {
				t.Fatalf("the pod was replaced %d times while the project's pause owes the %s a launch and no process of it runs in the pod", len(calls), stopped)
			}
		})
	}
}
