package admit

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// R19 of dispatch://INFRA-3320 starts an issue under a parent at work only on "the release_children
// of an architect above it or ... a person", and the architect's rulings
// (dispatch://INFRA-4730/comment/ada915ba-c94a-4876-8d6e-6cc1a1423f32 and
// dispatch://INFRA-4730/comment/317d3111-94a2-4777-81e9-ea15937a8c78) judge the writer of a todo on
// a child the daemon has not recorded with the same agent set as a status write on a recorded
// issue: the claims on the chain above and in the gate root's tree. The implementer of the middle
// issue's sibling holds a claim in the root's tree, which is the gate tree of the middle issue too
// (no gate holder stands between), and its write on a recorded child is set back
// (TestAWorkersStatusOnAChildIsSetBack's rule). It writes todo on a new child of the middle issue:
// nothing is recorded for the child.
func TestASiblingsTodoOnAnUnrecordedChildStartsNothing(t *testing.T) {
	const child = "LEGION-68"
	rig := seedOwnerTree(t, func(issues map[string]*record.Issue) { delete(issues, ownerGrandchild) })
	applyMirrored(t, rig.pool, rig.admission, child+"-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: child, Parent: ownerChild, Rank: child, ActorSession: siblingImplementer,
		Transition: &intake.StatusTransition{Seq: 2, StatusBefore: "backlog", Session: siblingImplementer}}, rig.engine)
	tick(t, rig.pool, rig.admission, child+"-after-todo")
	if stored := maybeIssue(t, rig.pool, child); stored != nil && record.InLegionsWork(*stored) {
		t.Errorf("%s after the todo of %s, the sibling's implementer, is recorded in Legion's work at %s/%s; want nothing recorded",
			child, siblingImplementer, stored.Phase, stored.Status)
	}
}
