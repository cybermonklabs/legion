package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A released role that the daemon or the operator then takes over (supervise's suspendedAgain)
// is still named at the end of the round over a later pod of the issue's kept volume
// (workOnRoundPod: a role the daemon or the operator took over, whatever suspended it since). The
// order table's takeover rows run on a pod with a volume of its own, which holds nothing from
// before it, so none of them names the role. Control: a rule under which a takeover of a claim an
// idle release suspended takes the pod's earlier work off that claim fails every row; no row of
// the order table fails under it.
func TestATakenOverReleasedRoleIsNamedWhereALaterPodOfItsKeptVolumeWent(t *testing.T) {
	for _, by := range []supervise.SuspendSource{supervise.SuspendedByDaemon, supervise.SuspendedByOperator} {
		for _, end := range []roundPodEnd{endPodGone, endRecreated, endRecreatedRefusing} {
			t.Run(string(by)+": "+end.String(), func(t *testing.T) {
				rig := takenOverKeptTo(t, end, by)
				// At these three ends wantRoundEnd reads of the kind only that the member did not
				// answer and that P's disk is the kept volume, which releasedKeptVolume says: an idle
				// release suspended the implementer in an earlier pod of the kept volume P mounts.
				wantRoundEnd(t, rig, end, releasedKeptVolume, "implementer (suspended)")
			})
		}
	}
}

// takenOverKeptTo brings a round over podUpdatePod to end. The implementer and the tester start
// in releasedPod on a kept volume, and the release suspends both. The tester wakes into P, the
// next pod of the same volume. Once the round opens and asks the tester, the daemon or the
// operator (by) takes the released implementer over, so no agent of it ever runs in P. The tester
// answers, and P goes on its own or is recreated, with or without the exchange identity, while
// the implementer stays suspended.
func takenOverKeptTo(t *testing.T, end roundPodEnd, by supervise.SuspendSource) *podUpdateRig {
	t.Helper()
	implementer, tester := claim.RoleImplementer, claim.RoleTester
	rig := newPodUpdateRig(t, podUpdateRole{role: implementer, state: supervise.StateQueued}, podUpdateRole{role: tester, state: supervise.StateQueued})
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
	refusals := rig.refuseLaunches()
	rig.ownsKeptVolume(keptVolumeMadeAt)
	rig.startInPod(implementer, releasedPod)
	rig.startInPod(tester, releasedPod)
	rig.releases(releasedPod, implementer, tester)
	rig.wakesInto(tester, podUpdatePod)
	rig.drifts(podUpdateExchangeDrift)

	rig.tick()
	rig.awaitRound(implementer, podUpdateRound)
	rig.awaitAsked(tester, podUpdateRound)
	rig.suspendBy(implementer, by)
	if c := rig.claimOf(implementer); c.State != supervise.StateSuspended || c.SuspendedBy != by || c.Session == "" ||
		len(c.RegisteredPods.Pods) != 1 || c.RegisteredPods.Pods[0] != releasedPod {
		t.Fatalf("the implementer is %s by %q with session %q and set %+v; want it taken over by the %s, with its session and the set of releasedPod alone",
			c.State, c.SuspendedBy, c.Session, c.RegisteredPods, by)
	}
	rig.signal(tester, "c0ffee")

	switch end {
	case endPodGone:
		rig.podGone()
	case endRecreated:
		rig.displacedInto(tester, podUpdateReplacementPod)
		rig.rt.ScriptPodDrift(rig.tokens[implementer])
		rig.rt.ScriptPodDrift(rig.tokens[tester])
	case endRecreatedRefusing:
		rig.goneRefused(refusals, tester, podUpdateReplacementPod, true)
		rig.rt.ScriptPodDrift(rig.tokens[implementer])
		rig.rt.ScriptPodDrift(rig.tokens[tester])
	default:
		t.Fatalf("end %s is not one this test brings about", end)
	}
	rig.awaitSwept(implementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.settleAll()
	return rig
}
