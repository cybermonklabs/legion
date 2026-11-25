package workflow

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ownersLine is the line of the owner's plan seedOwnersPlan writes: an implementer, a reviewer,
// then Legion's own retro and merge.
var ownersLine = []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}

// seedOwnersPlan writes the plan key's owner gave for its run generation: version 2, one task a
// whose steps are `implementer reviewer`, waiting at no act.
func seedOwnersPlan(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) {
	t.Helper()
	v := record.PlanVersion{Issue: key, Generation: generation, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: claim.Token(treeRoleToken(t, key, claim.RoleArchitect)), GivenAt: stepClock}
	task := record.PlanTask{Issue: key, Generation: generation, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", State: record.TaskWaiting}
	steps := []record.PlanStep{
		{Issue: key, Generation: generation, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
		{Issue: key, Generation: generation, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutPlan(t.Context(), tx, v, []record.PlanTask{task}, steps)
	})
}

// walkOf is the walk engine reads of key as the store holds it now.
func walkOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) Walk {
	t.Helper()
	var w Walk
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, key)
		if err != nil || issue == nil {
			return fmt.Errorf("read %s: %v (recorded %t)", key, err, issue != nil)
		}
		w, err = engine.walk(ctx, tx, *issue)
		return err
	})

	return w
}

// An issue whose run has no plan rows walks today's line, phase.Order, and the table of that line
// is today's (TestAWalkWithNoPlanRowsMovesByTodaysTable): what an issue that ran before this
// release walks. Plan rows of another run of the issue are not its run's. Once its run has rows,
// the walk is theirs: the task, its steps, its line, and where the task stands.
func TestAWalkWithNoRowsIsTodaysLine(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := stepEngine(nil)
	root := stepRoot(t, pool, phase.Implementing)
	seedOwnersPlan(t, pool, root.Key, 0)

	w := walkOf(t, pool, engine, root.Key)
	if w.Stored || w.Task != "default" || !slices.Equal(w.Line, phase.Order) || len(w.Steps) != 5 || w.Steps[0].Role != "planner" || w.Steps[4].AgentKey != "owner" {
		t.Fatalf("the walk of %s with no plan rows of its run = %+v, want today's line %v on today's five steps of the task default, not stored", root.Key, w, phase.Order)
	}

	seedOwnersPlan(t, pool, root.Key, root.Generation)
	w = walkOf(t, pool, engine, root.Key)
	if !w.Stored || w.Task != "a" || !slices.Equal(w.Line, ownersLine) || len(w.Steps) != 2 || w.State != record.TaskWaiting || w.Place != (record.TaskPlace{}) {
		t.Fatalf("the walk of %s with its run's plan = %+v, want task a on %v, waiting at no act", root.Key, w, ownersLine)
	}
}

// taskStands says why key's task does not stand at place in state with issues.phase the phase
// that place derives, nil when it does. The phase and the task's row are read in one select, so
// the two are the store's at one moment; want is the phase the move should have reached, which
// holds the derivation (issuePhaseOf) to the line as well as to itself.
func taskStands(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, place record.TaskPlace, state record.TaskState, want phase.Phase) error {
	t.Helper()
	var got string
	var stood record.TaskPlace
	var stoodState record.TaskState
	if err := pool.QueryRow(t.Context(), `select i.phase, t.running_kind, t.position, t.state from issues i
		join plan_tasks t on t.issue = i.key and t.generation = i.generation where i.key = $1`, key).
		Scan(&got, &stood.Kind, &stood.Position, &stoodState); err != nil {
		return fmt.Errorf("read the phase and the task of %s: %w", key, err)
	}
	if stood != place || stoodState != state {
		return fmt.Errorf("the task of %s stands at %+v, %s; want %+v, %s", key, stood, stoodState, place, state)
	}
	derived := issuePhaseOf(walkOf(t, pool, engine, key), stood, stoodState)
	if phase.Phase(got) != derived || phase.Phase(got) != want {
		return fmt.Errorf("issues.phase of %s = %s with its task at %+v, %s, which derives %s; want %s", key, got, stood, stoodState, derived, want)
	}
	return nil
}

// leaveAs takes key out of Legion's work at status as a person's move does (leave), on its record
// as the store holds it.
func leaveAs(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, status string) {
	t.Helper()
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		issue.Status = status
		if err := engine.store.PutIssue(ctx, tx, *issue); err != nil {
			return err
		}
		return engine.leave(ctx, tx, *issue, status)
	})
}

// returnAsTodo brings key back into Legion's work as a person's todo on an issue a stop left does
// (ReturnToWork), on its record as the store holds it.
func returnAsTodo(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	onStored(t, pool, engine, key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		parent := ""
		if issue.Parent != nil {
			parent = *issue.Parent
		}
		return engine.ReturnToWork(ctx, tx, issue, intake.DispatchIssue{Key: key, ResolvedRepo: issue.Repo, Seq: issue.LastDispatchSeq + 1,
			Type: "issue.updated", Status: "todo", Title: issue.Title, Parent: parent, Rank: issue.Rank})
	})
}

// taskStateAt is the state of key's task at generation, the run a new run replaced.
func taskStateAt(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) record.TaskState {
	t.Helper()
	var state record.TaskState
	if err := pool.QueryRow(t.Context(), `select state from plan_tasks where issue = $1 and generation = $2`, key, int64(generation)).Scan(&state); err != nil {
		t.Fatalf("read the task of %s at generation %d: %v", key, generation, err)
	}
	return state
}

// The task's place and issues.phase move in one write: after each move of an issue with plan rows,
// the phase the store holds is the one the task's place derives. A root and a child with the plan
// `implementer reviewer` each walk it from admitted to their merge, and the merge's close ends the
// task done there, the one end at which its last step passed (closeLanded). A person's backlog,
// and a person's done that is not the merge's close, in the middle of the first step stop the task
// at that step, as stop_task stops it, and the issue keeps its phase (stopTaskAtLeave,
// dispatch://INFRA-4730, the architect's decision of 2026-11-23): neither done nor left, since the
// run goes on, and a todo resumes the task there at a new entry (resumeStoppedTask). Every other
// end of a run leaves the task left where it stood: a root admission found with no way to run
// (LeaveUnslotted), and a new run, whose task is its own (NewRun). The control writes
// issues.phase done and leaves the task's row running at its merge, and the check fails.
func TestTheTaskPlaceAndThePhaseMoveInOneWrite(t *testing.T) {
	t.Parallel()
	moves := []struct {
		trigger TriggerKind
		place   record.TaskPlace
		phase   phase.Phase
	}{
		{TriggerGateOpened, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, phase.Implementing},
		{TriggerImplementationReady, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, phase.Reviewing},
		{TriggerReviewApproved, record.TaskPlace{Kind: record.PlaceRetro}, phase.Retro},
		{TriggerRetroCompleted, record.TaskPlace{Kind: record.PlaceMerge}, phase.Merging},
	}
	merge := record.TaskPlace{Kind: record.PlaceMerge}
	walkToMerge := func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
		t.Helper()
		if err := taskStands(t, pool, engine, key, record.TaskPlace{}, record.TaskWaiting, phase.Admitted); err != nil {
			t.Fatal(err)
		}
		for _, m := range moves {
			move(t, pool, engine, key, m.trigger, "", record.PhaseRow{})
			if err := taskStands(t, pool, engine, key, m.place, record.TaskRunning, m.phase); err != nil {
				t.Fatalf("after %s: %v", m.trigger, err)
			}
		}
	}
	child := func(t *testing.T, pool *pgxpool.Pool) record.Issue {
		t.Helper()
		stepRoot(t, pool, phase.Implementing)
		issue := stepChild(t, pool, "LEGION-209", phase.Admitted, nil)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: issue.Key, Repo: "sjawhar/legion", Number: 43,
			Branch: "legion/LEGION-209", HeadSHA: "child-head", Failing: []string{}})
		return issue
	}

	t.Run("a root walked to its merge and closed at it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		root := stepRoot(t, pool, phase.Admitted)
		seedOwnersPlan(t, pool, root.Key, root.Generation)
		walkToMerge(t, pool, engine, root.Key)
		endRunAs(t, pool, engine, root.Key, "done")
		if err := taskStands(t, pool, engine, root.Key, merge, record.TaskDone, phase.Done); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a child walked to its merge and closed at it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		issue := child(t, pool)
		seedOwnersPlan(t, pool, issue.Key, issue.Generation)
		walkToMerge(t, pool, engine, issue.Key)
		endRunAs(t, pool, engine, issue.Key, "done")
		if err := taskStands(t, pool, engine, issue.Key, merge, record.TaskDone, phase.Done); err != nil {
			t.Fatal(err)
		}
	})

	// A person's stop in the middle of the first step, a backlog or a done that is not the merge's
	// close, stops the task at that step and keeps the phase it derives; a todo resumes it there.
	stepOne := record.TaskPlace{Kind: record.PlaceStep, Position: 1}
	for _, tc := range []struct {
		name, status string
		root         bool
	}{
		{"a child parked at backlog in the middle of its first step", "backlog", false},
		{"a root parked at backlog in the middle of its first step", "backlog", true},
		{"a child a person set done in the middle of its first step, which is no merge's close", "done", false},
	} {
		t.Run(tc.name+": the task stops there, and a todo resumes it", func(t *testing.T) {
			pool := migratedPool(t)
			engine := stepEngine(nil)
			var issue record.Issue
			if tc.root {
				issue = stepRoot(t, pool, phase.Admitted)
			} else {
				issue = child(t, pool)
			}
			seedOwnersPlan(t, pool, issue.Key, issue.Generation)
			move(t, pool, engine, issue.Key, TriggerGateOpened, "", record.PhaseRow{})
			leaveAs(t, pool, engine, issue.Key, tc.status)
			if err := taskStands(t, pool, engine, issue.Key, stepOne, record.TaskStopped, phase.Implementing); err != nil {
				t.Fatalf("after the %s: %v", tc.status, err)
			}
			if task := taskOf(t, pool, engine, issue.Key); task.StoppedReason != issue.Key+" is "+tc.status {
				t.Fatalf("the stopped task's reason = %q, want %q", task.StoppedReason, issue.Key+" is "+tc.status)
			}
			returnAsTodo(t, pool, engine, issue.Key)
			if err := taskStands(t, pool, engine, issue.Key, stepOne, record.TaskRunning, phase.Implementing); err != nil {
				t.Fatalf("after the todo: %v", err)
			}
		})
	}

	// The same park of a child that started its work before Legion took plans, whose run has no
	// plan rows, keeps its phase and writes no task row.
	t.Run("a child with no plan rows parked at backlog keeps its phase with no task row", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		// The child stands in planning with no plan rows, as a child whose planning started before
		// Legion took plans stands: every first step of this release writes them (planFirstStep).
		issue := child(t, pool)
		issue.Phase = phase.Planning
		seedIssue(t, pool, issue)
		leaveAs(t, pool, engine, issue.Key, "backlog")
		var got string
		var tasks int
		if err := pool.QueryRow(t.Context(), `select phase, (select count(*) from plan_tasks where issue = $1) from issues where key = $1`,
			issue.Key).Scan(&got, &tasks); err != nil {
			t.Fatal(err)
		}
		if phase.Phase(got) != phase.Planning || tasks != 0 {
			t.Fatalf("the parked child with no plan rows stands at %s with %d task rows, want planning and none", got, tasks)
		}
	})

	// A run that ends without its merge leaves its task left where it stood.
	t.Run("a root admission ends with no way to run: the task is left, and the phase done", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		root := stepRoot(t, pool, phase.Admitted)
		seedOwnersPlan(t, pool, root.Key, root.Generation)
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		endRunAs(t, pool, engine, root.Key, "backlog")
		if err := taskStands(t, pool, engine, root.Key, stepOne, record.TaskLeft, phase.Done); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a new run of a child parked at backlog leaves the task of the run it replaces", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		issue := child(t, pool)
		seedOwnersPlan(t, pool, issue.Key, issue.Generation)
		move(t, pool, engine, issue.Key, TriggerGateOpened, "", record.PhaseRow{})
		leaveAs(t, pool, engine, issue.Key, "backlog")
		newRunOf(t, pool, engine, issue.Key, "LEGION-208")
		if got := taskStateAt(t, pool, issue.Key, issue.Generation); got != record.TaskLeft {
			t.Fatalf("the task of generation %d after the new run is %s, want left", issue.Generation, got)
		}
	})

	t.Run("control: the phase written alone fails the check", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		issue := child(t, pool)
		seedOwnersPlan(t, pool, issue.Key, issue.Generation)
		walkToMerge(t, pool, engine, issue.Key)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			stored, err := engine.store.Issue(ctx, tx, issue.Key)
			if err != nil {
				return err
			}
			stored.Phase, stored.Hold = phase.Done, nil
			return engine.store.PutIssue(ctx, tx, *stored)
		})

		if err := taskStands(t, pool, engine, issue.Key, merge, record.TaskRunning, phase.Done); err == nil {
			t.Fatal("the check passed a phase written without its task: it cannot see a write that skips the task")
		}
	})
}

// A move back is served by the table of the issue's walk, so it reaches only a phase its task's
// line holds: the reviewer of `implementer reviewer` can send the work back to implementing, and a
// move back to testing or planning, which no step of the task runs, is refused and moves nothing.
// The same move to testing of an issue with no plan rows is served, as every pair of today's line
// is (TestEveryBackwardEdgeAppliesThroughIntake).
func TestABackwardMoveReachesOnlyThePhasesOfTheTasksLine(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := stepEngine(nil)
	root := stepRoot(t, pool, phase.Admitted)
	seedOwnersPlan(t, pool, root.Key, root.Generation)
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	reviewing := record.TaskPlace{Kind: record.PlaceStep, Position: 2}
	if err := taskStands(t, pool, engine, root.Key, reviewing, record.TaskRunning, phase.Reviewing); err != nil {
		t.Fatal(err)
	}
	back := func(to phase.Phase) intake.Result {
		t.Helper()
		result, err := intake.ApplyFact(t.Context(), pool, "api", "back-to-"+string(to),
			intake.BackwardMove{Issue: root.Key, Requester: claim.RoleReviewer, To: to, Reason: "probe"}, engine, admissionStub{})
		if err != nil {
			t.Fatalf("the move back to %s: %v", to, err)
		}
		return result
	}
	for _, to := range []phase.Phase{phase.Testing, phase.Planning} {
		if result := back(to); refusalCode(result) != "BACKWARD_REFUSED" {
			t.Fatalf("the move back to %s = %+v, want BACKWARD_REFUSED: the task's line holds no %s", to, result.Refusal, to)
		}
		if err := taskStands(t, pool, engine, root.Key, reviewing, record.TaskRunning, phase.Reviewing); err != nil {
			t.Fatalf("after the refused move back to %s: %v", to, err)
		}
	}
	if result := back(phase.Implementing); result.Refusal != nil {
		t.Fatalf("the move back to implementing was refused: %+v", result.Refusal)
	}
	if err := taskStands(t, pool, engine, root.Key, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, phase.Implementing); err != nil {
		t.Fatalf("after the move back to implementing: %v", err)
	}
}

// A task in another order than today's goes back to a step before it by its own order: with the
// steps `implementer reviewer tester`, the tester runs after the review, and its move back to
// reviewing puts the task at the review's step, from which the approval moves it on to the test
// again and the test's pass to Legion's own retro. Each place moves with issues.phase in one write.
// The twin: an issue with no plan rows walks today's line, where testing comes before reviewing,
// so the tester's same move back to reviewing is refused there and moves nothing.
func TestATaskInAnyOrderGoesBackToAStepBeforeIt(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := stepEngine(nil)
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer", "tester")
	stands := func(place record.TaskPlace, want phase.Phase) {
		t.Helper()
		if err := taskStands(t, pool, engine, root.Key, place, record.TaskRunning, want); err != nil {
			t.Fatal(err)
		}
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	stands(record.TaskPlace{Kind: record.PlaceStep, Position: 1}, phase.Implementing)
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	stands(record.TaskPlace{Kind: record.PlaceStep, Position: 2}, phase.Reviewing)
	move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
	stands(record.TaskPlace{Kind: record.PlaceStep, Position: 3}, phase.Testing)

	result, err := intake.ApplyFact(t.Context(), pool, "api", "tester-back-to-reviewing",
		intake.BackwardMove{Issue: root.Key, Requester: claim.RoleTester, To: phase.Reviewing, Reason: "the review missed a case"}, engine, admissionStub{})
	if err != nil {
		t.Fatalf("the tester's move back to reviewing: %v", err)
	}
	if result.Refusal != nil {
		t.Fatalf("the tester's move back to reviewing was refused: %+v", result.Refusal)
	}
	stands(record.TaskPlace{Kind: record.PlaceStep, Position: 2}, phase.Reviewing)
	move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
	stands(record.TaskPlace{Kind: record.PlaceStep, Position: 3}, phase.Testing)
	move(t, pool, engine, root.Key, TriggerTesterPassed, "", record.PhaseRow{})
	stands(record.TaskPlace{Kind: record.PlaceRetro}, phase.Retro)

	// The twin: an issue with no plan rows, in testing, is refused the same move.
	todays := migratedPool(t)
	today := stepRoot(t, todays, phase.Testing)
	result, err = intake.ApplyFact(t.Context(), todays, "api", "tester-back-to-reviewing",
		intake.BackwardMove{Issue: today.Key, Requester: claim.RoleTester, To: phase.Reviewing, Reason: "the review missed a case"}, engine, admissionStub{})
	if err != nil {
		t.Fatalf("the tester's move back to reviewing with no plan rows: %v", err)
	}
	if refusalCode(result) != "BACKWARD_REFUSED" {
		t.Fatalf("the tester's move back to reviewing with no plan rows = %+v, want BACKWARD_REFUSED", result.Refusal)
	}
	if got := issueOf(t, todays, engine, today.Key); got.Phase != phase.Testing {
		t.Fatalf("after the refused move %s stands in %s, want testing", today.Key, got.Phase)
	}
}
