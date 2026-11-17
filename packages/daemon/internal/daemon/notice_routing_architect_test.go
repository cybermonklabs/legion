package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A worker-died or a worker-stalled about a child's architect is about that architect, so it
// starts at the child's parent: the running architect above the child hears it — the parent's,
// else the nearest running one above that, else the root's. The child's architect never hears it
// itself, which a stalled claim would, since it still holds its role, not even when the record's
// parents loop back to the child: that walk ends at the root. The same notices about a phase
// worker of the child go to the child's own running architect, which is the one that acts on its
// worker, and a root architect's own wake goes to the root's claim, as every notice of the root
// does. The runs are the outbox's own (holdsRole), so deliverNotice, its fence (earlierNoticeFor)
// and the ready's release (releaseWaiting) all pick this architect.
func TestAWakeAboutAnArchitectReachesTheArchitectAboveIt(t *testing.T) {
	root, parent, child, grandchild, orphan, stray, unrecorded := "LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4", "LEGION-5", "LEGION-6", "LEGION-77"
	looped, loop := "LEGION-8", "LEGION-9"
	tree := map[string]record.Issue{
		root:       {Key: root, Tree: root},
		parent:     {Key: parent, Tree: root, Parent: &root},
		child:      {Key: child, Tree: root, Parent: &parent},
		grandchild: {Key: grandchild, Tree: root, Parent: &child},
		orphan:     {Key: orphan, Tree: root},
		stray:      {Key: stray, Tree: root, Parent: &unrecorded},
		looped:     {Key: looped, Tree: root, Parent: &loop},
		loop:       {Key: loop, Tree: root, Parent: &looped},
	}
	working, failed := supervise.StateWorking, supervise.StateFailed
	for _, tc := range []struct {
		name   string
		issue  string
		kind   record.NoticeKind
		role   claim.Role
		states map[string]supervise.ClaimState
		owner  string
	}{
		{name: "a sub-architect's death under its parent's running architect", issue: child, kind: "worker-died", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{parent: working, child: failed}, owner: parent},
		{name: "a sub-architect's stall under its parent's running architect", issue: child, kind: "worker-stalled", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{parent: working, child: working}, owner: parent},
		{name: "a sub-architect's death whose parent has no running architect", issue: child, kind: "worker-died", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{parent: failed, child: failed}, owner: root},
		{name: "a sub-architect's stall whose parent has no running architect", issue: child, kind: "worker-stalled", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{child: working}, owner: root},
		{name: "a sub-architect's death under the nearest running architect above its parent", issue: grandchild, kind: "worker-died", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{parent: working, child: failed, grandchild: failed}, owner: parent},
		{name: "a sub-architect's stall under the root", issue: parent, kind: "worker-stalled", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{parent: working, child: working}, owner: root},
		{name: "a sub-architect's stall on a child with no parent", issue: orphan, kind: "worker-stalled", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{orphan: working}, owner: root},
		{name: "a sub-architect's stall under a parent that is not recorded", issue: stray, kind: "worker-stalled", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{stray: working}, owner: root},
		{name: "a sub-architect's stall whose parent's parent is itself", issue: looped, kind: "worker-stalled", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{looped: working, loop: failed}, owner: root},
		{name: "a planner's death on the child", issue: child, kind: "worker-died", role: claim.RolePlanner,
			states: map[string]supervise.ClaimState{parent: working, child: working}, owner: child},
		{name: "an implementer's death on the child", issue: child, kind: "worker-died", role: claim.RoleImplementer,
			states: map[string]supervise.ClaimState{parent: working, child: working}, owner: child},
		{name: "an implementer's stall on the child", issue: child, kind: "worker-stalled", role: claim.RoleImplementer,
			states: map[string]supervise.ClaimState{parent: working, child: working}, owner: child},
		{name: "an implementer's death on a child whose architect failed", issue: child, kind: "worker-died", role: claim.RoleImplementer,
			states: map[string]supervise.ClaimState{parent: working, child: failed}, owner: parent},
		{name: "the root architect's death", issue: root, kind: "worker-died", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{parent: working, child: working}, owner: root},
		{name: "the root architect's stall", issue: root, kind: "worker-stalled", role: claim.RoleArchitect,
			states: map[string]supervise.ClaimState{parent: working, child: working}, owner: root},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := map[claim.Token]supervise.ClaimState{mustClaimToken(t, root, claim.RoleArchitect): working}
			for issue, state := range tc.states {
				states[mustClaimToken(t, issue, claim.RoleArchitect)] = state
			}
			got, err := record.OwningArchitect("legion", tree, tree[tc.issue], record.NoticeStartsAtParent(tc.kind, tc.role), func(token claim.Token) bool {
				return holdsRole(supervise.ClaimView{Claim: supervise.Claim{State: states[token]}})
			})
			if want := mustClaimToken(t, tc.owner, claim.RoleArchitect); err != nil || got != want {
				t.Fatalf("owner = %s, %v; want %s", got, err, want)
			}
		})
	}
}
