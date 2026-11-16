package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A child-closed or a child-status notice is about the child's own run ending or moving, so the
// architect above the child decides what the rest of the tree does, never the child's own
// architect, which the workflow stops with the child (OwningIssue's own rule). A child of a tree
// whose recorded parent is nil — a person moved it out from under its parent in Dispatch, and
// admission kept it in its tree (admit's recordObservation) — has no parent to start the walk at,
// so the architect above it is the tree root's: the outbox can publish the notice while the
// child's own architect still holds its role (its stop runs on another lane), and that architect
// must not be the one told its own issue closed or left.
func TestAParentlessChildsCloseAndStatusChangeReachTheTreeRoot(t *testing.T) {
	root, orphan := "LEGION-1", "LEGION-5"
	tree := map[string]Issue{
		root:   {Key: root, Tree: root},
		orphan: {Key: orphan, Tree: root},
	}
	// The child's own architect still holds its role, as it does until its stop runs.
	runs := func(token claim.Token) bool {
		return token == architectToken(t, root) || token == architectToken(t, orphan)
	}
	for _, kind := range []NoticeKind{"child-closed", "child-status"} {
		t.Run(string(kind), func(t *testing.T) {
			if got := OwningIssue("legion", tree, tree[orphan], kind, claim.RoleArchitect, runs); got != root {
				t.Fatalf("a %s notice of the parentless child %s goes to the architect of %s, want the tree root %s's", kind, orphan, got, root)
			}
		})
	}
}
