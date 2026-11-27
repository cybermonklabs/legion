package supervise

// The pod start harness's role of a claim stored suspended by another than the daemon: the project
// release's suspension (dispatch://INFRA-3583), whose claims the daemon's boot starts in
// TestThePodsStartsGoInTheDesignsOrder (podstart_test.go).

import "github.com/sjawhar/legion/daemon/internal/claim"

// podRoleSuspendedBy is podRole of a claim stored suspended by by rather than by the daemon: the
// project release's suspension, for one, which the daemon's next boot resumes by rule.
func (h *harness) podRoleSuspendedBy(role claim.Role, by SuspendSource) *podRole {
	h.t.Helper()
	return h.podRoleOf(h.ctx, role, StateSuspended, by)
}
