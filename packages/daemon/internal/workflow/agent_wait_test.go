package workflow

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// tickingEngine is returnEngine logging into a buffer the test reads, on a clock one second later
// at each read, so each wait a step begins begins later than the one before it, as in production.
func tickingEngine() (*Engine, *bytes.Buffer) {
	engine := returnEngine()
	logged := &bytes.Buffer{}
	engine.log = slog.New(slog.NewTextHandler(logged, nil))
	tick := stepClock
	engine.cfg.Clock = func() time.Time {
		tick = tick.Add(time.Second)
		return tick
	}
	return engine, logged
}

// assertWaits fails unless task of tasks is running at place and its step waits for wait.
func assertWaits(t *testing.T, tasks map[string]record.PlanTask, task string, place record.TaskPlace, wait record.StepWait) {
	t.Helper()
	assertTask(t, tasks, task, record.TaskRunning, place)
	if got := tasks[task].StepWait; got != wait {
		t.Fatalf("the step of task %s waits for %q, want %q", task, got, wait)
	}
}

// newestStart is the newest start of role key's outbox holds, which fails the test where it holds
// none.
func newestStart(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) record.SuperviseRequest {
	t.Helper()
	starts := roleStartsOf(t, pool, key, role)
	if len(starts) == 0 {
		t.Fatalf("the outbox of %s holds no start of the %s", key, role)
	}
	return starts[len(starts)-1]
}

// assertStartOf fails unless start is the start of task's step, under the claim of agent on key.
func assertStartOf(t *testing.T, start record.SuperviseRequest, key, task string, agent claim.Agent) {
	t.Helper()
	if want := agentClaim(t, key, agent); start.Claim != want || !strings.Contains(start.Task, " Task: "+task+".") {
		t.Fatalf("the start names the claim %s and the task %q, want the claim %s of agent %s and the task of task %s", start.Claim, start.Task, want, agent, task)
	}
}

// assertNothingStarted fails unless nothing of task's step at place has started: no count of its
// starts and no record of its step.
func assertNothingStarted(t *testing.T, pool *pgxpool.Pool, key, task string, place record.TaskPlace) {
	t.Helper()
	starts, err := startRows(t.Context(), pool)
	if err != nil {
		t.Fatalf("read the counts of starts: %v", err)
	}
	if count, ok := starts[key+" "+task+" "+place.Kind+" "+strconv.Itoa(place.Position)]; ok {
		t.Fatalf("the step of task %s at %+v counts starts %q, want none: its start waits", task, place, count)
	}
	for _, write := range stepWrites(t, pool, key) {
		if write.Task == task && write.Step == place.Position && write.Kind == place.Kind {
			t.Fatalf("the step of task %s at %+v has the record %+v, want none: its start waits", task, place, write)
		}
	}
}

// An agent does one step at a time: tasks x and y each have one step of the implementer keyed
// k, and z, a reviewer's, needs both. At the first step x and y run, and the one start queued is
// of x's step for agent k; y's step waits for its agent, with no start counted and no record of
// it, and the log says that it waits, for agent k, which runs step 1 of x. x's step passes: in
// that transaction y's step starts, for agent k, under k's claim. y passes: z starts.
//
// The fault it catches is one agent on two steps at one time (its control: the key taken off y's
// step, so it is an implementer's of an agent of its own, which starts at the first step beside
// x's, and the assertion of one start of k fails).
func TestAStepWaitsForItsAgentInAnotherTask(t *testing.T) {
	pool := migratedPool(t)
	engine, logged := tickingEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "x", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskWaiting},
		planTask{id: "y", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskWaiting},
		planTask{id: "z", steps: []string{"reviewer"}, agents: []claim.Agent{"reviewer"}, needs: []string{"x", "y"}, state: record.TaskWaiting})

	firstStepOf(t, pool, engine, root.Key)

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "x", atStep(1), record.StepWaitNone)
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitAgent)
	assertTask(t, tasks, "z", record.TaskWaiting, record.TaskPlace{})
	starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
	if len(starts) != 1 {
		t.Fatalf("the implementer's starts of %s = %+v, want one: agent k does x's step, and y's waits", root.Key, starts)
	}
	assertStartOf(t, starts[0], root.Key, "x", "k")
	assertNothingStarted(t, pool, root.Key, "y", atStep(1))
	if line := logged.String(); !strings.Contains(line, `msg="workflow: a step waits for its agent"`) || !strings.Contains(line, "task=y") ||
		!strings.Contains(line, "agent=k") || !strings.Contains(line, `runs="x 1"`) {
		t.Fatalf("the log of the first step = %q, want the line of a step that waits for its agent: task y, agent k, which runs x 1", line)
	}

	taskMoves(t, pool, engine, root.Key, "x", TriggerImplementationReady, "")

	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "x", record.TaskDone, atStep(1))
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitNone)
	assertStartOf(t, newestStart(t, pool, root.Key, claim.RoleImplementer), root.Key, "y", "k")

	taskMoves(t, pool, engine, root.Key, "y", TriggerImplementationReady, "")

	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "y", record.TaskDone, atStep(1))
	assertWaits(t, tasks, "z", atStep(1), record.StepWaitNone)
	assertStartOf(t, newestStart(t, pool, root.Key, claim.RoleReviewer), root.Key, "z", "reviewer")
}

// The move of a task to its next step obeys the same rule as its first start: task x is
// `implementer tester:k`, y is `tester:k`, and z needs both. At the first step y's tester starts
// at once, for agent k. x's implementer passes: x stands at its tester's step, which waits for k,
// with nothing of it started, and the one start of the tester stands queued, y's. y's step ends:
// x's tester's step starts, for agent k.
//
// The owner's move back of a task to a step of k while k works another task waits the same way:
// task m runs its step of k, and w, `implementer:k reviewer`, stands at its review; the owner
// moves w back to its first step, which waits for k with nothing of it started. m passes, and w's
// step starts for k.
//
// The fault it catches is a rule that only the first start of a task obeys (its control: the check
// in startReady alone, the revision before, so the transition queues a second start of k).
func TestATransitionToTheNextStepOfABusyAgentWaits(t *testing.T) {
	t.Run("the next step", func(t *testing.T) {
		pool := migratedPool(t)
		engine, _ := tickingEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "x", steps: []string{"implementer", "tester:k"}, agents: []claim.Agent{"implementer", "k"}, state: record.TaskWaiting},
			planTask{id: "y", steps: []string{"tester:k"}, agents: []claim.Agent{"k"}, state: record.TaskWaiting},
			planTask{id: "z", steps: []string{"reviewer"}, agents: []claim.Agent{"reviewer"}, needs: []string{"x", "y"}, state: record.TaskWaiting})

		firstStepOf(t, pool, engine, root.Key)
		tasks := tasksOf(t, pool, engine, root.Key)
		assertWaits(t, tasks, "x", atStep(1), record.StepWaitNone)
		assertWaits(t, tasks, "y", atStep(1), record.StepWaitNone)

		taskMoves(t, pool, engine, root.Key, "x", TriggerImplementationReady, "")

		tasks = tasksOf(t, pool, engine, root.Key)
		assertWaits(t, tasks, "x", atStep(2), record.StepWaitAgent)
		assertNothingStarted(t, pool, root.Key, "x", atStep(2))
		starts := roleStartsOf(t, pool, root.Key, claim.RoleTester)
		if len(starts) != 1 {
			t.Fatalf("the tester's starts of %s = %+v, want one: agent k does y's step, and x's waits", root.Key, starts)
		}
		assertStartOf(t, starts[0], root.Key, "y", "k")

		taskMoves(t, pool, engine, root.Key, "y", TriggerTesterPassed, "")

		tasks = tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "y", record.TaskDone, atStep(1))
		assertWaits(t, tasks, "x", atStep(2), record.StepWaitNone)
		assertStartOf(t, newestStart(t, pool, root.Key, claim.RoleTester), root.Key, "x", "k")
	})

	t.Run("the owner's move back", func(t *testing.T) {
		pool := migratedPool(t)
		engine, _ := tickingEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "m", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "w", steps: []string{"implementer:k", "reviewer"}, agents: []claim.Agent{"k", "reviewer"}, state: record.TaskRunning, place: atStep(2)},
			planTask{id: "z", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"m", "w"}, state: record.TaskWaiting})

		onTask(t, pool, engine, root.Key, "w", func(ctx context.Context, tx pgx.Tx, view record.Issue, w Walk) error {
			pr, err := engine.store.PullRequest(ctx, tx, root.Key)
			if err != nil {
				return err
			}
			row, err := engine.phaseRow(ctx, tx, root.Key, claim.RoleArchitect)
			if err != nil {
				return err
			}
			return engine.transition(ctx, tx, view, TriggerBackward, atStep(1), row, pr, "the owner sends the work back")
		})

		tasks := tasksOf(t, pool, engine, root.Key)
		assertWaits(t, tasks, "w", atStep(1), record.StepWaitAgent)
		assertNothingStarted(t, pool, root.Key, "w", atStep(1))
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(starts) != 0 {
			t.Fatalf("the implementer's starts of %s after the move back = %+v, want none: agent k does m's step", root.Key, starts)
		}

		taskMoves(t, pool, engine, root.Key, "m", TriggerImplementationReady, "")

		tasks = tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "m", record.TaskDone, atStep(1))
		assertWaits(t, tasks, "w", atStep(1), record.StepWaitNone)
		assertStartOf(t, newestStart(t, pool, root.Key, claim.RoleImplementer), root.Key, "w", "k")
	})
}

// An agent that a move frees goes to the step that waits for it in the same transaction, once the
// move's own start is made: task y, `implementer:k`, waits for k, which does x's first step.
// (1) x is `implementer:k reviewer`: x's implementer passes, x stands at its review, and y's step
// starts for k in that transaction. (2) x is `implementer:k reviewer:k`: x's implementer passes, y's
// step starts for k, and x's review waits for k, behind y, whose wait began first.
//
// The fault it catches is a freed agent that goes to nobody, or to a step that waited less long
// (its control: the hand-on made where the run of the step that ends is ended, before the move,
// the revision before, where the store still gives k the step that ends: y keeps its wait in both
// cases).
func TestAnAgentFreedByAMoveToANextStepGoesToItsWaiter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		steps  []string
		agents []claim.Agent
		review record.StepWait
	}{
		{"its next step is another agent's", []string{"implementer:k", "reviewer"}, []claim.Agent{"k", "reviewer"}, record.StepWaitNone},
		{"its next step is the same agent's", []string{"implementer:k", "reviewer:k"}, []claim.Agent{"k", "k"}, record.StepWaitAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine, _ := tickingEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedTasks(t, pool, root.Key, root.Generation,
				planTask{id: "x", steps: tc.steps, agents: tc.agents, state: record.TaskWaiting},
				planTask{id: "y", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskWaiting},
				planTask{id: "z", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"x", "y"}, state: record.TaskWaiting})
			firstStepOf(t, pool, engine, root.Key)
			assertWaits(t, tasksOf(t, pool, engine, root.Key), "y", atStep(1), record.StepWaitAgent)

			taskMoves(t, pool, engine, root.Key, "x", TriggerImplementationReady, "")

			tasks := tasksOf(t, pool, engine, root.Key)
			assertWaits(t, tasks, "y", atStep(1), record.StepWaitNone)
			assertStartOf(t, newestStart(t, pool, root.Key, claim.RoleImplementer), root.Key, "y", "k")
			assertWaits(t, tasks, "x", atStep(2), tc.review)
			if tc.review == record.StepWaitAgent {
				assertNothingStarted(t, pool, root.Key, "x", atStep(2))
			}
		})
	}
}

// A freed agent takes the step that waited longest for it, not the step a move makes ready in the
// same act: the steps of tasks p and q wait for k, which does s's step, since t1 and t2, and r,
// which needs s, has a first step of k. s passes: r is ready and runs in that act, and its step
// waits for k behind p and q; p's step starts for k; q's and r's wait.
//
// The fault it catches is a step that takes an agent ahead of steps that waited for it longer
// (its control: start's check without the steps that wait, AgentWaiter, so r's step takes k and
// p and q keep waiting).
func TestAFreedAgentTakesTheStepThatWaitedLongest(t *testing.T) {
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "p", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "q", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "r", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, needs: []string{"s"}, state: record.TaskWaiting},
		planTask{id: "s", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "z", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"p", "q", "r"}, state: record.TaskWaiting})
	t1, t2 := stepClock.Add(-2*time.Hour), stepClock.Add(-time.Hour)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		if err := record.NewStore().SetStepWait(t.Context(), tx, root.Key, root.Generation, "p", record.StepWaitAgent, &t1); err != nil {
			return err
		}
		return record.NewStore().SetStepWait(t.Context(), tx, root.Key, root.Generation, "q", record.StepWaitAgent, &t2)
	})

	taskMoves(t, pool, engine, root.Key, "s", TriggerImplementationReady, "")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "s", record.TaskDone, atStep(1))
	assertWaits(t, tasks, "p", atStep(1), record.StepWaitNone)
	assertWaits(t, tasks, "q", atStep(1), record.StepWaitAgent)
	assertWaits(t, tasks, "r", atStep(1), record.StepWaitAgent)
	assertStartOf(t, newestStart(t, pool, root.Key, claim.RoleImplementer), root.Key, "p", "k")
	assertNothingStarted(t, pool, root.Key, "r", atStep(1))
}

// aWaitsWhileMWorks is the issue TestAStepThatWaitsIsNoRunningStep reads: task m does its step of
// agent k, x, a tester's, has passed, and a, `implementer:k`, which needs x and comes before m in
// task id order, waits for k. z, a reviewer's, needs a and m.
func aWaitsWhileMWorks(t *testing.T) (*pgxpool.Pool, *Engine, record.Issue) {
	t.Helper()
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, needs: []string{"x"}, state: record.TaskWaiting},
		planTask{id: "m", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskWaiting},
		planTask{id: "x", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, state: record.TaskWaiting},
		planTask{id: "z", steps: []string{"reviewer"}, agents: []claim.Agent{"reviewer"}, needs: []string{"a", "m"}, state: record.TaskWaiting})
	firstStepOf(t, pool, engine, root.Key)
	taskMoves(t, pool, engine, root.Key, "x", TriggerTesterPassed, "")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "a", atStep(1), record.StepWaitAgent)
	assertWaits(t, tasks, "m", atStep(1), record.StepWaitNone)
	return pool, engine, root
}

// A step that waits is no running step, so no reader that gives work reaches it: task m does its
// step of agent k, x, a tester's, passes, and a, `implementer:k`, which needs x and comes before m
// in task id order, waits for k. The issue's running steps hold m's and not a's; the boot's resume
// reaches the claim of k once, at m's phase; and a failed claim of k holds m and leaves a waiting.
// A completion of k that names no task, which the engine reads as the task of the step k runs, is
// m's: m is done, its pull request recorded open, and a is not.
//
// The fault it catches is a reader that gives a step that never started the work of its agent (its
// control: the issue's read of its running steps without the filter of the wait, so a's step is
// read as k's: the failed claim holds a, whose step never started, and k's completion makes a
// done and leaves m at work).
func TestAStepThatWaitsIsNoRunningStep(t *testing.T) {
	t.Run("its reads, the boot's resume and a failed claim of k", func(t *testing.T) {
		pool, engine, root := aWaitsWhileMWorks(t)
		issue := issueOf(t, pool, engine, root.Key)
		if step, ok := issue.StepOfAgent("k"); !ok || step.Task != "m" || len(issue.Running) != 1 {
			t.Fatalf("the running steps of %s = %+v, want m's alone, of agent k: a's step waits", root.Key, issue.Running)
		}

		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			t.Fatal(err)
		}
		k := agentClaim(t, root.Key, "k")
		if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, $2, $3, $3, 'implementer', 1, 'ses', '/sessions/ses.jsonl', 'suspended', 0, 0, 0, 0, 'daemon')`,
			string(k), project, root.Key); err != nil {
			t.Fatalf("seed the suspended claim of k: %v", err)
		}
		var owners phaseOwners
		if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
			var err error
			owners, err = engine.suspendedPhaseOwners(t.Context(), tx)
			return err
		}); err != nil {
			t.Fatalf("read the boot's owners: %v", err)
		}
		if len(owners.suspended) != 1 || owners.suspended[0].token != k || owners.suspended[0].issue.Phase != phase.Implementing {
			t.Fatalf("the boot's owners of %s = %+v, want the claim of k once, at m's implementing", root.Key, owners.suspended)
		}

		failed := failedClaimOf(t, root.Key, claim.RoleImplementer, "k")
		mustSpend(t, "the failure of k", applyFacts(t, pool, engine)("k-failed", failed))
		tasks := tasksOf(t, pool, engine, root.Key)
		assertHeld(t, tasks, "m", atStep(1), "")
		assertWaits(t, tasks, "a", atStep(1), record.StepWaitAgent)
	})

	t.Run("k's completion moves m", func(t *testing.T) {
		pool, engine, root := aWaitsWhileMWorks(t)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Task: "m", Repo: "sjawhar/legion", Number: 44,
			Branch: "legion/" + root.Key + ".m", HeadSHA: "head-m", Failing: []string{}})
		completed := intake.HandoffComplete{Generation: root.Generation, Issue: root.Key, Role: claim.RoleImplementer,
			Claim: agentClaim(t, root.Key, "k"), Summary: "done", Commit: "head-m", HandoffRecord: "k-m", HandoffVersion: 1}

		mustSpend(t, "k's completion", applyFacts(t, pool, engine)("k-implemented", completed))

		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "m", record.TaskDone, atStep(1))
		assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	})
}
