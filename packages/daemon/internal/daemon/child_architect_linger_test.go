package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A child whose first phase started gets its own architect for the rest of its run, even when
// its tree lingers before the outbox reaches that architect's start. The start waits behind the
// child's branch row on the child's lane; a person moves the root out of the workflow meanwhile,
// so the tree lingers and the outbox finishes the start without creating the claim (as it does
// every start of a lingering tree's member); then the person sets the root back to todo inside its
// linger, which reopens the tree and brings back each role of its mid-phase members
// (admit's startMidPhaseMembers). The child's planner comes back there, and its architect must
// come back with it: R1 starts an architect on every child at its first phase start, and D9's one
// exception is a child started before the deploy. A later phase start revives only a failed claim
// (workflow's reviveArchitect), so a start that brings back nothing here leaves the child with no
// architect of its own until it closes.
func TestAChildsArchitectStartDroppedByALingerComesBackWithTheTree(t *testing.T) {
	rig := newChildArchitectRig(t)
	rig.written("release", childAt("todo", 2))
	if got := readIssue(t, rig.pool, doorChild); got.Phase != phase.Planning {
		t.Fatalf("the released child stands at %s, want planning", got.Phase)
	}
	if starts := rig.architectStarts(doorChild); starts != 1 {
		t.Fatalf("the child's first phase start queued %d architect starts, want one", starts)
	}

	// The root leaves the workflow before the outbox runs the child's rows: its tree lingers, and
	// the outbox finishes every start of the tree's members without acting.
	rig.apply("root-parked", intake.DispatchIssue{Key: doorRoot, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "backlog", HandedOver: true,
		Title: "root", Rank: "U"})
	rig.drain()
	if _, found := rig.sup.Machine(mustClaimToken(t, doorChild, claim.RoleArchitect)); found {
		t.Fatal("the lingering tree's child got an architect claim; want the start finished without acting, as every member's start of a lingering tree is")
	}

	// The person sets the root back to todo inside its linger: the tree is reopened with its
	// children, and each mid-phase member's roles that ran come back (startMidPhaseMembers).
	rig.apply("root-todo", intake.DispatchIssue{Key: doorRoot, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: "todo", HandedOver: true,
		Title: "root", Rank: "U"})
	if got := readIssue(t, rig.pool, doorChild); got.Phase != phase.Planning || got.Tree != doorRoot {
		t.Fatalf("the child after the root's reopen = %s in the tree of %s, want planning in the tree of %s", got.Phase, got.Tree, doorRoot)
	}
	var plannerStarts int
	if err := rig.pool.QueryRow(rig.ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'planner'`, doorChild).Scan(&plannerStarts); err != nil {
		t.Fatalf("count the planner starts of %s: %v", doorChild, err)
	}
	if plannerStarts == 0 {
		t.Fatal("the reopened tree queued no start of the child's planner; the scenario did not reach the reopen's restart of its mid-phase members")
	}
	if starts := rig.architectStarts(doorChild); starts == 0 {
		t.Fatalf("the reopened tree queued %d start of the child's planner and none of the child's own architect, which its first phase start owed it and the lingering tree's outbox never created", plannerStarts)
	}
}
