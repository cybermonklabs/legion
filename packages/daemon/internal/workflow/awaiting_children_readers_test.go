package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A CI settlement on the pull request of a parent waiting in awaiting_children wakes no role of
// it and tells none, its tree's architect included (dispatch://INFRA-3320 R11): no role works the
// parent while it waits.
// The rig's implementer and architect are both released, as the idle rule leaves a waiting
// parent's quiet pod, so the settlement that wakes a released implementer at work
// (TestACISettlementWakesTheReleasedPhaseWorker) is the control: here it wakes nobody, queues no
// start, records no resume, and the settlement's own role messages (tellChecks) go to nobody.
func TestASettlementOnAWaitingParentWakesNoRoleAndTellsNone(t *testing.T) {
	rig := newWakeRig(t)
	waited := wakeAt.Add(-time.Hour)
	held := record.ChildrenWait{Start: phase.Implementing, Task: "Continue root. Issue: LEGION-208. Phase: implementing.", Since: waited,
		Reason: "the admission pass stopped the implementer's implementing phase, while these Dispatch children of LEGION-208 were open: LEGION-209 (todo)"}
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.AwaitingChildren, Generation: wakeIssueGeneration,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1, ChildrenWait: &held, ChildrenWaitedAt: &waited})

	rig.apply("settled", settlement("head1", "build"))

	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows after a settlement on a waiting parent = %+v, want none", starts)
	}
	if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
		t.Fatalf("pod resumes after a settlement on a waiting parent = %v, want none", resumes)
	}
	if told := roleMessageTokens(t, rig.pool); len(told) != 0 {
		t.Fatalf("role messages after a settlement on a waiting parent = %v, want none: no role of it, and not its architect, is told", told)
	}
	if got := storedIssue(t, rig.pool, "LEGION-208"); got.Phase != phase.AwaitingChildren || !sameChildrenWait(got.ChildrenWait, &held) {
		t.Fatalf("the parent after the settlement = phase %s, held start %+v; want it waiting as it was", got.Phase, got.ChildrenWait)
	}
}

// A parent waiting in awaiting_children that leaves Legion's work keeps its phase and the start it
// held (R20 of dispatch://INFRA-3320: the leave keeps the phase and the held start, so the return
// resumes where the stop left it), and nothing lingers: a root a person moves to the backlog and a
// child a person closes both stand in awaiting_children with the start they held, and the moment
// of the wait stays as it was.
func TestAWaitingParentThatLeavesTheWorkflowKeepsItsHeldStart(t *testing.T) {
	t.Parallel()
	t.Run("a root a person moves to the backlog", func(t *testing.T) {
		pool := migratedPool(t)
		waiting := cwSeedWaiting(t, pool, cwParent, phase.Implementing)

		applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))("moved-to-backlog",
			intake.DispatchIssue{Key: cwParent, Seq: 2, Type: "issue.updated", Status: "backlog", Title: "parent", Rank: "U"})

		got := storedIssue(t, pool, cwParent)
		if got.Phase != phase.AwaitingChildren || !sameChildrenWait(got.ChildrenWait, waiting.ChildrenWait) || got.Status != "backlog" {
			t.Fatalf("the root on the backlog = phase %s, status %s, held start %+v; want awaiting_children at backlog holding %+v",
				got.Phase, got.Status, got.ChildrenWait, waiting.ChildrenWait)
		}
		if got.ChildrenWaitedAt == nil || !got.ChildrenWaitedAt.Equal(*waiting.ChildrenWaitedAt) {
			t.Fatalf("the root on the backlog = waited at %v; want it kept from %v", got.ChildrenWaitedAt, waiting.ChildrenWaitedAt)
		}
	})

	t.Run("a child a person closes", func(t *testing.T) {
		pool := migratedPool(t)
		const root = "LEGION-207"
		cwSeedParent(t, pool, root, phase.Implementing, 41)
		waited := cwAt.Add(-time.Hour)
		parent := root
		held := record.ChildrenWait{Start: phase.Testing, Task: "Continue parent. Issue: LEGION-208. Phase: testing.",
			Reason: "the implementer completed implementing, while these Dispatch children of LEGION-208 were open: LEGION-209 (todo)", Since: waited}
		seedIssue(t, pool, record.Issue{Key: cwParent, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root,
			Parent: &parent, Project: "LEGION", Title: "parent", Phase: phase.AwaitingChildren, Generation: 1, Status: "in_progress", Rank: "V",
			LastDispatchSeq: 1, ChildrenWait: &held, ChildrenWaitedAt: &waited})

		applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))("closed",
			intake.DispatchIssue{Key: cwParent, Seq: 2, Type: "issue.closed", Status: "done", Title: "parent", Parent: root, Rank: "V"})

		got := storedIssue(t, pool, cwParent)
		if got.Phase != phase.AwaitingChildren || !sameChildrenWait(got.ChildrenWait, &held) || got.Status != "done" {
			t.Fatalf("the closed child = phase %s, status %s, held start %+v; want awaiting_children at done holding %+v", got.Phase, got.Status, got.ChildrenWait, held)
		}
		if got.ChildrenWaitedAt == nil || !got.ChildrenWaitedAt.Equal(waited) {
			t.Fatalf("the closed child = waited at %v; want it kept from %v", got.ChildrenWaitedAt, waited)
		}
	})
}
