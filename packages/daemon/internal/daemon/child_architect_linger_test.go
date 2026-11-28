package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A child whose first phase started gets its own architect for the rest of its run, even when its
// tree lingers before the outbox reaches that architect's first start. That start is the plain one
// the child's first phase start queues after the child's branch row (workflow's gateOpened), and it
// finds no claim. A person moves the root out of the workflow before the outbox runs it, so the
// tree lingers, and the outbox holds the start on its backoff with no claim created
// (childArchitectFirstStart, startRoot), where it finishes every other start of a lingering tree's
// member without acting, the planner's among them. The person then sets the root back to todo
// inside its linger, which re-admits the tree and brings back, for each mid-phase member, the claim
// of the agent on the step it stands in, stored or not, and each other stored claim of it, on a
// start naming that claim (admit's startMidPhaseMembers): the child's planner's, and not its
// architect's, which works no phase here and is no stored claim. So the held first start is the
// one row that gives the child its architect,
// and it must still be queued after the reopen. Had the outbox finished it, nothing would queue it
// again: a later phase start revives only a failed claim (workflow's reviveArchitect), and the
// child would run to its close with no architect of its own, which R1 allows only for a child
// started before the change (plan D9). TestAChildsArchitectFirstStartWaitsForItsTreeToRunAgain
// reads the held start act.
func TestAChildsArchitectFirstStartHeldByALingerIsQueuedWhenTheTreeReopens(t *testing.T) {
	t.Parallel()
	rig := newChildArchitectRig(t)
	rig.written("release", childAt("todo", 2))
	if got := readIssue(t, rig.pool, doorChild); got.Phase != phase.Planning {
		t.Fatalf("the released child stands at %s, want planning", got.Phase)
	}
	if starts := rig.architectStarts(doorChild); starts != 1 {
		t.Fatalf("the child's first phase start queued %d architect starts, want one", starts)
	}

	// The root leaves the workflow before the outbox runs the child's rows: its tree lingers, and
	// the outbox holds the child's architect start with no claim created.
	rig.apply("root-parked", intake.DispatchIssue{Key: doorRoot, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "backlog", HandedOver: true,
		Title: "root", Rank: "U"})
	rig.drain()
	if _, found := rig.sup.Machine(mustClaimToken(t, doorChild, claim.RoleArchitect)); found {
		t.Fatal("the lingering tree's child got an architect claim; want its first start held with no claim created while the tree lingers")
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
		t.Fatalf("the reopened tree queued %d start of the child's planner and left no start of the child's own architect queued: its first phase start owed it one, and the lingering tree's outbox created no claim for it", plannerStarts)
	}
}
