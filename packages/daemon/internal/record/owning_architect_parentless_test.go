package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A child-closed or a child-status notice is about the child, and the architect above it decides
// what the rest of the tree does: a child-status tells of the child's first phase start, which
// starts the child's own architect beside it, of its leave or its re-entry, of its joining the
// tree, or of its hold under an accepted parent, and a child-closed tells of its close. So the
// owner walk of either starts at the child's parent and never ends at the child's own architect
// (NoticeStartsAtParent). A child of a tree whose recorded parent is nil — a person moved it out
// from under its parent in Dispatch, and admission kept it in its tree (admit's recordObservation) —
// has no parent to start the walk at, so the architect above it is the tree root's. The outbox can
// publish such a notice while the child's own architect holds its role (one just started with the
// child's first phase, or one whose stop, on another lane, has not run yet), and that architect
// must not be the one told of its own issue.
func TestAParentlessChildsCloseAndStatusChangeReachTheTreeRoot(t *testing.T) {
	root, orphan := "LEGION-1", "LEGION-5"
	tree := map[string]Issue{
		root:   {Key: root, Tree: root},
		orphan: {Key: orphan, Tree: root},
	}
	// The child's own architect holds its role, as a newly started one does, and as one does until
	// its stop runs.
	runs := func(token claim.Token) bool {
		return token == architectToken(t, root) || token == architectToken(t, orphan)
	}
	for _, kind := range []NoticeKind{"child-closed", "child-status"} {
		t.Run(string(kind), func(t *testing.T) {
			if got := OwningIssue("legion", tree, tree[orphan], NoticeStartsAtParent(kind, claim.RoleArchitect), runs); got != root {
				t.Fatalf("a %s notice of the parentless child %s goes to the architect of %s, want the tree root %s's", kind, orphan, got, root)
			}
		})
	}
}

// Every phase-finished starts its owner walk at its issue. One that names a role is a worker's own,
// which the issue's own architect hears while it holds its role. The one that names no role is the
// gate's opening of a tree's own root (workflow's noticeFor), which the root's own architect hears.
// A child's gate opening sends no phase-finished: the child-status of its start tells the
// architect above it (workflow's ChildStartedNotice).
func TestEveryPhaseFinishedStartsAtItsIssue(t *testing.T) {
	for _, role := range append([]claim.Role{""}, claim.Roles...) {
		if NoticeStartsAtParent("phase-finished", role) {
			t.Errorf("a phase-finished of role %q starts at the issue's parent, want the issue", role)
		}
	}
	root, child := "LEGION-1", "LEGION-2"
	tree := map[string]Issue{
		root:  {Key: root, Tree: root},
		child: {Key: child, Tree: root, Parent: &root},
	}
	runs := func(token claim.Token) bool {
		return token == architectToken(t, root) || token == architectToken(t, child)
	}
	if got := OwningIssue("legion", tree, tree[root], NoticeStartsAtParent("phase-finished", ""), runs); got != root {
		t.Errorf("the phase-finished of the gate's opening of the root %s goes to the architect of %s, want the root's own", root, got)
	}
}
