package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
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

// Each real start raises its own agent's entry. Starting b after a must leave a's entry valid:
// completing a with the entry its start handed out moves only a, then b completes at its entry.
// A deployment step has no handoff record, so a current pull-request head cannot mask a lost entry.
func TestASecondTasksStartKeepsTheFirstAgentsPhaseEntry(t *testing.T) {
	pool := migratedPool(t)
	engine := deploymentEngine()
	root := stepRoot(t, pool, phase.Step)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "checker", "reviewer"}, agents: []claim.Agent{"implementer", "checker", "reviewer"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "b", steps: []string{"implementer", "checker", "reviewer"}, agents: []claim.Agent{"implementer-2", "checker-2", "reviewer-2"}, state: record.TaskRunning, place: atStep(2)})
	if _, err := pool.Exec(t.Context(), "update plan_steps set type = 'testing' where issue = $1 and role = 'checker'", root.Key); err != nil {
		t.Fatalf("store the checker type: %v", err)
	}
	seedTaskPRs(t, pool, root.Key, "a", "b")
	entries := map[string]int64{}
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		for _, task := range []string{"a", "b"} {
			scope, err := engine.ScopeOf(ctx, tx, root, task)
			if err != nil {
				return err
			}
			entry, _, err := engine.enter(ctx, tx, &scope, "checker", "check "+task, false, false)
			if err != nil {
				return err
			}
			entries[task] = entry
		}
		return nil
	})
	if entries["a"] == 0 || entries["b"] == 0 {
		t.Fatalf("the real starts raised no entry: %v", entries)
	}
	apply := applyFacts(t, pool, engine)
	first := intake.HandoffComplete{Generation: root.Generation, Issue: root.Key, Role: "checker", Claim: agentClaim(t, root.Key, "checker"),
		Task: "a", Summary: "checked a", Verdict: "pass", Commit: "head-a", Entry: entries["a"]}
	firstResult := apply("a-at-its-entry", first)
	if firstResult.Refusal != nil {
		t.Fatalf("a's completion of entry %d after b started entry %d was refused %s: %+v", entries["a"], entries["b"], firstResult.Refusal.Code, *firstResult.Refusal)
	}
	mustSpend(t, "a's completion after b's start", firstResult)
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(3))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(2))
	second := intake.HandoffComplete{Generation: root.Generation, Issue: root.Key, Role: "checker", Claim: agentClaim(t, root.Key, "checker-2"),
		Task: "b", Summary: "checked b", Verdict: "pass", Commit: "head-b", Entry: entries["b"]}
	mustSpend(t, "b's completion at its own entry", apply("b-at-its-entry", second))
	assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskRunning, atStep(3))
}

func TestSilenceReadsTheSelectedAgentOfAConcurrentRole(t *testing.T) {
	pool, engine := migratedPool(t), deploymentEngine()
	root := stepRoot(t, pool, phase.Step)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "checker", "reviewer"}, agents: []claim.Agent{"implementer", "checker", "reviewer"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "b", steps: []string{"implementer", "checker", "reviewer"}, agents: []claim.Agent{"implementer-2", "checker-2", "reviewer-2"}, state: record.TaskRunning, place: atStep(2)})
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		for _, task := range []string{"a", "b"} {
			scope, err := engine.ScopeOf(ctx, tx, root, task)
			if err != nil {
				return err
			}
			if _, _, err := engine.enter(ctx, tx, &scope, "checker", "check "+task, false, false); err != nil {
				return err
			}
		}
		for _, task := range []string{"a", "b"} {
			agent := claim.Agent("checker")
			if task == "b" {
				agent = "checker-2"
			}
			worker := agentClaim(t, root.Key, agent)
			read, err := engine.phaseWorkerRecord(ctx, tx, root.Key, root.Generation, "checker", worker)
			if err != nil {
				return err
			}
			if read == nil || read.scope.Task() != task || read.row.Claim != worker || read.row.Entry == 0 {
				t.Fatalf("silence of %s read %+v; want its own raised entry on task %s", worker, read, task)
			}
		}
		if read, err := engine.phaseWorkerRecord(ctx, tx, root.Key, root.Generation, "checker", ""); err != nil || read != nil {
			t.Fatalf("a role alone chose one of two agents: read %+v, error %v", read, err)
		}
		return nil
	})
}
