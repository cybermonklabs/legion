package admit

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// R19 of dispatch://INFRA-3320: "An issue with an issue above it in Legion's work starts when its
// parent is in Legion's work and Dispatch shows the issue todo, written by the release_children of
// an architect above it or by a person" (the architect's ruling,
// dispatch://INFRA-4730/comment/ada915ba-c94a-4876-8d6e-6cc1a1423f32). A todo on a child the daemon
// has not recorded, written by the parent's own implementer, whose session holds a claim on the
// parent, is neither: it starts nothing, as the same session's todo on a recorded child stopped at
// backlog is set back. A person's todo on such a child starts it.
func TestAnAgentsTodoOnAChildUnderAParentAtWorkStartsNothing(t *testing.T) {
	const parent, session = "LEGION-640", "ses-parent-implementer"
	agent := ownerClaim{tree: parent, issue: parent, role: claim.RoleImplementer, session: session, state: supervise.StateWorking}
	t.Run("an unrecorded child, the parent's implementer's todo", func(t *testing.T) {
		const child = "LEGION-641"
		pool := migratedPool(t)
		admission := quiet(t, 3)
		seedRunning(t, pool, parent, "")
		seedSessionClaims(t, pool, agent)
		applyMirrored(t, pool, admission, "agent-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 1,
			Type: "issue.updated", Status: "todo", Title: child, Parent: parent, Rank: child, ActorSession: session}, admission.engine)
		tick(t, pool, admission, "after-agent-todo")
		assertNotStarted(t, pool, child)
		if line, _ := passJudgement(t, pool, admission); slices.Contains(line, child) {
			t.Errorf("the waiting line = %v, want no %s: %s, a phase worker's session, wrote the todo", line, child, session)
		}
		if stored := maybeIssue(t, pool, child); stored != nil && record.InLegionsWork(*stored) {
			t.Errorf("%s after the agent's todo is recorded in Legion's work: %s/%s generation %d", child, stored.Phase, stored.Status, stored.Generation)
		}
	})
	t.Run("an unrecorded child, a person's todo", func(t *testing.T) {
		const child = "LEGION-643"
		pool := migratedPool(t)
		admission := quiet(t, 3)
		seedRunning(t, pool, parent, "")
		seedSessionClaims(t, pool, agent)
		applyMirrored(t, pool, admission, "person-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 1,
			Type: "issue.updated", Status: "todo", Title: child, Parent: parent, Rank: child}, admission.engine)
		tick(t, pool, admission, "after-person-todo")
		if stored := maybeIssue(t, pool, child); stored == nil || !record.InLegionsWork(*stored) {
			t.Errorf("%s after a person's todo = %+v, want it recorded in Legion's work", child, stored)
		}
	})
	t.Run("a recorded child stopped at backlog, the parent's implementer's todo", func(t *testing.T) {
		const child = "LEGION-642"
		pool := migratedPool(t)
		admission := quiet(t, 3)
		seedRunning(t, pool, parent, "")
		stopped := chainRecord(child, parent, phase.Implementing, "backlog")
		stopped.Tree, stopped.LastDispatchSeq = parent, 1
		putIssue(t, pool, stopped)
		seedSessionClaims(t, pool, agent)
		apply(t, pool, admission, "agent-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2,
			Type: "issue.updated", Status: "todo", Title: child, Parent: parent, Rank: child, ActorSession: session}, admission.engine)
		if got := issue(t, pool, child); record.InLegionsWork(got) || got.Status != "backlog" {
			t.Errorf("%s after the agent's todo = %s/%s, want it set back at backlog", child, got.Phase, got.Status)
		}
	})
}
