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
// names. a, the first to start, works on the issue's branch, with its pull request #42, and each
// other implementer's task opens its own pull request on its own branch before its step passes.
// b passes: c runs and d waits. a passes: d still waits, for c. c passes: d runs. d passes: e runs.
// e passes: its retro, since e merges.
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

	seedTaskPR(t, pool, engine, root.Key, "b", 44)
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

	seedTaskPR(t, pool, engine, root.Key, "c", 45)
	taskMoves(t, pool, engine, root.Key, "c", TriggerImplementationReady, "")
	joinEnds(t, pool, engine, root.Key, "d")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "c", record.TaskDone, atStep(1))
	assertTask(t, tasks, "d", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "e", record.TaskWaiting, noAct)

	seedTaskPR(t, pool, engine, root.Key, "d", 46)
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
// startAtFirstStep starting the stopped task at its first step whatever its needs, which starts d).
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

// A task stopped before its first step went out starts again with its pass where it was, since no
// run of it ended: task a runs, b, which needs a, waits and is stopped there, at no act, and a
// later version that changes b ends its stop. b waits for a again, its stop's reason gone and its
// pass still 0 (startAtFirstStep raises the pass only where the task's place started).
//
// The fault it catches is a pass raised for a run that never was (its control: the pass raised by
// the task's state, for every task that was not waiting, which raises b's to 1).
func TestAStoppedTaskAtNoActKeepsItsPassWhenItStartsAgain(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, needs: []string{"a"}, state: record.TaskWaiting})
	stopTasks(t, pool, root.Key, root.Generation, "b")

	mustTake(t, "version 2", applyFacts(t, pool, engine)("version-2", laterVersion(t, root.Key, root.Generation, 2, block("a", ""), block("b", " Again.", "a"))))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "b", record.TaskWaiting, record.TaskPlace{})
	if b := tasks["b"]; b.Counts.Pass != 0 || b.StoppedReason != "" {
		t.Fatalf("task b after version 2 counts pass %d and is stopped for %q, want pass 0 and no stop's reason: no run of it ended", b.Counts.Pass, b.StoppedReason)
	}
}

// A person's todo on a root held for the stopped task of a plan of several writes the Dispatch
// status of the phase the root then stands in over the todo Dispatch shows, as it does for a plan of
// one: task a, which b needs, is stopped at its step, so no task runs and the root is held from
// implementing; the record keeps in_progress, that phase's status. The todo starts a again at its
// first step, and one status write puts in_progress over the todo.
//
// The fault it catches is a status judged against the record's status, which the hold kept, rather
// than against the status Dispatch shows (its control: startAtFirstStep comparing with Status,
// which writes nothing, so the board stays at todo).
func TestATodoOnARootHeldForAStoppedTaskOfSeveralWritesItsStatusOverTheTodo(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, needs: []string{"a"}, state: record.TaskWaiting})
	apply := applyFacts(t, pool, engine)
	if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop of a was refused: %+v", result.Refusal)
	}
	held := issueOf(t, pool, engine, root.Key)
	if held.Phase != phase.Held || held.Hold == nil || *held.Hold != (record.Hold{From: phase.Implementing, Reason: record.HoldTaskStopped}) || held.Status != "in_progress" {
		t.Fatalf("%s after the stop stands in %s (hold %+v) with status %q, want held from implementing for the stopped task, keeping in_progress", root.Key, held.Phase, held.Hold, held.Status)
	}
	statuses := len(statusWrites(t, pool, root.Key))

	if result := apply("todo", personsTodo(root.Key)); result.Refusal != nil {
		t.Fatalf("the todo was refused: %+v", result.Refusal)
	}

	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(1))
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil || issue.DispatchStatus != "todo" {
		t.Fatalf("%s after the todo stands in %s (hold %+v), Dispatch at %q, want implementing with no hold, Dispatch at todo", root.Key, issue.Phase, issue.Hold, issue.DispatchStatus)
	}
	if got := statusWrites(t, pool, root.Key)[statuses:]; len(got) != 1 || got[0] != record.OrdinaryStatusWrite("in_progress", "todo", "") {
		t.Fatalf("the todo queued the status writes %+v, want the one of in_progress over the todo Dispatch shows", got)
	}
}

// The gate's opening writes its rows in one order for every plan: the branch row, the status of
// the phase the first task's step puts the issue in, the plan node, the start of that step, and the
// notice of the gate's opening. A root whose Dispatch status is todo opens its gate on a plan of one
// task, and on a plan of two whose second task needs the first, so one task starts in each.
//
// The fault it catches is a plan of several that writes its node before the first task's status
// (its control: the node written before the ready tasks start, as the gate wrote it for a plan of
// several, which puts plan_record before dispatch_status there).
func TestTheGatesOpeningWritesItsRowsInOneOrderForEveryPlan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []planTask
	}{
		{"a plan of one task", []planTask{
			{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskWaiting}}},
		{"a plan of two tasks", []planTask{
			{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskWaiting},
			{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, needs: []string{"a"}, state: record.TaskWaiting}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			root.Status = "todo"
			seedIssue(t, pool, root)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedTasks(t, pool, root.Key, root.Generation, tc.tasks...)

			firstStepOf(t, pool, engine, root.Key)

			assertOutboxKinds(t, pool, []string{"issue_branch", "dispatch_status", "plan_record", "supervise", "notice"})
			assertWaits(t, tasksOf(t, pool, engine, root.Key), "a", atStep(1), record.StepWaitNone)
			if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Status != "in_progress" {
				t.Fatalf("%s after its gate opened stands in %s with status %q, want implementing with in_progress", root.Key, issue.Phase, issue.Status)
			}
		})
	}
}

// A task of several whose first step names a role the deployment no longer defines stops where it
// stands when its needs are done, as a move into such a step stops a task: task x's review passes,
// which leaves x done and b, `checker`, ready, so b stops naming step 1 and the checker, the
// checker is never started, and the owner is told once; a runs on, so the issue stands in
// implementing with no hold.
//
// The fault it catches is the start of a role nobody defines (its control: startReady without the
// check of the lost role, which starts b's checker).
func TestALostRoleAtTheFirstStepOfATaskOfSeveralStopsIt(t *testing.T) {
	pool := migratedPool(t)
	engine := lostChecker()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"checker"}, agents: []claim.Agent{"checker"}, needs: []string{"x"}, state: record.TaskWaiting},
		planTask{id: "c", steps: []string{"tester:c"}, agents: []claim.Agent{"test-c"}, needs: []string{"a", "b"}, state: record.TaskWaiting},
		planTask{id: "x", steps: []string{"reviewer:x"}, agents: []claim.Agent{"rev-x"}, state: record.TaskRunning, place: atStep(1)})
	seedTaskBranch(t, pool, root.Key, "x", "legion/"+root.Key+".x")

	taskMoves(t, pool, engine, root.Key, "x", TriggerReviewApproved, "")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "x", record.TaskDone, atStep(1))
	assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	const why = "step 1 names the role checker, which the deployment no longer defines"
	if b := tasks["b"]; b.State != record.TaskStopped || b.StoppedReason != why {
		t.Fatalf("task b = %s stopped for %q, want stopped for %q", b.State, b.StoppedReason, why)
	}
	if starts := roleStartsOf(t, pool, root.Key, "checker"); len(starts) != 0 {
		t.Fatalf("the checker's starts of %s = %+v, want none: the deployment no longer defines it", root.Key, starts)
	}
	if stopped := noticesOf(t, pool, root.Key, "task-stopped"); len(stopped) != 1 || stopped[0].Role != "checker" || stopped[0].Reason != "task b of LEGION-208 was stopped: "+why {
		t.Fatalf("the task-stopped notices of %s = %+v, want exactly one, the checker's, with the reason", root.Key, stopped)
	}
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil {
		t.Fatalf("%s after b stopped stands in %s (hold %+v), want implementing with no hold: a runs", root.Key, issue.Phase, issue.Hold)
	}
}

// planReads is a store that counts its reads of a plan (record.Store.Plan).
type planReads struct {
	record.Store
	reads int
}

func (s *planReads) Plan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (*record.StoredPlan, error) {
	s.reads++
	return s.Store.Plan(ctx, tx, issue, generation)
}

// A ready start reads the plan twice: where the start gives the task its branch (startTaskCopy,
// after record.Store.AssignTaskBranch) and where the outbox queues the start (startBranch). Its
// scope, its write of the issue and the next task's scope read the plan the caller read, as the
// writes before them left it (TaskScope.plan). The one task of a plan of one, waiting at admitted,
// starts at its first step under its agent's claim.
//
// The fault it catches is a start that reads the plan again for each part of it (its control:
// putScope reading the plan again, which counts three).
func TestAReadyStartReadsThePlanTwice(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	counting := &planReads{Store: engine.store}
	engine.store = counting
	root := stepRoot(t, pool, phase.Admitted)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskWaiting})

	reads := -1
	onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		stored, err := engine.store.Plan(ctx, tx, issue.Key, issue.Generation)
		if err != nil {
			return err
		}
		counting.reads = 0
		if _, err := engine.startReady(ctx, tx, issue, stored, ""); err != nil {
			return err
		}
		reads = counting.reads
		return nil
	})

	if reads != 2 {
		t.Fatalf("the ready start of task a read the plan %d times, want 2", reads)
	}
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "a", atStep(1), record.StepWaitNone)
	if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); !slices.Equal(claims, []claim.Token{agentClaim(t, root.Key, "impl-a")}) {
		t.Fatalf("the implementer's starts of %s name %v, want a's agent impl-a alone", root.Key, claims)
	}
}
