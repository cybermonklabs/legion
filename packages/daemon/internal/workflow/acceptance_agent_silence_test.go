package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The silence rule's half that is not about a moment (unpromptedPhaseWorker) reads the worker of
// the open phase with the issue (OpenRoleOf) and whether that worker has work there
// (record.AtWork). At accepting on a step `acceptance` with no key, the fresh agent of acceptance
// works the place (Issue.PlaceRole acceptance), so its claim, up between turns holding no task and
// silent past its bound, is one nothing prompted, and is named. At a step `acceptance:owner` the
// owner works the place (PlaceRole architect), which is no phase worker, so neither the role
// acceptance nor the architect is named.
func TestTheSilenceRuleFindsAFreshAcceptanceAgentNothingPrompted(t *testing.T) {
	fresh := aSilentImplementer()
	fresh.issue.Phase, fresh.issue.PlaceRole, fresh.role = phase.Accepting, claim.RoleAcceptance, claim.RoleAcceptance
	if !unpromptedPhaseWorker(fresh.issue, fresh.root, fresh.gate, fresh.role, fresh.run) || !fresh.answer() {
		t.Fatalf("the fresh agent of acceptance of an issue at accepting, up between turns with no task and silent past its bound, is not named its unprompted phase worker")
	}
	owner := fresh
	owner.issue.PlaceRole = claim.RoleArchitect
	for _, role := range []claim.Role{claim.RoleAcceptance, claim.RoleArchitect} {
		owner.role = role
		if unpromptedPhaseWorker(owner.issue, owner.root, owner.gate, owner.role, owner.run) || owner.answer() {
			t.Fatalf("at a step acceptance:owner the %s's claim is named an unprompted phase worker, want none: the owner works the place", role)
		}
	}
}
