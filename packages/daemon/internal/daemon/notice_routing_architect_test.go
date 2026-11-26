package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A worker-died or a worker-stalled about a child's architect is about that architect, so it
// starts above the child on its Dispatch chain: the running architect above the child hears it —
// the parent's, else the nearest running one above that, else the gate root's. The child's
// architect never hears it itself while an issue above it is in Legion's work, which a stalled
// claim would, since it still holds its role. An issue with nothing above it in Legion's work — a
// root, one with no parent in Dispatch, or one under an issue the daemon does not record — hears
// its own, and one whose chain loops back on itself waits until the daemon has read Dispatch
// again. The same notices about a phase worker of the child go to the child's own running
// architect, which is the one that acts on its worker. The runs are the outbox's own (holdsRole),
// so deliverNotice, its fence (earlierNoticeFor) and the ready's release (releaseWaiting) all pick
// this architect.
func TestAWakeAboutAnArchitectReachesTheArchitectAboveIt(t *testing.T) {
	t.Parallel()
	root, parent, child, grandchild, orphan, stray := "LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4", "LEGION-5", "LEGION-6"
	looped, loop := "LEGION-8", "LEGION-9"
	issues := []record.Issue{
		routingIssue(root, root, ""), routingIssue(parent, root, root), routingIssue(child, root, parent),
		routingIssue(grandchild, root, child), routingIssue(orphan, root, ""), routingIssue(stray, root, "LEGION-77"),
		routingIssue(looped, root, loop), routingIssue(loop, root, looped),
	}
	keys := make([]string, len(issues))
	for i, issue := range issues {
		keys[i] = issue.Key
	}
	route := routingChains(t, issues, map[string]string{"LEGION-77": ""}, keys)
	for _, tc := range []struct {
		name   string
		issue  string
		kind   record.NoticeKind
		role   claim.Role
		claims map[string]routeClaim
		owner  string
	}{
		{name: "a sub-architect's death under its parent's running architect", issue: child, kind: "worker-died", role: claim.RoleArchitect,
			claims: map[string]routeClaim{parent: holds, child: failedHere}, owner: parent},
		{name: "a sub-architect's stall under its parent's running architect", issue: child, kind: "worker-stalled", role: claim.RoleArchitect,
			claims: map[string]routeClaim{parent: holds, child: holds}, owner: parent},
		{name: "a sub-architect's run of provider errors under its parent's running architect", issue: child, kind: record.ProviderErrorsNotice, role: claim.RoleArchitect,
			claims: map[string]routeClaim{parent: holds, child: holds}, owner: parent},
		{name: "a sub-architect's run of provider errors whose parent has no running architect", issue: child, kind: record.ProviderErrorsNotice, role: claim.RoleArchitect,
			claims: map[string]routeClaim{child: holds}, owner: root},
		{name: "an implementer's run of provider errors on the child", issue: child, kind: record.ProviderErrorsNotice, role: claim.RoleImplementer,
			claims: map[string]routeClaim{parent: holds, child: holds}, owner: child},
		{name: "a sub-architect's death whose parent has no running architect", issue: child, kind: "worker-died", role: claim.RoleArchitect,
			claims: map[string]routeClaim{parent: failedHere, child: failedHere}, owner: root},
		{name: "a sub-architect's stall whose parent has no running architect", issue: child, kind: "worker-stalled", role: claim.RoleArchitect,
			claims: map[string]routeClaim{child: holds}, owner: root},
		{name: "a sub-architect's death under the nearest running architect above its parent", issue: grandchild, kind: "worker-died", role: claim.RoleArchitect,
			claims: map[string]routeClaim{parent: holds, child: failedHere, grandchild: failedHere}, owner: parent},
		{name: "a sub-architect's stall under the root", issue: parent, kind: "worker-stalled", role: claim.RoleArchitect,
			claims: map[string]routeClaim{parent: holds, child: holds}, owner: root},
		{name: "a stall of the architect of an issue with no parent", issue: orphan, kind: "worker-stalled", role: claim.RoleArchitect,
			claims: map[string]routeClaim{orphan: holds}, owner: orphan},
		{name: "a stall of the architect of an issue under one the daemon does not record", issue: stray, kind: "worker-stalled", role: claim.RoleArchitect,
			claims: map[string]routeClaim{stray: holds}, owner: stray},
		{name: "a stall of the architect of an issue whose chain loops", issue: looped, kind: "worker-stalled", role: claim.RoleArchitect,
			claims: map[string]routeClaim{looped: holds, loop: failedHere}, owner: "waits"},
		{name: "a planner's death on the child", issue: child, kind: "worker-died", role: claim.RolePlanner,
			claims: map[string]routeClaim{parent: holds, child: holds}, owner: child},
		{name: "an implementer's death on the child", issue: child, kind: "worker-died", role: claim.RoleImplementer,
			claims: map[string]routeClaim{parent: holds, child: holds}, owner: child},
		{name: "an implementer's stall on the child", issue: child, kind: "worker-stalled", role: claim.RoleImplementer,
			claims: map[string]routeClaim{parent: holds, child: holds}, owner: child},
		{name: "an implementer's death on a child whose architect failed", issue: child, kind: "worker-died", role: claim.RoleImplementer,
			claims: map[string]routeClaim{parent: holds, child: failedHere}, owner: parent},
		{name: "the root architect's death", issue: root, kind: "worker-died", role: claim.RoleArchitect,
			claims: map[string]routeClaim{parent: holds, child: holds}, owner: root},
		{name: "the root architect's stall", issue: root, kind: "worker-stalled", role: claim.RoleArchitect,
			claims: map[string]routeClaim{parent: holds, child: holds}, owner: root},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := map[string]routeClaim{root: holds}
			for issue, held := range tc.claims {
				claims[issue] = held
			}
			if got := routedTo(t, route, tc.issue, record.Notice{Kind: tc.kind, Role: tc.role}, claims); got != tc.owner {
				t.Fatalf("owner = %s; want %s", got, tc.owner)
			}
		})
	}
}
