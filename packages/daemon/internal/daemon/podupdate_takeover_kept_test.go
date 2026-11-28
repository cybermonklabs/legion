package daemon

import "testing"

// A released role that the daemon or the operator then takes over (supervise's suspendedAgain)
// is still named at the end of the round over a later pod of the issue's kept volume
// (workOnRoundPod: a role the daemon or the operator took over, whatever suspended it since). The
// order table's takeover rows run on a pod with a volume of its own, which holds nothing from
// before it, so none of them names the role. suspendedTo brings each kind to its end: the
// implementer and the tester start in releasedPod on the kept volume, and the release suspends
// both. The tester wakes into P, the next pod of the same volume. Once the round opens and asks
// the tester, the daemon or the operator takes the released implementer over, so no agent of it
// ever runs in P. The tester answers, and P goes on its own or is recreated, with or without the
// exchange identity, while the implementer stays suspended. Control: a rule under which a
// takeover of a claim an idle release suspended takes the pod's earlier work off that claim fails
// every row; no row of the order table fails under it.
func TestATakenOverReleasedRoleIsNamedWhereALaterPodOfItsKeptVolumeWent(t *testing.T) {
	t.Parallel()
	for _, kind := range []memberKind{daemonTakeoverKeptHeld, operatorTakeoverKeptHeld} {
		for _, end := range []roundPodEnd{endPodGone, endRecreated, endRecreatedRefusing} {
			t.Run(string(kind.takenOverBy())+": "+end.String(), func(t *testing.T) {
				rig := suspendedTo(t, end, kind)
				wantRoundEnd(t, rig, end, kind, "implementer (suspended)")
			})
		}
	}
}
