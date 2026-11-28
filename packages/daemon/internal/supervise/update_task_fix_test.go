package supervise

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A pod update round's ask tells a step beside a running fix to push to a branch of its own, by
// the one command that pushes there (workspace.BesidePushCommand), and tells the fix to push to
// its own branch F. The test reads values the program builds and no sentence: each branch is
// claim.BesideBranch's answer, which claim.IsBesideBranch accepts, and the composed task holds
// that branch's push command as BesidePushCommand returns it. The fix's task holds F and not the
// shared workspace's push of F, since the fix pushes from its own working copy.
func TestAPodUpdateTaskBesideAFixHoldsItsOwnBranchAndItsPushCommand(t *testing.T) {
	const key = "LEGION-484"
	endsAt := time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC)
	beside := func(role claim.Role) string {
		t.Helper()
		branch, err := claim.BesideBranch(key, 1, "a", 1, role)
		if err != nil || !claim.IsBesideBranch(branch) {
			t.Fatalf("BesideBranch(%s, 1, a, 1, %s) = %q, %v; want a branch beside the fix", key, role, branch, err)
		}
		return branch
	}
	fix := beside(claim.RoleImplementer)

	for _, role := range []claim.Role{claim.RoleTester, claim.RoleArchitect, claim.RoleMerger} {
		own := beside(role)
		text := UpdateTask(role, testRound, testReasons, endsAt, UpdatePush{Branch: own})
		if !strings.Contains(text, workspace.BesidePushCommand(own)) {
			t.Errorf("the ask to the %s beside the fix holds no push of its own branch %s, want BesidePushCommand(%s)", role, own, own)
		}
		if strings.Contains(text, fix) {
			t.Errorf("the ask to the %s beside the fix names the fix's own branch %s, want its own %s alone", role, fix, own)
		}
	}

	text := UpdateTask(claim.RoleImplementer, testRound, testReasons, endsAt, UpdatePush{Branch: fix, OwnFix: true})
	if !strings.Contains(text, fix) {
		t.Errorf("the ask to the fix names no branch %s, want its own", fix)
	}
	if strings.Contains(text, workspace.BesidePushCommand(fix)) {
		t.Errorf("the ask to the fix holds the shared workspace's push of %s, want the push from the fix's own working copy", fix)
	}
}
