package admit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

const (
	pathsRoot, pathsChild = "LEGION-208", "LEGION-209"
	pathsArtifact         = "6a1f0c3e-2b7d-4e5a-9c81-3d4f5e6a7b80"
)

// todaysDefaultRoles is the roles of the steps of a project that names no default_steps, in their
// order: today's line up to the owner's acceptance.
var todaysDefaultRoles = []string{"planner", "implementer", "tester", "reviewer", "acceptance"}

// architectClaim is the claim of the architect of root, the owner who gives every plan of these
// tests: it acts on root and on each issue under root on the Dispatch chain (record.PlanOwners).
func architectClaim(t *testing.T, root string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, root, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// rootArchitectsRead is what an architect route reads of key for the architect of key's gate root
// on its Dispatch chain (record.Chains.StateTree; api's ownsOnChain and openStep): that
// architect's claim, the issue's generation and the run of the step it stands in, which sign_off
// and request_backward_move name on their facts.
func rootArchitectsRead(t *testing.T, pool *pgxpool.Pool, key string) (claim.Token, uint64, record.StepEntry) {
	t.Helper()
	recorded := issue(t, pool, key)
	var entry record.StepEntry
	var root string
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx, records := context.Background(), record.NewStore()
		var err error
		if entry, err = records.OpenStep(ctx, tx, key); err != nil {
			return err
		}
		chains, err := record.ReadChainsOf(ctx, tx, records, recorded.Project, []string{key})
		if err != nil {
			return err
		}
		root = chains.StateTree(key)
		return nil
	}); err != nil {
		t.Fatalf("read the open step and the chain of %s: %v", key, err)
	}
	return architectClaim(t, root), recorded.Generation, entry
}

// ownersPlanRows is the plan `implementer reviewer` that the architect of root gave for key at
// generation: version 1 of one document, its one task a standing as task says.
func ownersPlanRows(t *testing.T, key, root string, generation uint64, task record.PlanTask) (record.PlanVersion, []record.PlanTask, []record.PlanStep) {
	t.Helper()
	version := record.PlanVersion{Issue: key, Generation: generation, Version: 1, ArtifactID: pathsArtifact, Source: record.PlanSourceOwner,
		GivenBy: architectClaim(t, root), GivenAt: fixedNow.Add(-time.Hour)}
	task.Issue, task.Generation, task.Task, task.PlanVersion, task.BlockText = key, generation, "a", 1, "Build the widget."
	steps := []record.PlanStep{
		{Issue: key, Generation: generation, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
		{Issue: key, Generation: generation, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
	}
	return version, []record.PlanTask{task}, steps
}

// seedOwnersPlan records ownersPlanRows and answers the plan as the store reads it back.
func seedOwnersPlan(t *testing.T, pool *pgxpool.Pool, key, root string, generation uint64, task record.PlanTask) *record.StoredPlan {
	t.Helper()
	version, tasks, steps := ownersPlanRows(t, key, root, generation, task)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPlan(context.Background(), tx, version, tasks, steps); err != nil {
			t.Fatalf("seed the plan of %s at generation %d: %v", key, generation, err)
		}
	})
	return storedPlanAt(t, pool, key, generation)
}

// seedDefaultPlan records the plan of the default steps that key's run at generation took at its
// first step, its one task default done: a run of this release whose owner gave no plan.
func seedDefaultPlan(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) {
	t.Helper()
	version := record.PlanVersion{Issue: key, Generation: generation, Source: record.PlanSourceDefault, GivenAt: fixedNow.Add(-time.Hour)}
	tasks := []record.PlanTask{{Issue: key, Generation: generation, Task: "default", BlockText: "The project's default steps.",
		StepsByDefault: true, State: record.TaskDone}}
	types := []plan.Type{plan.TypePlanning, plan.TypeImplementation, plan.TypeTesting, plan.TypeReview, plan.TypeReview}
	steps := make([]record.PlanStep, len(todaysDefaultRoles))
	for i, role := range todaysDefaultRoles {
		steps[i] = record.PlanStep{Issue: key, Generation: generation, Task: "default", Position: i + 1, Role: role, Type: types[i]}
	}
	steps[4].AgentKey = "owner"
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPlan(context.Background(), tx, version, tasks, steps); err != nil {
			t.Fatalf("seed the default plan of %s at generation %d: %v", key, generation, err)
		}
	})
}

// storedPlanAt is the plan of key's run at generation, nil where it holds no plan rows.
func storedPlanAt(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) *record.StoredPlan {
	t.Helper()
	var stored *record.StoredPlan
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if stored, err = record.NewStore().Plan(context.Background(), tx, key, generation); err != nil {
			t.Fatalf("read the plan of %s at generation %d: %v", key, generation, err)
		}
	})
	return stored
}

// workerStarts is each start of a phase worker that carries work queued on key, as
// `role/phase@generation`, oldest first (effects leaves out the task-less ones). The architect's
// start names no phase, so it is not among them.
func workerStarts(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	var starts []string
	for _, got := range effects(t, pool) {
		if request, ok := got.payload.(record.SuperviseRequest); ok && got.issue == key && request.Op == "start" && request.Phase != "" {
			starts = append(starts, fmt.Sprintf("%s/%s@%d", request.Role, request.Phase, request.Generation))
		}
	}
	return starts
}

// stepRoles is the roles of the steps of task in stored, in their order.
func stepRoles(stored *record.StoredPlan, task string) []string {
	var roles []string
	for _, step := range stored.Steps[task] {
		roles = append(roles, step.Role)
	}
	return roles
}

// assertTakesOwnersPlan fails unless key's run at generation stands at its first step, implementing,
// on the owner's plan carried from its start (record.Store's CarryPlan without carried): version 1
// of the owner's document, given by the architect of root, its task a running at step 1 with every
// count 0, and its steps implementer and reviewer, no review read; and unless the one worker start
// queued is the implementer's of that generation, so no planner starts.
func assertTakesOwnersPlan(t *testing.T, pool *pgxpool.Pool, key, root string, generation uint64) {
	t.Helper()
	if got := issue(t, pool, key); got.Generation != generation || got.Phase != phase.Implementing || got.Hold != nil {
		t.Fatalf("%s = generation %d in %s (hold %+v), want generation %d implementing, the first step of its owner's plan",
			key, got.Generation, got.Phase, got.Hold, generation)
	}
	stored := storedPlanAt(t, pool, key, generation)
	if stored == nil {
		t.Fatalf("%s at generation %d holds no plan rows, want its owner's plan carried", key, generation)
	}
	if v := stored.Version; v.Source != record.PlanSourceOwner || v.Version != 1 || v.ArtifactID != pathsArtifact || v.GivenBy != architectClaim(t, root) {
		t.Fatalf("the version in force of %s at generation %d = %+v, want version 1 of %s given by the architect of %s", key, generation, v, pathsArtifact, root)
	}
	if len(stored.Tasks) != 1 {
		t.Fatalf("the tasks of %s at generation %d = %+v, want the one task a", key, generation, stored.Tasks)
	}
	task := stored.Tasks[0]
	if task.Task != "a" || task.State != record.TaskRunning || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) ||
		task.Counts.Pass != 0 || task.Counts.Rounds != 0 || task.Counts.ConflictRounds != 0 || len(task.Counts.BackwardMoves) != 0 || task.Counts.CapTold != 0 {
		t.Fatalf("task a of %s at generation %d = %+v, want it running at step 1 with every count 0, carried from its start", key, generation, task)
	}
	if got := stepRoles(stored, "a"); !reflect.DeepEqual(got, []string{"implementer", "reviewer"}) {
		t.Fatalf("the steps of task a = %v, want implementer and reviewer", got)
	}
	if got, want := workerStarts(t, pool, key), []string{fmt.Sprintf("implementer/implementing@%d", generation)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the worker starts of %s = %v, want %v: the first step of the owner's plan, and no planner", key, got, want)
	}
}

// assertTakesDefaultSteps fails unless key's run at generation stands at planning on the project's
// default steps, stored at its first step, and the one worker start queued is the planner's
// of that generation: today's line.
func assertTakesDefaultSteps(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) {
	t.Helper()
	if got := issue(t, pool, key); got.Generation != generation || got.Phase != phase.Planning || got.Hold != nil {
		t.Fatalf("%s = generation %d in %s (hold %+v), want generation %d planning, the first of the default steps",
			key, got.Generation, got.Phase, got.Hold, generation)
	}
	stored := storedPlanAt(t, pool, key, generation)
	if stored == nil || stored.Version.Source != record.PlanSourceDefault || stored.Version.Version != 0 || stored.Version.GivenBy != "" ||
		len(stored.Tasks) != 1 || stored.Tasks[0].Task != "default" || stored.Tasks[0].State != record.TaskRunning ||
		stored.Tasks[0].Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) {
		t.Fatalf("the plan of %s at generation %d = %+v, want the default steps, their task running at step 1", key, generation, stored)
	}
	if got := stepRoles(stored, "default"); !reflect.DeepEqual(got, todaysDefaultRoles) {
		t.Fatalf("the default steps of %s = %v, want %v", key, got, todaysDefaultRoles)
	}
	if got, want := workerStarts(t, pool, key), []string{fmt.Sprintf("planner/planning@%d", generation)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the worker starts of %s = %v, want %v", key, got, want)
	}
}

// assertPlanUnchanged fails unless the plan of key at generation is still want: a new run copies
// the plan of an earlier one, and leaves it as it was.
func assertPlanUnchanged(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, want *record.StoredPlan) {
	t.Helper()
	if got := storedPlanAt(t, pool, key, generation); !reflect.DeepEqual(got, want) {
		t.Fatalf("the plan of %s at generation %d = %+v, want it as it was: %+v", key, generation, got, want)
	}
}

// liveTreeWithDoneChild records the tree of pathsRoot running, implementing on a slot of its own
// with its design gate approved, and pathsChild done at generation 1 beneath it, and answers an
// admission with a slot to spare, so a run the child opens takes a slot of its own at once.
func liveTreeWithDoneChild(t *testing.T, pool *pgxpool.Pool) *Admission {
	t.Helper()
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlottedInPhase(t, pool, testProject, pathsRoot, "A", phase.Implementing, nil)
	seedGate(t, pool, record.DesignGate{Issue: pathsRoot, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: new(1)})
	parent := pathsRoot
	putIssue(t, pool, record.Issue{Key: pathsChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: pathsRoot,
		Project: testProject, Title: "child", Parent: &parent, Phase: phase.Done, Generation: 1, Status: "done", Rank: "B", LastDispatchSeq: 2})
	return admission
}

// rerunChild is rerun_child on pathsChild (ownTodoOnChild).
func rerunChild(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	ownTodoOnChild(t, pool, admission, "rerun")
}

// ownTodoOnChild is the daemon's own todo on pathsChild, which rerun_child and release_children
// make: the route writes todo on the child in Dispatch and applies the event its own write makes
// in the request (api's recordOwnStatusWrite), through the daemon's handlers, the copy of
// Dispatch's issues first (blockers.Mirror): an unrecorded child's chain is read from its row there.
func ownTodoOnChild(t *testing.T, pool *pgxpool.Pool, admission *Admission, route string) {
	t.Helper()
	parent := pathsRoot
	written := dispatch.Issue{Key: pathsChild, Project: testProject, Title: "child", Status: "todo", Parent: &parent, Rank: "B",
		LastSeq: 3, ResolvedRepo: "acme/widgets"}
	event := intake.OwnStatusWrite(written)
	fact := intake.ObservedIssue(written)
	fact.Delivered = &event
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "status/own-write/"+route, fact,
		blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission); err != nil {
		t.Fatalf("apply the %s of %s: %v", route, pathsChild, err)
	}
}

// personsTodo is a person's move of pathsChild to todo in Dispatch, as its live event carries it.
func personsTodo(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: pathsChild, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated",
		Status: "todo", Title: "child", Parent: pathsRoot, Rank: "B"}, admission.engine)
}

// assertChildStarted fails unless the tree's architect was told the child opens a new run, its
// second, and that it starts its first step, first.
func assertChildStarted(t *testing.T, pool *pgxpool.Pool, first phase.Phase) {
	t.Helper()
	want := []string{pathsChild + " is todo; it opens a new run, generation 2, from its first phase",
		fmt.Sprintf("%s starts %s under %s: it reached the front of the waiting line and took an admission slot", pathsChild, first, pathsRoot) + childStartedOwnership}
	if got := childStatusReasons(t, pool, pathsChild); !reflect.DeepEqual(got, want) {
		t.Fatalf("the child-status notices of %s = %q, want %q", pathsChild, got, want)
	}
}

// childStartedOwnership is how every child-status notice of a child's start ends, after its
// reason: the child's own architect starts with it and owns it while that architect holds its
// role, so the architect it tells hears only what goes above that architect
// (workflow.ChildStartedNotice).
const childStartedOwnership = ". Its own architect starts with it and owns it while that architect holds its role: its " +
	"decomposition, its children and its acceptance. You hear its child-status and child-closed notices, a worker-died or " +
	"worker-stalled of its architect, and its other notices while its architect does not hold its role, which the daemon's " +
	"holdsRole decides"

// childStatusReasons is the reason of each child-status notice queued on key, oldest first: what
// the architect above key is told of its runs.
func childStatusReasons(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	var reasons []string
	for _, got := range effects(t, pool) {
		if notice, ok := got.payload.(record.Notice); ok && got.issue == key && notice.Kind == "child-status" {
			reasons = append(reasons, notice.Reason)
		}
	}
	return reasons
}

// closedRoot records pathsRoot done at generation 1, its run ended, holding no slot.
func closedRoot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: pathsRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: pathsRoot,
		Project: testProject, Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", HandedOver: true, LastDispatchSeq: 1})
}

// readmitAndOpen is a person's todo on the closed pathsRoot, which opens its next run at
// generation 2 (NewRun, decision 8 of dispatch://INFRA-4730) with its architect started, and then
// the opening of its design gate, which starts its first step (the workflow's gateOpened).
func readmitAndOpen(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: pathsRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "A", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, pathsRoot); got.Generation != 2 || got.Phase != phase.Admitted {
		t.Fatalf("%s after the todo = %#v, want generation 2 at admitted: the fixture must open its next run", pathsRoot, got)
	}
	applyFact(t, pool, admission, "root-gate", intake.GateRegistered{Issue: pathsRoot, ArtifactID: "artifact-root", Version: 3, ApprovedVersion: 3,
		ApprovedMark: record.EventMark{Seq: 9, At: reviewedAt}})
}

// Each path that opens a generation of an issue says what happens to its plan, and each
// subtest asserts the first step that starts. A rerun of a child (rerun_child), a person's move
// of a done child to todo and a person's todo on a closed root open a new run (NewRun) whose first
// step takes the newest plan its owner gave, of any generation, from its start: generation 2
// starts at implementing, the first step of `implementer reviewer`, with every count 0, and no
// planner starts (the workflow's planFirstStep, at StartFirstPhase and gateOpened). Each one's
// control is the same path for an issue whose last run took the default steps, because its owner
// gave no plan: generation 2 starts planning on the default steps. The twin: an issue that ran
// before the release has no plan rows, and each path goes on as today: a new run starts planning.
// A run that a hand-over of the release before carries in is the first boot's conversion's to
// carry with its plan (the daemon's TestALandedHandOverCarriesItsPlanIntoItsNewRun): no path of
// this release hands an issue over.
func TestEachPathThatOpensAGenerationSaysItsPlan(t *testing.T) {
	childPaths := []struct {
		name string
		open func(*testing.T, *pgxpool.Pool, *Admission)
	}{
		{"a rerun of a child", rerunChild},
		{"a person's move of a done child to todo", personsTodo},
	}
	for _, path := range childPaths {
		t.Run(path.name+" takes its owner's newest plan from its start", func(t *testing.T) {
			pool := migratedPool(t)
			admission := liveTreeWithDoneChild(t, pool)
			old := seedOwnersPlan(t, pool, pathsChild, pathsRoot, 1, record.PlanTask{State: record.TaskDone,
				Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}, Counts: record.ReturnCounts{Pass: 1, Rounds: 2}})

			path.open(t, pool, admission)

			assertTakesOwnersPlan(t, pool, pathsChild, pathsRoot, 2)
			assertPlanUnchanged(t, pool, pathsChild, 1, old)
			assertChildStarted(t, pool, phase.Implementing)
		})
		t.Run(path.name+", control: a child whose last run took the default steps starts planning on them", func(t *testing.T) {
			pool := migratedPool(t)
			admission := liveTreeWithDoneChild(t, pool)
			seedDefaultPlan(t, pool, pathsChild, 1)

			path.open(t, pool, admission)

			assertTakesDefaultSteps(t, pool, pathsChild, 2)
			assertChildStarted(t, pool, phase.Planning)
		})
	}

	t.Run("a new run of a closed root takes its owner's newest plan from its start", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		closedRoot(t, pool)
		old := seedOwnersPlan(t, pool, pathsRoot, pathsRoot, 1, record.PlanTask{State: record.TaskDone,
			Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}, Counts: record.ReturnCounts{Pass: 1, Rounds: 1}})

		readmitAndOpen(t, pool, admission)

		assertTakesOwnersPlan(t, pool, pathsRoot, pathsRoot, 2)
		assertPlanUnchanged(t, pool, pathsRoot, 1, old)
	})
	t.Run("a new run of a closed root, control: a root whose last run took the default steps starts planning on them", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		closedRoot(t, pool)
		seedDefaultPlan(t, pool, pathsRoot, 1)

		readmitAndOpen(t, pool, admission)

		assertTakesDefaultSteps(t, pool, pathsRoot, 2)
	})

	t.Run("an issue that ran before the release goes on as today on each path", func(t *testing.T) {
		for _, path := range childPaths {
			t.Run(path.name+" starts planning on the default steps", func(t *testing.T) {
				pool := migratedPool(t)
				admission := liveTreeWithDoneChild(t, pool)

				path.open(t, pool, admission)

				assertTakesDefaultSteps(t, pool, pathsChild, 2)
				assertChildStarted(t, pool, phase.Planning)
				if got := storedPlanAt(t, pool, pathsChild, 1); got != nil {
					t.Fatalf("the run before the release holds the plan %+v, want none", got)
				}
			})
		}
		t.Run("a new run of a closed root starts planning on the default steps", func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			closedRoot(t, pool)

			readmitAndOpen(t, pool, admission)

			assertTakesDefaultSteps(t, pool, pathsRoot, 2)
		})
	})
}

// A plan given at generation 0 for an issue the daemon never recorded waits through the end of
// its giver's run: the root closes, and a person's todo opens its next run (NewRun). The rows at
// generation 0 are still there, because the token of the root's architect comes from the root's
// key, which the new run keeps, and the root's release of the issue starts it at implementing, the
// first step of that plan, with no notice of a plan not taken. The released issue runs at its own
// first generation, 1, whatever run its root is in (R17 of dispatch://INFRA-3320: an issue's own
// facts). The control: the same release of an issue its owner gave no plan starts planning on the
// default steps.
func TestAPlanGivenBeforeTheIssueIsRecordedWaitsThroughTheRootsNextRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		given bool
	}{
		{name: "the plan at generation 0 starts the released issue at implementing", given: true},
		{name: "control: an issue its owner gave no plan starts planning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
			closedRoot(t, pool)
			var waiting *record.StoredPlan
			if tc.given {
				waiting = seedOwnersPlan(t, pool, pathsChild, pathsRoot, 0, record.PlanTask{State: record.TaskWaiting})
			}
			readmitAndOpen(t, pool, admission)
			assertPlanUnchanged(t, pool, pathsChild, 0, waiting)
			if maybeIssue(t, pool, pathsChild) != nil {
				t.Fatalf("%s is recorded before its release, want it unrecorded", pathsChild)
			}

			// The architect's release_children writes todo on the issue (api's enterTodoChild).
			ownTodoOnChild(t, pool, admission, "release")

			if !tc.given {
				assertTakesDefaultSteps(t, pool, pathsChild, 1)
				return
			}
			assertTakesOwnersPlan(t, pool, pathsChild, pathsRoot, 1)
			assertPlanUnchanged(t, pool, pathsChild, 0, waiting)
			for _, got := range effects(t, pool) {
				if notice, ok := got.payload.(record.Notice); ok && notice.Kind == "plan-not-taken" {
					t.Fatalf("the release told %+v, want no plan passed over", notice)
				}
			}
		})
	}
}
