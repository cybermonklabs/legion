package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A child-closed or a child-status notice is about the child, and the architect above it decides
// what the rest of the work under it does: a child-status tells of the child's first phase start,
// which starts the child's own architect beside it, of its leave or its return, or of its hold
// under an accepted parent, and a child-closed tells of its close. So the owner walk of either
// starts above the child on its Dispatch chain (NoticeStartsAtParent) and never ends at the
// child's own architect while an issue above it is in Legion's work, though that architect holds
// its role, as a newly started one does and as one does until its stop runs. An issue a person
// moved out from under its parent in Dispatch has nothing above it in Legion's work: it is a root
// of its own (R15 of dispatch://INFRA-3320), whatever tree its record stores, and hears its own at
// its gate root, itself.
func TestAChildsCloseAndStatusChangeStartAboveItOnTheChain(t *testing.T) {
	root := chainIssue("LEGION-1", "LEGION-1", "", phase.Implementing, "in_progress")
	child := chainIssue("LEGION-2", "LEGION-1", "LEGION-1", phase.Planning, "in_progress")
	orphan := chainIssue("LEGION-5", "LEGION-1", "", phase.Planning, "in_progress")
	issues := []Issue{root, child, orphan}
	chains := chainsOf(issues, copyOf(issues...))
	runs := func(token claim.Token) bool {
		return token == architectToken(t, root.Key) || token == architectToken(t, child.Key) || token == architectToken(t, orphan.Key)
	}
	for _, kind := range []NoticeKind{"child-closed", "child-status"} {
		t.Run(string(kind), func(t *testing.T) {
			fromParent := NoticeStartsAtParent(kind, claim.RoleArchitect)
			if got := OwningIssue("legion", chains, child.Key, fromParent, runs); got != root.Key {
				t.Errorf("a %s notice of %s under %s goes to the architect of %s, want %s's", kind, child.Key, root.Key, got, root.Key)
			}
			if got := OwningIssue("legion", chains, orphan.Key, fromParent, runs); got != orphan.Key {
				t.Errorf("a %s notice of %s, which has no parent in Dispatch and stores the tree of %s, goes to the architect of %s, want its own as the root it is",
					kind, orphan.Key, root.Key, got)
			}
		})
	}
}
