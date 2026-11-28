package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// dispatch://INFRA-2746 R1's merge-forward term (record.MergeForwardHolds): an ask holds a role
// only until it lands. Every landed row of
// TestMergeForwardHoldsOnlyTheRolesItsHoldReachesAtItsHeadAndPhaseUntilItLands also moves the pull
// request's head to the landed commit, so the head term (an ask of another head holds nothing)
// already frees each role there, and those rows pass with the landed term gone. This test holds
// the landed term alone: the ask records the head its merge left (MergeForward.Landed), and the
// pull request stands again at the head the ask was made of, not at that landed head, as a head
// that came back leaves it. Such an ask holds no role, in the phase it was made in and, in
// merging, whoever it asked.
func TestMergeForwardHoldsNoRoleOnceItLandedThoughThePullRequestStandsAgainAtItsHead(t *testing.T) {
	const head, landed = "c0de", "d00d"
	roles := append([]claim.Role{claim.RoleController}, claim.Roles...)
	for _, c := range []struct {
		name string
		// in is the phase the ask was made in, at the phase the issue stands in, and asked the
		// role the ask names ("" for an ask a daemon before #4979 wrote).
		in, at phase.Phase
		asked  claim.Role
	}{
		{name: "an ask made in testing", in: phase.Testing, at: phase.Testing},
		{name: "an ask made in reviewing", in: phase.Reviewing, at: phase.Reviewing},
		{name: "an ask of the implementer made in reviewing, carried into merging", in: phase.Reviewing, at: phase.Merging,
			asked: claim.RoleImplementer},
		{name: "an ask that records no asked role, made in merging", in: phase.Merging, at: phase.Merging},
	} {
		pullRequest := &PullRequest{State: PullRequestOpen, Number: 42, HeadSHA: head,
			MergeForward: &MergeForward{Head: head, Base: "main", Phase: c.in, Role: c.asked, Why: ConflictCheckNotGreen}}
		// Before it lands, the ask holds the worker of the phase the issue stands in and no other
		// role, so the landed rows below test the landed term and not a shape the hold never reaches.
		for _, role := range roles {
			if got, want := MergeForwardHolds(pullRequest, c.at, role), role == phase.RoleFor(c.at); got != want {
				t.Fatalf("%s, not landed, holds the %s in %s = %t, want %t", c.name, role, c.at, got, want)
			}
		}
		pullRequest.MergeForward.Landed = landed
		for _, role := range roles {
			if MergeForwardHolds(pullRequest, c.at, role) {
				t.Errorf("%s that landed at %s holds the %s in %s while the pull request stands again at %s, want no hold",
					c.name, landed, role, c.at, head)
			}
		}
	}
}
