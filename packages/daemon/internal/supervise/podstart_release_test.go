package supervise

// The pod start harness's role of a claim stored suspended by another than the daemon: the project
// release's suspension (dispatch://INFRA-3583), whose claims the daemon's boot starts in
// TestTheBootsStartsOfTheClaimsAProjectReleaseStoppedGoAsNoRequestRaisedThem.

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// podRoleSuspendedBy is podRole of a claim stored suspended by by rather than by the daemon: the
// project release's suspension, for one, which the daemon's next boot resumes by rule.
func (h *harness) podRoleSuspendedBy(role claim.Role, by SuspendSource) *podRole {
	h.t.Helper()
	return h.podRoleOf(h.ctx, role, StateSuspended, by)
}

// The daemon's boot starts of the claims a project release stopped go as the starts no request
// raised do in TestThePodsStartsGoInTheDesignsOrder (podstart_test.go): the first to reach a pod
// that holds nothing goes first, whatever its role, and the ones that wait behind it go with the
// role working the issue's phase first, then the others in the order of claim.Roles.
func TestTheBootsStartsOfTheClaimsAProjectReleaseStoppedGoAsNoRequestRaisedThem(t *testing.T) {
	// The implementer works the phase. The daemon's next boot resumes by rule each claim the
	// project release stopped (workflow's startReleaseStopped), here in an order that puts the
	// phase worker last, while an operator's resume of the planner holds the pod.
	h := podHarness(t, claim.RoleImplementer)
	holder := h.podRole(claim.RolePlanner, StateSuspended)
	reviewer := h.podRoleSuspendedBy(claim.RoleReviewer, SuspendedByProjectRelease)
	tester := h.podRoleSuspendedBy(claim.RoleTester, SuspendedByProjectRelease)
	implementer := h.podRoleSuspendedBy(claim.RoleImplementer, SuspendedByProjectRelease)
	h.mustPostTo(holder.m, RequestResume{Claim: holder.token, By: SuspendedByOperator})
	bootStart := func(r *podRole) <-chan error {
		return h.postAsync(r.m, RequestRuleResume{Claim: r.token, Reason: "the boot starts a claim the project release stopped"})
	}

	reviewerStarted := bootStart(reviewer)
	h.wantWaitsForPod(reviewer, 0)
	testerStarted := bootStart(tester)
	h.wantWaitsForPod(tester, 0)
	implementerStarted := bootStart(implementer)
	h.wantWaitsForPod(implementer, 0)

	h.wantStartOrder(holder, waitingStart{implementer, implementerStarted}, waitingStart{tester, testerStarted},
		waitingStart{reviewer, reviewerStarted})
}
