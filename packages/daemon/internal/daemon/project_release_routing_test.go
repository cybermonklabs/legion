package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim the project release stopped keeps its role for the notice routing, as a released claim
// does: its session, its phase and its task are as the release left them, and the daemon's next
// boot starts it again, so a notice for a sub-architect it stopped stays that sub-architect's
// rather than walking up to the architect above it. A claim whose stop is in flight keeps no role,
// and neither does a claim another source suspended.
func TestAClaimTheProjectReleaseStoppedHoldsItsRoleForTheNoticeRouting(t *testing.T) {
	for _, tc := range []struct {
		name string
		view supervise.ClaimView
		want bool
	}{
		{name: "stopped by the project release",
			view: supervise.ClaimView{Claim: supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByProjectRelease}}, want: true},
		{name: "released by the idle release",
			view: supervise.ClaimView{Claim: supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased}}, want: true},
		{name: "stopped by the project release while a stop is in flight",
			view: supervise.ClaimView{Claim: supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByProjectRelease}, Stopping: true}},
		{name: "suspended by the daemon",
			view: supervise.ClaimView{Claim: supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon}}},
		{name: "suspended by the operator",
			view: supervise.ClaimView{Claim: supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := holdsRole(tc.view); got != tc.want {
				t.Fatalf("holdsRole = %t, want %t", got, tc.want)
			}
		})
	}
}
