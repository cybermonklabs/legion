package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A notice owner-step goes to the owner of its issue's task, the issue's own architect, whether
// that claim runs or not; every other notice of the issue goes to the architect that owns it now.
// In the tree LEGION-1 > LEGION-2 > LEGION-3, LEGION-2's sub-architect runs (an operator started
// it) and LEGION-3's own architect does not (its pod was released, or it never started): the
// owner-step of LEGION-3 reaches LEGION-3's architect, and the phase-finished of LEGION-3, in the
// same tree and the same claims, reaches LEGION-2's.
func TestAnOwnerStepGoesToTheArchitectOfItsIssue(t *testing.T) {
	t.Parallel()
	const project = "legion"
	issue := func(key, parent string) record.Issue {
		i := record.Issue{Key: key, Tree: "LEGION-1", Project: project}
		if parent != "" {
			i.Parent = &parent
		}
		return i
	}
	tree := record.TreeByKey([]record.Issue{issue("LEGION-1", ""), issue("LEGION-2", "LEGION-1"), issue("LEGION-3", "LEGION-2")})
	root, sub := mustClaimToken(t, "LEGION-1", claim.RoleArchitect), mustClaimToken(t, "LEGION-2", claim.RoleArchitect)
	runs := func(token claim.Token) bool { return token == root || token == sub }
	for _, tc := range []struct {
		kind record.NoticeKind
		want claim.Token
	}{
		{record.OwnerStepNotice, mustClaimToken(t, "LEGION-3", claim.RoleArchitect)},
		{"phase-finished", sub},
	} {
		got, err := noticeArchitect(project, tree, tree["LEGION-3"], record.Notice{Kind: tc.kind, Phase: "implementing"}, runs)
		if err != nil || got != tc.want {
			t.Errorf("the %s of LEGION-3 goes to %q (%v), want %q", tc.kind, got, err, tc.want)
		}
	}
}
