package daemon

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A released role whose wake was refused for the round's pod, and which the daemon or the
// operator then suspended before its launch reached any pod, is still named at the end of the
// round over a later pod of the issue's kept volume (workOnRoundPod). After #4869 (INFRA-3271)
// the test uses a kept volume rather than a restored bundle: the earlier pod and the round's pod
// mount the same volume. The order table's takeover rows suspend a role the release still holds
// (supervise's suspendedAgain); this test suspends a woken one, whose launch is under way
// (supervise's suspended). Control: a rule under which the daemon's or the operator's suspension
// of a woken claim whose launch is under way takes the pod's earlier work off that claim, so that
// workOnRoundPod holds of it only where an agent of it registered in the round's pod, while a
// takeover of a claim the release still holds keeps that work on it, fails every row; no row of
// the order table fails under it.
func TestAWokenReleasedRoleSuspendedBeforeAnyPodIsNamedWhereThePodWent(t *testing.T) {
	for _, by := range []supervise.SuspendSource{supervise.SuspendedByDaemon, supervise.SuspendedByOperator} {
		for _, end := range []roundPodEnd{endPodGone, endRecreated, endRecreatedRefusing} {
			t.Run(string(by)+": "+end.String(), func(t *testing.T) {
				rig := wokenSuspendedTo(t, end, by)
				wantRoundEnd(t, rig, end, wokenSuspendedBeforeAnyPod, "implementer (suspended)")
			})
		}
	}
}

// wokenSuspendedTo brings a round over podUpdatePod to end. The implementer and tester start in
// releasedPod on a kept volume. The tester wakes into P, the next pod of the same volume. Once the
// round opens and asks the tester, the implementer's wake is refused for P before its agent
// registers in P, and the daemon or the operator (by) suspends the woken implementer, so its
// launch reached no pod. The tester answers, and P goes on its own or is recreated, with or
// without the exchange identity, while the implementer stays suspended.
func wokenSuspendedTo(t *testing.T, end roundPodEnd, by supervise.SuspendSource) *podUpdateRig {
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
	refusals.refuse(implementer, podUpdatePod)
	if err := rig.machineOf(implementer).Handle(rig.ctx, rig.wakeFor(implementer)); !errors.Is(err, runtime.ErrAwaitingPodRound) {
		t.Fatalf("the implementer's wake answered %v, want its launch refused for the pod's round", err)
	}
	if err := rig.machineOf(implementer).Handle(rig.ctx, supervise.RequestSuspend{Claim: rig.tokens[implementer], By: by}); err != nil &&
		!errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("suspend the woken implementer by %s: %v", by, err)
	} else if err != nil {
		rig.retryLaunch()
	}
	c := rig.awaitClaim(implementer, "its suspension before any pod", func(c supervise.Claim) bool {
		return c.State == supervise.StateSuspended && c.Locator == nil && c.AwaitsPod == nil
	})
	if c.SuspendedBy != by || c.Session == "" || len(c.RegisteredPods.Pods) != 1 || c.RegisteredPods.Pods[0] != releasedPod {
		t.Fatalf("the implementer is suspended by %q with session %q and set %+v; want the %s's suspension, its session and the set of releasedPod alone",
			c.SuspendedBy, c.Session, c.RegisteredPods, by)
	}
	rig.settleAll()
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
