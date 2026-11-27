package workflow

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// planArtifact is the plan document every version of these tests is a version of.
const planArtifact = "6a1f0c3e-2b7d-4e5a-9c81-3d4f5e6a7b80"

// givenTask is the task block a of a version: its steps as words, `role` or `role:key` (none for
// a block that names no steps), and its text, as plan.FromBlocks reads such a block.
func givenTask(t *testing.T, text string, words ...string) plan.Task {
	t.Helper()
	steps, err := plan.ParseSteps(strings.Join(words, " "))
	if err != nil {
		t.Fatalf("the steps %q: %v", words, err)
	}
	task := plan.Task{ID: "a", Text: text}
	if len(steps) > 0 {
		task.Steps = steps
	}
	return task
}

// ownersPlan is register_plan's fact for key at generation: version of planArtifact, holding
// tasks, given by the architect of LEGION-208, the tree's root, as the route records it.
func ownersPlan(t *testing.T, key string, generation uint64, version int, tasks ...plan.Task) intake.PlanGiven {
	t.Helper()
	return intake.PlanGiven{Issue: key, Generation: generation, GivenBy: claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect)),
		Plan: plan.Plan{ArtifactID: planArtifact, Version: version, Tasks: tasks}}
}

// planID is the id the route names that registration by (api's registerPlan).
func planID(key string, generation uint64, version int) string {
	return fmt.Sprintf("plan:%s:%d:%s:%d", key, generation, planArtifact, version)
}

// mustTake fails unless result is a plan taken: no refusal, and spent.
func mustTake(t *testing.T, what string, result intake.Result) {
	t.Helper()
	if result.Refusal != nil || result.Unspent || result.Duplicate {
		t.Fatalf("%s = %+v, want it taken", what, result)
	}
}

// planStanding is everything a plan given can change: the issue record (nil for none), the plan
// of the generation, every version stored there, and the number of outbox rows.
type planStanding struct {
	issue    *record.Issue
	plan     *record.StoredPlan
	versions []string
	outbox   int
}

func planStandingOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, generation uint64) planStanding {
	t.Helper()
	var now planStanding
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if now.issue, err = engine.store.Issue(ctx, tx, key); err != nil {
			return err
		}
		now.plan, err = engine.store.Plan(ctx, tx, key, generation)
		return err
	})

	rows, err := pool.Query(t.Context(), "select version, source from issue_plans where issue = $1 and generation = $2 order by version", key, int64(generation))
	if err != nil {
		t.Fatalf("read the versions of %s: %v", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int
		var source string
		if err := rows.Scan(&version, &source); err != nil {
			t.Fatalf("read the versions of %s: %v", key, err)
		}
		now.versions = append(now.versions, fmt.Sprintf("%d %s", version, source))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the versions of %s: %v", key, err)
	}
	now.outbox = outboxRows(t, pool)
	return now
}

// task is the one task of the plan s holds, which the test fails without.
func (s planStanding) task(t *testing.T) record.PlanTask {
	t.Helper()
	if s.plan == nil || len(s.plan.Tasks) != 1 {
		t.Fatalf("the plan = %+v (versions %v), want one task", s.plan, s.versions)
	}
	return s.plan.Tasks[0]
}

// stepsOfTask is the stored steps of task, each as `role:key/type` (`role/type` with no key).
func stepsOfTask(stored *record.StoredPlan, task string) []string {
	var words []string
	for _, step := range stored.Steps[task] {
		word := step.Role
		if step.AgentKey != "" {
			word += ":" + step.AgentKey
		}
		words = append(words, word+"/"+string(step.Type))
	}
	return words
}

// processed says whether the fact id is marked applied. A release before answers a marked id given
// again as a duplicate; this one applies a PlanGiven whatever its mark (intake's factApplies).
func processed(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var marked bool
	if err := pool.QueryRow(t.Context(), "select exists (select 1 from processed_events where source = 'api' and event_id = $1)", id).Scan(&marked); err != nil {
		t.Fatalf("read whether %s is processed: %v", id, err)
	}
	return marked
}

// assertPlanRefusal fails unless result is exactly want, left unspent with its processed mark gone,
// which only a release before reads (this one applies a PlanGiven whatever its mark, intake's
// factApplies), and key stands at generation as before stood.
func assertPlanRefusal(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, generation uint64, id string, result intake.Result, want *intake.Refusal, before planStanding) {
	t.Helper()
	if !reflect.DeepEqual(result.Refusal, want) {
		t.Fatalf("the plan = %+v, want the refusal %+v", result.Refusal, want)
	}
	if !result.Unspent || processed(t, pool, id) {
		t.Fatalf("the refused plan %s is spent (unspent %v), want it unspent and unmarked, so a release before applies the same version when given again", id, result.Unspent)
	}
	if after := planStandingOf(t, pool, engine, key, generation); !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused plan changed %s:\n before %+v\n after  %+v", key, before, after)
	}
}

// architectOf is the claim of the architect of LEGION-208, which gives every plan of these tests.
func architectOf(t *testing.T) claim.Token {
	return claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect))
}

// The owner's plan is stored where its task has not started: for a root the daemon
// recorded at admitted, at the root's generation, so that its gate opens to the plan's first step;
// for an issue the daemon has not recorded, at generation 0, where a later version replaces a
// waiting task's steps. The version is the owner's, of its document, by its giver, at the
// engine's clock, and each task waits at no act with the steps its block names, each with the type
// of its role. A block that names no steps runs the default steps, and given again it changes no
// task, also while the task runs: the held plan is read as its block was given, not from its steps.
// The version taken while the task runs queues one row, the run's plan node at that version.
// An issue that started its work before Legion took plans is refused and walks today's line, and
// so is a plan given at a generation the issue has left, and one given to a member of a lingering
// tree, which the linger keeps admitted; each refusal writes nothing and leaves the fact unspent.
// The twin: a root given no plan opens to planning, the first of the default steps, in the rows of
// the task default that its first step writes.
func TestAPlanGivenIsStoredWhereItsTaskHasNotStarted(t *testing.T) {
	t.Parallel()
	t.Run("a root at admitted takes it, and its gate opens to the plan's first step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		before := planStandingOf(t, pool, engine, root.Key, root.Generation)

		mustTake(t, "version 1", applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 1),
			ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))

		after := planStandingOf(t, pool, engine, root.Key, root.Generation)
		if !reflect.DeepEqual(after.versions, []string{"1 owner"}) || after.plan == nil {
			t.Fatalf("the versions of %s = %v, want the owner's version 1 alone", root.Key, after.versions)
		}
		got, want := after.plan.Version, record.PlanVersion{Issue: root.Key, Generation: root.Generation, Version: 1, ArtifactID: planArtifact,
			Source: record.PlanSourceOwner, GivenBy: architectOf(t), GivenAt: stepClock}
		if !got.GivenAt.Equal(want.GivenAt) {
			t.Fatalf("version 1 was given at %s, want the engine's clock %s", got.GivenAt, want.GivenAt)
		}
		if got.GivenAt = want.GivenAt; !reflect.DeepEqual(got, want) {
			t.Fatalf("the version in force = %+v, want %+v", got, want)
		}
		task := record.PlanTask{Issue: root.Key, Generation: root.Generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		if len(after.plan.Tasks) != 1 || !reflect.DeepEqual(after.plan.Tasks[0], task) {
			t.Fatalf("the tasks = %+v, want the one %+v, waiting at no act", after.plan.Tasks, task)
		}
		if got := stepsOfTask(after.plan, "a"); !slices.Equal(got, []string{"implementer/implementation", "reviewer/review"}) {
			t.Fatalf("the steps of a = %v, want the implementer and the reviewer, each with its role's type", got)
		}
		if !reflect.DeepEqual(after.issue, before.issue) || after.outbox != before.outbox {
			t.Fatalf("the plan moved %s or queued %d outbox rows, want only the plan stored", root.Key, after.outbox-before.outbox)
		}

		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing {
			t.Fatalf("%s after its gate opened stands in %s, want implementing, the first step of its owner's plan", root.Key, issue.Phase)
		}
		if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskRunning || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) {
			t.Fatalf("task a after the gate opened is %s at %+v, want running at step 1", task.State, task.Place)
		}
		if planners, implementers := roleStartsOf(t, pool, root.Key, claim.RolePlanner), roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(planners) != 0 || len(implementers) != 1 {
			t.Fatalf("the gate started %d planners and %d implementers, want no planner and one implementer", len(planners), len(implementers))
		}
	})

	t.Run("the twin: a root given no plan opens to planning, the first step of the task default", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		firstStep(t, pool, engine, root.Key)
		want := walkStanding{phase.Planning, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"0 default"}, todaysStoredSteps}
		if got := walkStandingOf(t, pool, engine, root.Key, root.Generation); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s given no plan after its gate opened stands at %+v, want %+v", root.Key, got, want)
		}
	})

	t.Run("an issue the daemon has not recorded takes it at generation 0, and a later version replaces its waiting task's steps", func(t *testing.T) {
		const child = "LEGION-500"
		pool := migratedPool(t)
		engine := returnEngine()

		mustTake(t, "version 1", applyFacts(t, pool, engine)(planID(child, 0, 1), ownersPlan(t, child, 0, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		first := planStandingOf(t, pool, engine, child, 0)
		if first.issue != nil || !reflect.DeepEqual(first.versions, []string{"1 owner"}) || first.task(t).State != record.TaskWaiting {
			t.Fatalf("%s after version 1 = %+v, want no record and the owner's version 1 at generation 0, its task waiting", child, first)
		}

		mustTake(t, "version 2", applyFacts(t, pool, engine)(planID(child, 0, 2), ownersPlan(t, child, 0, 2, givenTask(t, "Build and test the widget.", "implementer", "tester"))))
		second := planStandingOf(t, pool, engine, child, 0)
		if !reflect.DeepEqual(second.versions, []string{"1 owner", "2 owner"}) || second.plan == nil || second.plan.Version.Version != 2 {
			t.Fatalf("the versions of %s = %v, want 1 and 2, 2 in force", child, second.versions)
		}
		want := record.PlanTask{Issue: child, Task: "a", PlanVersion: 2, BlockText: "Build and test the widget.", State: record.TaskWaiting}
		if len(second.plan.Tasks) != 1 || !reflect.DeepEqual(second.plan.Tasks[0], want) {
			t.Fatalf("the tasks after version 2 = %+v, want the one %+v", second.plan.Tasks, want)
		}
		if got := stepsOfTask(second.plan, "a"); !slices.Equal(got, []string{"implementer/implementation", "tester/testing"}) {
			t.Fatalf("the steps of a after version 2 = %v, want version 2's", got)
		}
	})

	t.Run("a block that names no steps runs the default steps and, given again, changes no task while it runs", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		block := givenTask(t, "Build the widget.")

		mustTake(t, "version 1", applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 1), ownersPlan(t, root.Key, root.Generation, 1, block)))
		given := planStandingOf(t, pool, engine, root.Key, root.Generation)
		if task := given.task(t); !task.StepsByDefault {
			t.Fatalf("task a, whose block names no steps, is stored with StepsByDefault false, want true")
		}
		if got := stepsOfTask(given.plan, "a"); !slices.Equal(got, []string{"planner/planning", "implementer/implementation", "tester/testing", "reviewer/review", "acceptance:owner/review"}) {
			t.Fatalf("the steps of a, whose block names none, = %v, want the default steps", got)
		}
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Planning {
			t.Fatalf("%s after its gate opened stands in %s, want planning, the first of the default steps", root.Key, issue.Phase)
		}
		running := planStandingOf(t, pool, engine, root.Key, root.Generation)

		mustTake(t, "version 2 with the same block", applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 2), ownersPlan(t, root.Key, root.Generation, 2, block)))

		after := planStandingOf(t, pool, engine, root.Key, root.Generation)
		if !reflect.DeepEqual(after.versions, []string{"1 owner", "2 owner"}) {
			t.Fatalf("the versions after the same block given again = %v, want 1 and 2", after.versions)
		}
		task, was := after.task(t), running.task(t)
		if task.State != record.TaskRunning || task.Place != was.Place || !task.StepsByDefault || !reflect.DeepEqual(after.plan.Steps, running.plan.Steps) {
			t.Fatalf("task a after the same block given again = %+v with steps %v, want it running at %+v on the same steps", task, stepsOfTask(after.plan, "a"), was.Place)
		}
		if !reflect.DeepEqual(after.issue, running.issue) || after.outbox != running.outbox+1 {
			t.Fatalf("the same block given again moved %s or queued %d outbox rows, want no move and one row, the plan node", root.Key, after.outbox-running.outbox)
		}
		node := record.PlanRecord{ID: PlanID(root.Key, root.Generation), Generation: root.Generation, Source: record.PlanSourceOwner,
			PlanDocument: &record.PlanDocument{ArtifactID: planArtifact, Version: 2},
			Tasks:        []record.PlanRecordTask{{ID: "a", Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}, Needs: &[]string{}}}}
		if nodes := planWrites(t, pool, root.Key); len(nodes) == 0 || !reflect.DeepEqual(nodes[len(nodes)-1], node) {
			t.Fatalf("the plan nodes of %s after version 2 = %+v, want the newest %+v", root.Key, nodes, node)
		}
	})

	t.Run("an issue that started before Legion took plans is refused and walks today's line", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		id := planID(root.Key, root.Generation, 1)
		before := planStandingOf(t, pool, engine, root.Key, root.Generation)

		result := applyFacts(t, pool, engine)(id, ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer")))

		reason := "LEGION-208 started its work before Legion took plans, so it runs the steps it started with to their end. This plan changed nothing."
		assertPlanRefusal(t, pool, engine, root.Key, root.Generation, id, result, &intake.Refusal{Status: 409, Code: "PLAN_REFUSED",
			Message: "Legion refused this plan and recorded nothing: " + reason, Reasons: []plan.Refusal{{Reason: reason}}}, before)
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Testing {
			t.Fatalf("%s after its implementation was ready stands in %s, want testing, today's line", root.Key, issue.Phase)
		}
	})

	t.Run("a plan given at a generation the issue has left is refused", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		root.Generation = 2
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return engine.store.PutIssue(ctx, tx, root) })
		id := planID(root.Key, 1, 1)
		before := planStandingOf(t, pool, engine, root.Key, 1)

		result := applyFacts(t, pool, engine)(id, ownersPlan(t, root.Key, 1, 1, givenTask(t, "Build the widget.", "implementer", "reviewer")))

		assertPlanRefusal(t, pool, engine, root.Key, 1, id, result, &intake.Refusal{Status: 409, Code: "PLAN_STALE_GENERATION",
			Message: "the plan was given for generation 1 of LEGION-208, and LEGION-208 is on generation 2 now. Give the plan again. This plan changed nothing."}, before)
		if now := planStandingOf(t, pool, engine, root.Key, 2); now.plan != nil {
			t.Fatalf("the refused plan wrote the plan %+v at generation 2, want none", now.plan)
		}
	})

	t.Run("a plan given to a member of a lingering tree is refused", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		member := stepChild(t, pool, "LEGION-209", phase.Admitted, nil)
		// The root's leave as done begins the tree's linger (beginLinger), which writes the root
		// done and keeps the member admitted, where a plan of its own would be stored.
		leaveAs(t, pool, engine, root.Key, "done")
		id := planID(member.Key, member.Generation, 1)
		before := planStandingOf(t, pool, engine, member.Key, member.Generation)

		result := applyFacts(t, pool, engine)(id, ownersPlan(t, member.Key, member.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer")))

		assertPlanRefusal(t, pool, engine, member.Key, member.Generation, id, result, lingeringPlan(), before)
	})
}

// running is the refusal of a version that changes or removes task a while it runs.
func runningRefusal() *intake.Refusal {
	reason := "task a is running: stop it with stop_task before you change it."
	return &intake.Refusal{Status: 409, Code: "PLAN_REFUSED", Message: "Legion refused this plan and recorded nothing: " + reason,
		Reasons: []plan.Refusal{{Task: "a", Reason: reason}}}
}

// A later version changes only a task that waits or is stopped. While
// task a runs, a version that changes its steps, or only its text, is refused naming a, and writes
// nothing. A version that leaves a's block as it is, and changes only the prose outside it, is
// taken, a goes on at its step, and the run's plan node is written again at that version. A
// version at or below the one in force, and a version of another document, are refused. A version
// given while a is stopped that leaves its block as it is
// is taken and a stays stopped; one with other steps is taken, and starts a again at its first
// step on the new steps: the hold is cleared, the issue stands in that step's phase, its role is
// started at a raised entry told why, the pass rises by one, and no pair, round or hook is counted.
// A version that changes a task that is done, or one whose issue left the workflow, is refused.
func TestALaterVersionChangesOnlyAWaitingOrStoppedTask(t *testing.T) {
	t.Parallel()
	t.Run("while a runs, then after it is stopped", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)
		key, gen := root.Key, root.Generation
		built := givenTask(t, "Build the widget.", "implementer", "tester", "reviewer")
		mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, built)))
		move(t, pool, engine, key, TriggerGateOpened, "", record.PhaseRow{})

		before := planStandingOf(t, pool, engine, key, gen)
		id := planID(key, gen, 2)
		assertPlanRefusal(t, pool, engine, key, gen, id,
			apply(id, ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", "implementer", "reviewer"))), runningRefusal(), before)
		id = planID(key, gen, 3)
		assertPlanRefusal(t, pool, engine, key, gen, id,
			apply(id, ownersPlan(t, key, gen, 3, givenTask(t, "Build the widget, and its manual.", "implementer", "tester", "reviewer"))), runningRefusal(), before)

		// The control: the same block, with only the prose outside it changed, which no
		// plan.Plan holds, is taken, and a goes on at its step.
		mustTake(t, "version 4 with a's block as it is", apply(planID(key, gen, 4), ownersPlan(t, key, gen, 4, built)))
		kept := planStandingOf(t, pool, engine, key, gen)
		if !reflect.DeepEqual(kept.versions, []string{"1 owner", "4 owner"}) {
			t.Fatalf("the versions after version 4 = %v, want 1 and 4", kept.versions)
		}
		if task, was := kept.task(t), before.task(t); task.State != record.TaskRunning || task.Place != was.Place ||
			!reflect.DeepEqual(kept.plan.Steps, before.plan.Steps) || !sameReturnCounts(task.Counts, was.Counts) {
			t.Fatalf("task a after version 4 = %+v, want it running at %+v on its steps, its counts as they were", task, was.Place)
		}
		if !reflect.DeepEqual(kept.issue, before.issue) || kept.outbox != before.outbox+1 {
			t.Fatalf("version 4 moved %s or queued %d outbox rows, want no move and one row, the plan node", key, kept.outbox-before.outbox)
		}
		node := record.PlanRecord{ID: PlanID(key, gen), Generation: gen, Source: record.PlanSourceOwner,
			PlanDocument: &record.PlanDocument{ArtifactID: planArtifact, Version: 4},
			Tasks:        []record.PlanRecordTask{{ID: "a", Steps: []string{"implementer", "tester", "reviewer"}, Needs: &[]string{}}}}
		if nodes := planWrites(t, pool, key); len(nodes) == 0 || !reflect.DeepEqual(nodes[len(nodes)-1], node) {
			t.Fatalf("the plan nodes of %s after version 4 = %+v, want the newest %+v", key, nodes, node)
		}

		// Version 3, refused while a ran, is now below the version in force.
		id = planID(key, gen, 3)
		assertPlanRefusal(t, pool, engine, key, gen, id, apply(id, ownersPlan(t, key, gen, 3, built)), &intake.Refusal{Status: 409, Code: "PLAN_VERSION_OLD",
			Message: "the plan in force of LEGION-208 is version 4, and Legion takes only a later version: version 3 changed nothing."}, kept)
		other := ownersPlan(t, key, gen, 5, built)
		other.Plan.ArtifactID = "0b6a2d4e-9f1c-4c3a-8e5d-7a6b5c4d3e2f"
		otherID := fmt.Sprintf("plan:%s:%d:%s:%d", key, gen, other.Plan.ArtifactID, 5)
		assertPlanRefusal(t, pool, engine, key, gen, otherID, apply(otherID, other), &intake.Refusal{Status: 409, Code: "PLAN_DOCUMENT_CHANGED",
			Message: "the plan in force of LEGION-208 is document " + planArtifact + "; give a later version of that document."}, kept)

		if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}
		stopped := planStandingOf(t, pool, engine, key, gen)
		entry := roleRowOf(t, pool, engine, key, claim.RoleImplementer).Entry
		prompts := len(hookPrompts(t, pool))

		// A version that leaves the stopped task's block as it is changes no task: a stays stopped,
		// and the one row queued is the run's plan node at version 5.
		mustTake(t, "version 5 with a's block as it is", apply(planID(key, gen, 5), ownersPlan(t, key, gen, 5, built)))
		still := planStandingOf(t, pool, engine, key, gen)
		if task := still.task(t); task.State != record.TaskStopped || task.Place != stopped.task(t).Place || still.issue.Phase != phase.Held ||
			still.outbox != stopped.outbox+1 {
			t.Fatalf("task a after version 5 = %+v, %s %s, %d outbox rows queued, want it stopped where it stood, the issue held, one row, the plan node",
				task, key, still.issue.Phase, still.outbox-stopped.outbox)
		}
		node.Version = 5
		if nodes := planWrites(t, pool, key); len(nodes) == 0 || !reflect.DeepEqual(nodes[len(nodes)-1], node) {
			t.Fatalf("the plan nodes of %s after version 5 = %+v, want the newest %+v", key, nodes, node)
		}

		mustTake(t, "version 6 with other steps", apply(planID(key, gen, 6), ownersPlan(t, key, gen, 6, givenTask(t, "Build the widget.", "implementer", "reviewer"))))

		after := planStandingOf(t, pool, engine, key, gen)
		if after.issue.Phase != phase.Implementing || after.issue.Hold != nil {
			t.Fatalf("%s after version 6 stands in %s with hold %+v, want implementing, the phase of step 1, with no hold", key, after.issue.Phase, after.issue.Hold)
		}
		task := after.task(t)
		if task.State != record.TaskRunning || task.StoppedReason != "" || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) || task.PlanVersion != 6 {
			t.Fatalf("task a after version 6 = %+v, want version 6's, running at step 1 with no stop reason", task)
		}
		if got := stepsOfTask(after.plan, "a"); !slices.Equal(got, []string{"implementer/implementation", "reviewer/review"}) {
			t.Fatalf("the steps of a after version 6 = %v, want version 6's", got)
		}
		want := stopped.task(t).Counts
		want.Pass++
		if !sameReturnCounts(task.Counts, want) {
			t.Fatalf("task a counts %+v after version 6, want %+v: the pass raised by one, no pair and no round", task.Counts, want)
		}
		if got := len(hookPrompts(t, pool)); got != prompts {
			t.Fatalf("version 6 fired %d hook prompts, want none", got-prompts)
		}
		row := roleRowOf(t, pool, engine, key, claim.RoleImplementer)
		const told = "Reason: the owner gave version 6 of the plan while task a was stopped."
		if row.Entry != entry+1 || !row.EntryMovedBack || !strings.Contains(row.EntryTask, told) {
			t.Fatalf("the implementer's row has entry %d (moved back %v) and task %q, want entry %d, raised as a move back, whose task holds %q",
				row.Entry, row.EntryMovedBack, row.EntryTask, entry+1, told)
		}
		// The start of step 1 takes the place of the implementer's start the issue still owes,
		// since no outbox ran here (Store.EnqueueStart): the last start is the one at the new entry.
		starts := roleStartsOf(t, pool, key, claim.RoleImplementer)
		if len(starts) == 0 {
			t.Fatalf("version 6 left %s with no start of the implementer, want one at entry %d", key, row.Entry)
		}
		if start := starts[len(starts)-1]; start.Phase != phase.Implementing || start.Entry != row.Entry || start.Task != row.EntryTask {
			t.Fatalf("the last start of the implementer = %+v, want the start of implementing at entry %d with its row's task", start, row.Entry)
		}
	})

	t.Run("a task that is done and a task that left are refused", func(t *testing.T) {
		// The issue's leave writes its task done at a close and left at any other status
		// (leaveState), and the issue done with it.
		for _, tc := range []struct {
			state           record.TaskState
			status, message string
		}{
			{record.TaskDone, "done", "task a of LEGION-208 is done, so nothing of it is left to change. This plan changed nothing."},
			{record.TaskLeft, "backlog", "task a of LEGION-208 left the workflow with its issue before it was done, so nothing of it is left to change. This plan changed nothing."},
		} {
			t.Run(string(tc.state), func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				apply := applyFacts(t, pool, engine)
				root := stepRoot(t, pool, phase.Admitted)
				mustTake(t, "version 1", apply(planID(root.Key, root.Generation, 1), ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
				leaveAs(t, pool, engine, root.Key, tc.status)
				before := planStandingOf(t, pool, engine, root.Key, root.Generation)
				id := planID(root.Key, root.Generation, 2)

				result := apply(id, ownersPlan(t, root.Key, root.Generation, 2, givenTask(t, "Build the widget.", "implementer", "tester")))

				assertPlanRefusal(t, pool, engine, root.Key, root.Generation, id, result, &intake.Refusal{Status: 409, Code: "TASK_DONE", Message: tc.message}, before)
			})
		}
	})
}

// A version refused because its task ran is judged again when the owner's same call comes after
// stop_task, which the route names by the same id (issue, generation, document, version): the
// workflow judges every PlanGiven whatever its id's mark (intake's factApplies), so it is taken,
// rather than answered as a duplicate that took nothing. The refusal leaves the id unmarked, which
// only a release before reads.
func TestARefusedVersionIsTakenWhenGivenAgainAfterTheStop(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "tester", "reviewer"))))
	move(t, pool, engine, key, TriggerGateOpened, "", record.PhaseRow{})
	id, version2 := planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", "implementer", "reviewer"))

	if result := apply(id, version2); !reflect.DeepEqual(result.Refusal, runningRefusal()) || processed(t, pool, id) {
		t.Fatalf("version 2 while a runs = %+v (processed %v), want the running refusal, its id unmarked", result, processed(t, pool, id))
	}
	if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
	}

	mustTake(t, "version 2 given again after the stop", apply(id, version2))

	after := planStandingOf(t, pool, engine, key, gen)
	if !processed(t, pool, id) || !reflect.DeepEqual(after.versions, []string{"1 owner", "2 owner"}) || after.issue.Phase != phase.Implementing {
		t.Fatalf("version 2 given again left %s %s with versions %v (processed %v), want it taken: implementing on version 2, its id marked",
			key, after.issue.Phase, after.versions, processed(t, pool, id))
	}
	if got := stepsOfTask(after.plan, "a"); !slices.Equal(got, []string{"implementer/implementation", "reviewer/review"}) {
		t.Fatalf("the steps of a after version 2 = %v, want version 2's", got)
	}
}

// seedDefaultPlan writes the plan key's run holds at generation when its owner gave none: the
// default steps of a project that names none (config.Project.RunsByDefault) as version 0 of the
// source default, with no document and no giver, and one task default whose block names no steps
// and so runs the default steps, each with the type of its role, waiting at no act.
func seedDefaultPlan(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) {
	t.Helper()
	defaults := config.Project{}.RunsByDefault()
	version := record.PlanVersion{Issue: key, Generation: generation, Version: 0, Source: record.PlanSourceDefault, GivenAt: stepClock}
	task := record.PlanTask{Issue: key, Generation: generation, Task: plan.DefaultTask, BlockText: "The project's default steps.",
		StepsByDefault: true, State: record.TaskWaiting}
	steps := make([]record.PlanStep, len(defaults))
	for i, step := range defaults {
		steps[i] = record.PlanStep{Issue: key, Generation: generation, Task: plan.DefaultTask, Position: i + 1, Role: step.Role,
			AgentKey: step.Key, Type: builtinDeployment.Roles[step.Role].Type}
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutPlan(t.Context(), tx, version, []record.PlanTask{task}, steps)
	})
}

// A version that replaces the stopped task of the default steps starts the new task. An issue on
// the project's default steps runs its planner. While the task default runs, a
// version whose one block is a, which removes default, is refused naming default and writes
// nothing. The owner stops default: the issue is held from planning for the stopped task, and the
// planner's process is stopped. The same version given again is taken: the default version stays
// beside the owner's, the task default and its steps are gone, a alone holds the version's steps
// and runs at step 1 with every count 0 (a new task, so no pass is raised and no pair or round is
// counted), the hold is cleared, the planner's start the issue still owed gives way to the
// implementer's, and the implementer is started at a raised entry and told which version started
// it. A version that keeps default and adds a is a plan of two tasks, which the route refuses
// (api's TestRegisterPlanTakesAPlanAndRefusesWithReasons), so the workflow never takes it. The
// twin: an issue that started its work before Legion took plans, which stands in planning with no
// plan rows, takes no plan, and stays where it stands.
func TestAVersionThatReplacesTheStoppedDefaultTaskStartsTheNewTask(t *testing.T) {
	t.Parallel()
	t.Run("the stopped task default is replaced by a, which starts at its first step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)
		key, gen := root.Key, root.Generation
		seedDefaultPlan(t, pool, key, gen)
		move(t, pool, engine, key, TriggerGateOpened, "", record.PhaseRow{})
		if issue := issueOf(t, pool, engine, key); issue.Phase != phase.Planning {
			t.Fatalf("%s on the default steps after its gate opened stands in %s, want planning", key, issue.Phase)
		}
		id, version1 := planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))

		before := planStandingOf(t, pool, engine, key, gen)
		reason := "task default is running: stop it with stop_task before you change it."
		assertPlanRefusal(t, pool, engine, key, gen, id, apply(id, version1), &intake.Refusal{Status: 409, Code: "PLAN_REFUSED",
			Message: "Legion refused this plan and recorded nothing: " + reason, Reasons: []plan.Refusal{{Task: plan.DefaultTask, Reason: reason}}}, before)

		if result := apply("stop", ownersStop(t, root, plan.DefaultTask)); result.Refusal != nil {
			t.Fatalf("the owner's stop of default was refused: %+v", result.Refusal)
		}
		held := issueOf(t, pool, engine, key)
		if held.Phase != phase.Held || held.Hold == nil || *held.Hold != (record.Hold{From: phase.Planning, Reason: record.HoldTaskStopped}) {
			t.Fatalf("%s after the stop stands in %s with hold %+v, want held from planning for the stopped task", key, held.Phase, held.Hold)
		}
		if stops := superviseRowsOf(t, pool, key, "step_stop"); len(stops) != 1 || stops[0].Role != claim.RolePlanner || stops[0].Generation != gen {
			t.Fatalf("the step stops = %+v, want one of the planner", stops)
		}
		if planners := roleStartsOf(t, pool, key, claim.RolePlanner); len(planners) != 1 {
			t.Fatalf("the planner's starts before version 1 = %+v, want the one the gate's opening wrote, still owed: no outbox ran", planners)
		}
		prompts := len(hookPrompts(t, pool))

		mustTake(t, "version 1 given again after the stop", apply(id, version1))

		after := planStandingOf(t, pool, engine, key, gen)
		if !reflect.DeepEqual(after.versions, []string{"0 default", "1 owner"}) {
			t.Fatalf("the versions of %s = %v, want the default version 0 and the owner's version 1", key, after.versions)
		}
		task := after.task(t)
		if task.Task != "a" || task.PlanVersion != 1 || task.State != record.TaskRunning || task.StoppedReason != "" ||
			task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) || !sameReturnCounts(task.Counts, record.ReturnCounts{}) {
			t.Fatalf("the one task after version 1 = %+v, want a of version 1, running at step 1 with every count 0", task)
		}
		if got := stepsOfTask(after.plan, "a"); !slices.Equal(got, []string{"implementer/implementation", "reviewer/review"}) {
			t.Fatalf("the steps of a = %v, want the implementer and the reviewer, each with its role's type", got)
		}
		var leftOfDefault int
		if err := pool.QueryRow(t.Context(), `select (select count(*) from plan_tasks where issue = $1 and task = 'default')
			+ (select count(*) from plan_steps where issue = $1 and task = 'default')`, key).Scan(&leftOfDefault); err != nil || leftOfDefault != 0 {
			t.Fatalf("rows of the task default after version 1 = %d (%v), want none", leftOfDefault, err)
		}
		if after.issue.Phase != phase.Implementing || after.issue.Hold != nil {
			t.Fatalf("%s after version 1 stands in %s with hold %+v, want implementing, the phase of a's step 1, with no hold", key, after.issue.Phase, after.issue.Hold)
		}
		row := roleRowOf(t, pool, engine, key, claim.RoleImplementer)
		const told = "Reason: the owner gave version 1 of the plan while task default was stopped."
		if row.Entry != 1 || !strings.Contains(row.EntryTask, told) {
			t.Fatalf("the implementer's row has entry %d and task %q, want entry 1, raised, whose task holds %q", row.Entry, row.EntryTask, told)
		}
		if row.StepTask != "a" || row.StepPlace != 1 {
			t.Fatalf("the implementer's row names step %d of task %q, want step 1 of task a, the step its start raised it for", row.StepPlace, row.StepTask)
		}
		if starts := roleStartsOf(t, pool, key, claim.RoleImplementer); len(starts) != 1 || starts[0].Phase != phase.Implementing || starts[0].Entry != row.Entry {
			t.Fatalf("the starts of the implementer = %+v, want one, of implementing at entry %d", starts, row.Entry)
		}
		// The start of a's step 1 takes the place of the planner's start the issue still owed
		// (Store.EnqueueStart): the step of the removed task never starts.
		if planners := roleStartsOf(t, pool, key, claim.RolePlanner); len(planners) != 0 {
			t.Fatalf("the planner's starts after version 1 = %+v, want none: the task default is gone", planners)
		}
		if got := len(hookPrompts(t, pool)); got != prompts {
			t.Fatalf("version 1 fired %d hook prompts, want none: no step failed", got-prompts)
		}
		// The start of a's step 1 is counted at a's own place and recorded as a's: one start of
		// a/step/1, none of a/step/2, beside the planner's start of default/step/1 the gate's
		// opening counted.
		if got, want := stepStartCounts(t, pool, key), []string{"a/step/1=1", "default/step/1=1"}; !slices.Equal(got, want) {
			t.Fatalf("the starts of %s after version 1 = %v, want %v", key, got, want)
		}
		var startsOfA []record.StepRecord
		for _, write := range stepWrites(t, pool, key) {
			if write.Task == "a" && isStart(write) {
				startsOfA = append(startsOfA, write)
			}
		}
		if len(startsOfA) != 1 || startsOfA[0].Kind != record.PlaceStep || startsOfA[0].Step != 1 || startsOfA[0].Role != "implementer" ||
			startsOfA[0].Type != "implementation" || startsOfA[0].ID != StepID(key, gen, record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: 1}, 1) {
			t.Fatalf("the start records of task a = %+v, want one, of step 1 (implementer, implementation) at attempt 1", startsOfA)
		}
		// The version taken writes the run's plan node again, as the plan of task a.
		if nodes := planWrites(t, pool, key); len(nodes) == 0 || !reflect.DeepEqual(nodes[len(nodes)-1], ownersNode(1)) {
			t.Fatalf("the plan nodes of %s after version 1 = %+v, want the newest %+v", key, nodes, ownersNode(1))
		}
	})

	t.Run("the twin: an issue in planning that started before Legion took plans takes no plan", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		// An issue whose planning started before Legion took plans stands in planning with no plan
		// rows: every first step of this release writes them (planFirstStep).
		root := stepRoot(t, pool, phase.Planning)
		before := planStandingOf(t, pool, engine, root.Key, root.Generation)
		id := planID(root.Key, root.Generation, 1)
		assertPlanRefusal(t, pool, engine, root.Key, root.Generation, id,
			applyFacts(t, pool, engine)(id, ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))),
			intake.PlanRefused([]plan.Refusal{intake.StartedBeforePlans(root.Key)}), before)
	})
}

// walkStanding is where an issue's run stands after one move of its walk, as the store holds it:
// its phase, its one task's place and state, every version of its plan, and the task's steps
// (none where the run has no plan rows).
type walkStanding struct {
	phase    phase.Phase
	place    record.TaskPlace
	state    record.TaskState
	versions []string
	steps    []string
}

func walkStandingOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, generation uint64) walkStanding {
	t.Helper()
	now := planStandingOf(t, pool, engine, key, generation)
	at := walkStanding{phase: now.issue.Phase, versions: now.versions}
	if now.plan != nil {
		task := now.task(t)
		at.place, at.state, at.steps = task.Place, task.State, stepsOfTask(now.plan, task.Task)
	}
	return at
}

// The walk of the version an owner gave holds to its end, and holds the same across a boot. The
// issue walks the line of version 1, implementing, reviewing, retro, merging and done, and its
// run's plan holds version 1 alone at each move. With the daemon booted in the middle of the walk,
// the engine built again on the same store, the walk is the same. The control: version 2 given
// while the task waits, before its first step, is taken, and the walk starts planning. The twin:
// an issue that started its work before Legion took plans walks today's line, and its walk writes
// no plan rows. That a version Dispatch holds and nobody gave reaches no walk is the route's to
// prove, since the route alone reads Dispatch's versions: api's TestAVersionNobodyGaveChangesNothing,
// beside TestRegisterPlanTakesAPlanAndRefusesWithReasons.
func TestTheWalkOfAGivenVersionHoldsAcrossABoot(t *testing.T) {
	t.Parallel()
	const merged = "merge-42"
	versionOne := []string{"implementer/implementation", "reviewer/review"}
	want := []walkStanding{
		{phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"1 owner"}, versionOne},
		{phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning, []string{"1 owner"}, versionOne},
		{phase.Retro, record.TaskPlace{Kind: record.PlaceRetro}, record.TaskRunning, []string{"1 owner"}, versionOne},
		{phase.Merging, record.TaskPlace{Kind: record.PlaceMerge}, record.TaskRunning, []string{"1 owner"}, versionOne},
		{phase.Done, record.TaskPlace{Kind: record.PlaceMerge}, record.TaskDone, []string{"1 owner"}, versionOne},
	}
	// walk gives version 1 and walks the issue to done, building the engine again after the move
	// bootAfter (none for -1). It answers where the run stood after each move.
	walk := func(t *testing.T, bootAfter int) []walkStanding {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		key, gen := root.Key, root.Generation
		mustTake(t, "version 1", applyFacts(t, pool, engine)(planID(key, gen, 1),
			ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		moves := []func(){
			func() { move(t, pool, engine, key, TriggerGateOpened, "", record.PhaseRow{}) },
			func() { move(t, pool, engine, key, TriggerImplementationReady, "", record.PhaseRow{}) },
			func() { move(t, pool, engine, key, TriggerReviewApproved, "", record.PhaseRow{}) },
			func() { move(t, pool, engine, key, TriggerRetroCompleted, "", record.PhaseRow{}) },
			func() {
				if result := applyFacts(t, pool, engine)("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: merged}); result.Refusal != nil {
					t.Fatalf("the merge was refused: %+v", result.Refusal)
				}
			},
		}
		var walked []walkStanding
		for i, next := range moves {
			next()
			walked = append(walked, walkStandingOf(t, pool, engine, key, gen))
			if i == bootAfter {
				engine = returnEngine()
			}
		}
		// The run's plan node is version 1's, written once at the start.
		assertPlanNodes(t, pool, key, "after the walk", ownersNode(1))
		return walked
	}

	t.Run("the walk of version 1", func(t *testing.T) {
		if got := walk(t, -1); !reflect.DeepEqual(got, want) {
			t.Fatalf("the walk of version 1 =\n%+v\nwant\n%+v", got, want)
		}
	})

	t.Run("the walk of version 1 with the daemon booted in reviewing", func(t *testing.T) {
		if got := walk(t, 1); !reflect.DeepEqual(got, want) {
			t.Fatalf("the walk of version 1 with a boot in reviewing =\n%+v\nwant\n%+v", got, want)
		}
	})

	t.Run("control: version 2 given while the task waits is taken, and the walk starts planning", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)
		key, gen := root.Key, root.Generation
		mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", "planner", "implementer", "tester"))))
		move(t, pool, engine, key, TriggerGateOpened, "", record.PhaseRow{})
		got := walkStandingOf(t, pool, engine, key, gen)
		if wantAt := (walkStanding{phase.Planning, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"1 owner", "2 owner"},
			[]string{"planner/planning", "implementer/implementation", "tester/testing"}}); !reflect.DeepEqual(got, wantAt) {
			t.Fatalf("after version 2 and the gate's opening %s stands at %+v, want %+v", key, got, wantAt)
		}
	})

	t.Run("the twin: an issue that started before Legion took plans walks today's line and writes no plan rows", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		// It stands in planning with no plan rows, as an issue whose planning started before
		// Legion took plans stands: every first step of this release writes them (planFirstStep).
		root := stepRoot(t, pool, phase.Planning)
		move(t, pool, engine, root.Key, TriggerPlannerCompleted, "", record.PhaseRow{})
		if got := walkStandingOf(t, pool, engine, root.Key, root.Generation); !reflect.DeepEqual(got, walkStanding{phase: phase.Implementing}) {
			t.Fatalf("%s with no plan rows stands at %+v after planning, want implementing with no plan rows", root.Key, got)
		}
	})
}

// recordedAt records key at admitted in its first run in tree, a child of the tree's root where it
// is not that root.
func recordedAt(t *testing.T, pool *pgxpool.Pool, key, tree string) {
	t.Helper()
	issue := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: tree,
		Project: "LEGION", Title: key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	if !claim.IsTreeRoot(key, tree) {
		parent := tree
		issue.Parent = &parent
	}
	seedIssue(t, pool, issue)
}

// firstStep starts key's first phase as the waiting line does (StartFirstPhase, whose gateOpened
// reads the plans of the issue), on its record as the store holds it now.
func firstStep(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	issue := issueOf(t, pool, engine, key)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return engine.StartFirstPhase(ctx, tx, issue, nil) })
}

// planRowsOf is how many rows of key each of the three plan tables holds, of every generation:
// issue_plans, plan_tasks and plan_steps.
func planRowsOf(t *testing.T, pool *pgxpool.Pool, key string) [3]int {
	t.Helper()
	var rows [3]int
	if err := pool.QueryRow(t.Context(), `select (select count(*) from issue_plans where issue = $1),
		(select count(*) from plan_tasks where issue = $1), (select count(*) from plan_steps where issue = $1)`, key).Scan(&rows[0], &rows[1], &rows[2]); err != nil {
		t.Fatalf("count the plan rows of %s: %v", key, err)
	}
	return rows
}
