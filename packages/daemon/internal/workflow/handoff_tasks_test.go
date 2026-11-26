package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedTaskPRs gives each of tasks of key its own open pull request, on its branch
// legion/<key>.<task>, which the task's row then holds (seedTaskBranch), so the pull request is the
// task's (record.StoredPlan.TaskOfBranch), numbered 44 and up in the order given, at the head
// head-<task>.
func seedTaskPRs(t *testing.T, pool *pgxpool.Pool, key string, tasks ...string) {
	t.Helper()
	for i, task := range tasks {
		branch := "legion/" + key + "." + task
		seedTaskBranch(t, pool, key, task, branch)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: 44 + i,
			Branch: branch, HeadSHA: "head-" + task, Failing: []string{}})
	}
}

// completedBy is the completion of the step agent runs on key, of role, naming task as the route
// reads it from the claim's agent (intake.HandoffComplete.Task): with a handoff record of its own,
// the commit head-<task> and verdict, and at no phase entry, which no entry fence judges.
func completedBy(t *testing.T, key string, role claim.Role, agent claim.Agent, task, verdict string) intake.HandoffComplete {
	t.Helper()
	return intake.HandoffComplete{Generation: 1, Issue: key, Role: role, Claim: agentClaim(t, key, agent), Task: task,
		Summary: "done", Verdict: verdict, Commit: "head-" + task, HandoffRecord: string(agent) + "-" + task, HandoffVersion: 1}
}

// A completion moves the task of its claim's agent's step and no other, whichever task comes first
// in the issue's order. Two tasks of one role: a and b each stand at their implementer's step, of
// the agents implementer and implementer-2, each with its own pull request; implementer-2's
// completion moves b to its review and leaves a implementing, and implementer's then moves a. Two
// tasks of two roles: a implements and b tests, of the agents implementer and tester; the tester's
// pass makes b done (it does not merge), and the implementer's completion then moves a to its
// review. A completion of an agent that runs no step of the issue, implementer-3, is refused
// HANDOFF_NOT_CURRENT_PHASE and moves nothing.
//
// Each completion is at no phase entry: two tasks whose steps share a role share that role's
// phases row, so in production the second start's entry would refuse the first agent's completion
// (HANDOFF_STALE_ENTRY) until the row is keyed by the step.
//
// The fault it catches is a completion that moves the other task (its control: the read tree's
// task of a completion, the issue's first running task, so implementer-2's completion moves a).
func TestTwoCompletionsOfOneRoleAreOfTheirOwnTasks(t *testing.T) {
	t.Run("two tasks of one role", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "b", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer-2", "reviewer-2"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "c", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
		seedTaskPRs(t, pool, root.Key, "a", "b")
		apply := applyFacts(t, pool, engine)

		mustSpend(t, "implementer-2's completion", apply("b-implemented", completedBy(t, root.Key, claim.RoleImplementer, "implementer-2", "b", "")))
		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "b", record.TaskRunning, atStep(2))
		assertTask(t, tasks, "a", record.TaskRunning, atStep(1))

		mustSpend(t, "implementer's completion", apply("a-implemented", completedBy(t, root.Key, claim.RoleImplementer, "implementer", "a", "")))
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))

		mustRefuse(t, "a completion of an agent that runs no step", apply("nobody", completedBy(t, root.Key, claim.RoleImplementer, "implementer-3", "", "")),
			"HANDOFF_NOT_CURRENT_PHASE", "the implementer does not run phase reviewing of LEGION-208; this completion changed nothing")
	})

	t.Run("two tasks of two roles", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "b", steps: []string{"implementer", "tester"}, agents: []claim.Agent{"implementer-2", "tester"}, state: record.TaskRunning, place: atStep(2)},
			planTask{id: "c", steps: []string{"tester"}, agents: []claim.Agent{"tester-2"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
		seedTaskPRs(t, pool, root.Key, "a", "b")
		apply := applyFacts(t, pool, engine)

		mustSpend(t, "the tester's pass", apply("b-tested", completedBy(t, root.Key, claim.RoleTester, "tester", "b", "pass")))
		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "b", record.TaskDone, atStep(2))
		assertTask(t, tasks, "a", record.TaskRunning, atStep(1))

		mustSpend(t, "implementer's completion", apply("a-implemented", completedBy(t, root.Key, claim.RoleImplementer, "implementer", "a", "")))
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))
	})
}
