package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// agentClaim is the claim token of the agent named name on key (claim.AgentToken), the claim a
// start of that agent's step names.
func agentClaim(t *testing.T, key string, name claim.Agent) claim.Token {
	t.Helper()
	token, err := claim.AgentToken("legion", key, name)
	if err != nil {
		t.Fatalf("the claim token of agent %s of %s: %v", name, key, err)
	}
	return token
}

// firstStep opens the gate of key, which stands admitted with its slot, as the waiting line starts
// its first step (StartFirstPhase).
func firstStepOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	onStored(t, pool, engine, key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		return engine.StartFirstPhase(ctx, tx, issue, nil)
	})
}

// startClaims is the claim each start of role in key's outbox names, oldest first.
func startClaims(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) []claim.Token {
	t.Helper()
	var claims []claim.Token
	for _, start := range roleStartsOf(t, pool, key, role) {
		claims = append(claims, start.Claim)
	}
	return claims
}

// Each task starts once the tasks it needs are done, in the transaction that does the last of
// them, and the tasks that need nothing start with the first step: the plan of tasks a to e of one
// step each, a, b, c and d an implementer's and e a tester's, where c needs b, d needs a and c, and
// e needs d, so e is the task that merges. At the first step a and b run and c, d and e wait, and
// the starts of a's agent and b's agent are queued, each under its own claim, the agent its step
// names. b passes: c runs and d waits. a passes: d still waits, for c. c passes: d runs. d passes:
// e runs. e passes: its retro, since e merges.
//
// The fault it catches is a task that starts before its needs are done (its control: c's needs
// taken out of the plan, so c starts at the first step beside a and b, and the assertion that c
// waits fails).
func TestEachTaskStartsWhenItsNeedsAreDone(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	noAct := record.TaskPlace{}
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskWaiting},
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-2"}, state: record.TaskWaiting},
		planTask{id: "c", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-3"}, needs: []string{"b"}, state: record.TaskWaiting},
		planTask{id: "d", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-4"}, needs: []string{"a", "c"}, state: record.TaskWaiting},
		planTask{id: "e", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"d"}, state: record.TaskWaiting})

	firstStepOf(t, pool, engine, root.Key)
	joinEnds(t, pool, engine, root.Key, "b")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "c", record.TaskWaiting, noAct)
	assertTask(t, tasks, "d", record.TaskWaiting, noAct)
	assertTask(t, tasks, "e", record.TaskWaiting, noAct)
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil {
		t.Fatalf("%s at its first step stands in %s (hold %+v), want implementing, where a and b run", root.Key, issue.Phase, issue.Hold)
	}
	want := []claim.Token{agentClaim(t, root.Key, "implementer"), agentClaim(t, root.Key, "implementer-2")}
	if got := startClaims(t, pool, root.Key, claim.RoleImplementer); !slices.Equal(got, want) {
		t.Fatalf("the implementer's starts of %s name the claims %v, want %v: one for the agent of a's step and one for b's", root.Key, got, want)
	}

	taskMoves(t, pool, engine, root.Key, "b", TriggerImplementationReady, "")
	joinEnds(t, pool, engine, root.Key, "c")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "b", record.TaskDone, atStep(1))
	assertTask(t, tasks, "c", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "d", record.TaskWaiting, noAct)
	if got := startClaims(t, pool, root.Key, claim.RoleImplementer); !slices.Contains(got, agentClaim(t, root.Key, "implementer-3")) {
		t.Fatalf("the implementer's starts of %s name the claims %v, want one of c's agent implementer-3", root.Key, got)
	}

	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskDone, atStep(1))
	assertTask(t, tasks, "d", record.TaskWaiting, noAct)

	taskMoves(t, pool, engine, root.Key, "c", TriggerImplementationReady, "")
	joinEnds(t, pool, engine, root.Key, "d")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "c", record.TaskDone, atStep(1))
	assertTask(t, tasks, "d", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "e", record.TaskWaiting, noAct)

	taskMoves(t, pool, engine, root.Key, "d", TriggerImplementationReady, "")
	joinEnds(t, pool, engine, root.Key, "e")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "d", record.TaskDone, atStep(1))
	assertTask(t, tasks, "e", record.TaskRunning, atStep(1))
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Testing {
		t.Fatalf("%s while e tests stands in %s, want testing", root.Key, issue.Phase)
	}

	taskMoves(t, pool, engine, root.Key, "e", TriggerTesterPassed, "")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "e", record.TaskRunning, record.TaskPlace{Kind: record.PlaceRetro})
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Retro || issue.Hold != nil {
		t.Fatalf("%s after e passed stands in %s (hold %+v), want retro: e is the task that merges", root.Key, issue.Phase, issue.Hold)
	}
}

// stopTasks stops each of tasks of the plan of key at generation for its owner, as the store
// writes a stop (record.Store.StopTask).
func stopTasks(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, tasks ...string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		for _, task := range tasks {
			if err := record.NewStore().StopTask(t.Context(), tx, key, generation, task, "task "+task+" of "+key+" was stopped by its owner"); err != nil {
				return err
			}
		}
		return nil
	})
}

// laterVersion is register_plan's fact for key at its generation: version of the document the
// plan seedTasks writes ("plan-artifact"), holding tasks, given by the architect of LEGION-208.
func laterVersion(t *testing.T, key string, generation uint64, version int, tasks ...plan.Task) intake.PlanGiven {
	t.Helper()
	return intake.PlanGiven{Issue: key, Generation: generation, GivenBy: claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect)),
		Plan: plan.Plan{ArtifactID: "plan-artifact", Version: version, Tasks: tasks}}
}

// block is the plan_task block of task id as a version gives it: "Do <id>." with text added, the
// steps of one implementer of key id, and needs.
func block(id, text string, needs ...string) plan.Task {
	return plan.Task{ID: id, Text: "Do " + id + "." + text, Needs: needs, Steps: []plan.Step{{Role: "implementer", Key: id}}}
}

// A stopped task starts again, when a later version changes it, only where each task it needs is
// done; else it waits for them, and starts when they are done (startReady). Task a runs, b and d
// ran and are stopped, d needing a, and c needs a, b and d, so c is the task that merges. A later
// version that changes b and d starts b again at its first step, under its agent's claim, its pass
// raised, and d, whose need a is not done, waits at no act with its pass raised and its stop's
// reason gone. The issue stands in implementing throughout: a runs.
//
// The fault it catches is a task that starts again before its needs are done (its control:
// restartTask starting the stopped task at its first step whatever its needs, which starts d).
func TestAStoppedTaskStartsAgainOnlyWhereItsNeedsAreDone(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, needs: []string{"a", "b", "d"}, state: record.TaskWaiting},
		planTask{id: "d", steps: []string{"implementer:d"}, agents: []claim.Agent{"impl-d"}, needs: []string{"a"}, state: record.TaskRunning, place: atStep(1)})
	stopTasks(t, pool, root.Key, root.Generation, "b", "d")

	given := laterVersion(t, root.Key, root.Generation, 2, block("a", ""), block("b", " Again."), block("c", "", "a", "b", "d"), block("d", " Again.", "a"))
	mustTake(t, "version 2", applyFacts(t, pool, engine)("version-2", given))
	joinEnds(t, pool, engine, root.Key, "b")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "c", record.TaskWaiting, record.TaskPlace{})
	assertTask(t, tasks, "d", record.TaskWaiting, record.TaskPlace{})
	if b, d := tasks["b"], tasks["d"]; b.Counts.Pass != 1 || d.Counts.Pass != 1 || d.StoppedReason != "" {
		t.Fatalf("task b's pass is %d and task d's %d with d stopped for %q, want each pass raised to 1 and d's stop's reason gone", b.Counts.Pass, d.Counts.Pass, d.StoppedReason)
	}
	claims := startClaims(t, pool, root.Key, claim.RoleImplementer)
	if !slices.Contains(claims, agentClaim(t, root.Key, "impl-b")) || slices.Contains(claims, agentClaim(t, root.Key, "impl-d")) {
		t.Fatalf("the implementer's starts of %s name the claims %v, want one of b's agent impl-b and none of d's", root.Key, claims)
	}
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil {
		t.Fatalf("%s after version 2 stands in %s (hold %+v), want implementing", root.Key, issue.Phase, issue.Hold)
	}
}
