package daemon

import "testing"

// A released role whose wake was refused for the round's pod, and which the daemon or the
// operator then suspended before its launch reached any pod, is still named at the end of the
// round over a later pod of the issue's kept volume (workOnRoundPod). The earlier pod and the
// round's pod mount the same kept volume. The order table's takeover rows, and the rows of
// TestATakenOverReleasedRoleIsNamedWhereALaterPodOfItsKeptVolumeWent, take over a role the
// release still holds (supervise's suspendedAgain). This test suspends a woken one, whose launch
// is under way (supervise's suspended). suspendedTo brings each kind to its end: the implementer
// and the tester start in releasedPod on the kept volume, and the release suspends both. The
// tester wakes into P, the next pod of the same volume. Once the round opens and asks the tester,
// the implementer's wake is refused for P before its agent registers in P, and the daemon or the
// operator suspends the woken implementer, so its launch reached no pod. The tester answers, and P
// goes on its own or is recreated, with or without the exchange identity, while the implementer
// stays suspended. Control: a rule under which the daemon's or the operator's suspension of a
// woken claim whose launch is under way takes the pod's earlier work off that claim, so that
// workOnRoundPod holds of it only where an agent of it registered in the round's pod, while a
// takeover of a claim the release still holds keeps that work on it, fails every row; no row of
// the order table fails under it.
func TestAWokenReleasedRoleSuspendedBeforeAnyPodIsNamedWhereThePodWent(t *testing.T) {
	t.Parallel()
	for _, kind := range []memberKind{daemonSuspendedWokenKept, operatorSuspendedWokenKept} {
		for _, end := range []roundPodEnd{endPodGone, endRecreated, endRecreatedRefusing} {
			t.Run(string(kind.suspendsWoken())+": "+end.String(), func(t *testing.T) {
				rig := suspendedTo(t, end, kind)
				wantRoundEnd(t, rig, end, kind, "implementer (suspended)")
			})
		}
	}
}
