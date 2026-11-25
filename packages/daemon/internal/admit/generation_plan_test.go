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

// architectClaim is the claim of the architect of the tree of root, the owner who gives every plan
// of these tests (record.ArchitectOfTree).
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

// rootArchitectsRead is what an architect route reads of key for the architect of key's tree
// root (api's architectForIssue and openStep): that architect's claim, the issue's generation and
// the run of the step it stands in, which sign_off and request_backward_move name on their facts.
func rootArchitectsRead(t *testing.T, pool *pgxpool.Pool, key string) (claim.Token, uint64, record.StepEntry) {
	t.Helper()
	recorded := issue(t, pool, key)
	var entry record.StepEntry
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		entry, err = record.NewStore().OpenStep(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the open step of %s: %v", key, err)
	}
	return architectClaim(t, recorded.Tree), recorded.Generation, entry
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
// in the request (api's recordOwnStatusWrite).
func ownTodoOnChild(t *testing.T, pool *pgxpool.Pool, admission *Admission, route string) {
	t.Helper()
	parent := pathsRoot
	written := dispatch.Issue{Key: pathsChild, Project: testProject, Title: "child", Status: "todo", Parent: &parent, Rank: "B",
		LastSeq: 3, ResolvedRepo: "acme/widgets"}
	event := intake.OwnStatusWrite(written)
	fact := intake.ObservedIssue(written)
	fact.Delivered = &event
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "status/own-write/"+route, fact, admission.engine, admission); err != nil {
		t.Fatalf("apply the %s of %s: %v", route, pathsChild, err)
	}
}

// personsTodo is a person's move of pathsChild to todo in Dispatch, as its live event carries it.
func personsTodo(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: pathsChild, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated",
		Status: "todo", Title: "child", Parent: pathsRoot, Rank: "B"}, admission.engine)
}

// assertChildStarted fails unless the tree's architect was told the child runs again under the
// tree (`change: todo`) and that it starts its first step, first, having taken an admission slot
// at the front of the waiting line, which a start that names no issue it waited on says
// (`change: started` with no waited_on).
func assertChildStarted(t *testing.T, pool *pgxpool.Pool, first phase.Phase) {
	t.Helper()
	want := []string{pathsChild + " todo tree=" + pathsRoot + " phase= from= waited_on=",
		pathsChild + " started tree=" + pathsRoot + " phase=" + string(first) + " from= waited_on="}
	if got := joinedNotices(t, pool, pathsChild); !reflect.DeepEqual(got, want) {
		t.Fatalf("the child-status notices of %s = %q, want %q", pathsChild, got, want)
	}
}

// lingeringRoot records pathsRoot done at generation 1, lingering after its close, holding no slot.
func lingeringRoot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	until := fixedNow.Add(time.Hour)
	putIssue(t, pool, record.Issue{Key: pathsRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: pathsRoot,
		Project: testProject, Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", HandedOver: true, LingerUntil: &until, LastDispatchSeq: 1})
}

// readmitAndOpen is a person's todo on the lingering or closed pathsRoot, which re-admits it as
// generation 2 with its architect started (readmit, not carried), and then the opening of its
// design gate, which starts its first step (the workflow's gateOpened).
func readmitAndOpen(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: pathsRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "A", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, pathsRoot); got.Generation != 2 || got.Phase != phase.Admitted || got.LingerUntil != nil {
		t.Fatalf("re-admitted %s = %#v, want generation 2 at admitted with no linger: the fixture must re-admit the root", pathsRoot, got)
	}
	applyFact(t, pool, admission, "root-gate", intake.GateRegistered{Issue: pathsRoot, ArtifactID: "artifact-root", Version: 3, ApprovedVersion: 3,
		ApprovedMark: record.EventMark{Seq: 9, At: reviewedAt}})
}

// orphanOfLingeringTree records pathsChild at generation 1 of the lingering pathsRoot's tree,
// standing as shape says, with its open pull request and its hand-over, and answers the event of
// the hand-over that carries its run to a root of its own (readmit with carry).
func orphanOfLingeringTree(t *testing.T, pool *pgxpool.Pool, shape func(*record.Issue)) intake.DispatchIssue {
	t.Helper()
	lingeringRoot(t, pool)
	parent := pathsRoot
	child := record.Issue{Key: pathsChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: pathsRoot, Project: testProject,
		Title: pathsChild, Parent: &parent, Phase: phase.Reviewing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}
	shape(&child)
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		for _, put := range []error{
			records.PutIssue(ctx, tx, child),
			records.PutGate(ctx, tx, record.DesignGate{Issue: pathsRoot, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 2, ApprovedVersion: new(2)}),
			records.PutPullRequest(ctx, tx, record.PullRequest{Issue: pathsChild, Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head-42",
				Generation: 1, State: record.PullRequestOpen}),
			records.PutHandover(ctx, tx, record.Handover{Issue: pathsChild, Tree: pathsRoot, Generation: 1}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})
	return intake.DispatchIssue{Key: pathsChild, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: pathsChild,
		Parent: pathsRoot, Rank: "B", HandedOver: true, DaemonActor: true}
}

// assertCarriedOrphan fails unless pathsChild is a root of its own at generation 2 that still
// stands where its run stood, at want, with the hold it had.
func assertCarriedOrphan(t *testing.T, pool *pgxpool.Pool, want phase.Phase, hold *record.Hold) {
	t.Helper()
	if got := issue(t, pool, pathsChild); got.Tree != pathsChild || got.Generation != 2 || got.Phase != want || !reflect.DeepEqual(got.Hold, hold) {
		t.Fatalf("handed-over %s = tree %s, generation %d, %s (hold %+v), want its own tree at generation 2, still %s (hold %+v)",
			pathsChild, got.Tree, got.Generation, got.Phase, got.Hold, want, hold)
	}
}

// carriedPlan is stored as it stands at generation 1, copied to generation 2 with each task where
// it stood (record.Store's CarryPlan with carried).
func carriedPlan(stored *record.StoredPlan) *record.StoredPlan {
	carried := &record.StoredPlan{Version: stored.Version, Steps: map[string][]record.PlanStep{}}
	carried.Version.Generation = 2
	for _, task := range stored.Tasks {
		task.Generation = 2
		carried.Tasks = append(carried.Tasks, task)
	}
	for task, steps := range stored.Steps {
		for _, step := range steps {
			step.Generation = 2
			carried.Steps[task] = append(carried.Steps[task], step)
		}
	}
	return carried
}

// Each path that opens a generation of an issue says what happens to its plan, and each
// subtest asserts the first step that starts. A rerun of a child (rerun_child), a person's move
// of a done child to todo and the re-admission of a root that carries no run open a run whose
// first step takes the newest plan its owner gave, of any generation, from its start: generation
// 2 starts at implementing, the first step of `implementer reviewer`, with every count 0, and no
// planner starts (the workflow's planFirstStep, at StartFirstPhase and gateOpened). Each one's
// control is the same path for an issue whose last run took the default steps, because its owner
// gave no plan: generation 2 starts planning on the default steps. A carried orphan, handed over
// with its run, goes on at the step it stood at: generation 2 stands at reviewing with the task
// at the same place and counts (CarryPlan with carried, in readmit), and the reviewer is the step
// that starts; its control is a carried orphan held for its stopped task, which stays held with
// its task stopped and starts no step. The twin: an issue that ran before the release has no
// plan rows, and each path goes on as today: a new run starts planning, and a carried orphan
// carries no plan rows and goes on at its phase.
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

	t.Run("a re-admission of a root, not carried, takes its owner's newest plan from its start", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		lingeringRoot(t, pool)
		old := seedOwnersPlan(t, pool, pathsRoot, pathsRoot, 1, record.PlanTask{State: record.TaskDone,
			Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}, Counts: record.ReturnCounts{Pass: 1, Rounds: 1}})

		readmitAndOpen(t, pool, admission)

		assertTakesOwnersPlan(t, pool, pathsRoot, pathsRoot, 2)
		assertPlanUnchanged(t, pool, pathsRoot, 1, old)
	})
	t.Run("a re-admission of a root, control: a root whose last run took the default steps starts planning on them", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		lingeringRoot(t, pool)
		seedDefaultPlan(t, pool, pathsRoot, 1)

		readmitAndOpen(t, pool, admission)

		assertTakesDefaultSteps(t, pool, pathsRoot, 2)
	})

	t.Run("a carried orphan goes on at the step it stood at, with the same place and counts", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		handover := orphanOfLingeringTree(t, pool, func(*record.Issue) {})
		old := seedOwnersPlan(t, pool, pathsChild, pathsRoot, 1, record.PlanTask{State: record.TaskRunning,
			Place:  record.TaskPlace{Kind: record.PlaceStep, Position: 2},
			Counts: record.ReturnCounts{Pass: 2, Rounds: 1, ConflictRounds: 1, BackwardMoves: map[string]int{"reviewing-implementing": 1}, CapTold: 1}})

		apply(t, pool, admission, "handover", handover, admission.engine)

		assertCarriedOrphan(t, pool, phase.Reviewing, nil)
		if got, want := storedPlanAt(t, pool, pathsChild, 2), carriedPlan(old); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan of the carried orphan at generation 2 = %+v, want its task where it stood, with its place and counts: %+v", got, want)
		}
		assertPlanUnchanged(t, pool, pathsChild, 1, old)
		if got, want := workerStarts(t, pool, pathsChild), []string{"reviewer/reviewing@2"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the worker starts of the carried orphan = %v, want %v: the step it stood at, and no first step", got, want)
		}
	})
	t.Run("a carried orphan, control: one held for its stopped task stays held and starts no step", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		hold := &record.Hold{From: phase.Implementing, Reason: record.HoldTaskStopped}
		handover := orphanOfLingeringTree(t, pool, func(child *record.Issue) { child.Phase, child.Hold = phase.Held, hold })
		old := seedOwnersPlan(t, pool, pathsChild, pathsRoot, 1, record.PlanTask{State: record.TaskStopped, StoppedReason: "The owner stopped it.",
			Place: record.TaskPlace{Kind: record.PlaceStep, Position: 1}})

		apply(t, pool, admission, "handover", handover, admission.engine)

		assertCarriedOrphan(t, pool, phase.Held, hold)
		if got, want := storedPlanAt(t, pool, pathsChild, 2), carriedPlan(old); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan of the held orphan at generation 2 = %+v, want its task stopped where it stood: %+v", got, want)
		}
		if got := workerStarts(t, pool, pathsChild); len(got) != 0 {
			t.Fatalf("the worker starts of the held orphan = %v, want none", got)
		}
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
		t.Run("a re-admission of a root starts planning on the default steps", func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			lingeringRoot(t, pool)

			readmitAndOpen(t, pool, admission)

			assertTakesDefaultSteps(t, pool, pathsRoot, 2)
		})
		t.Run("a carried orphan carries no plan rows and goes on at its phase", func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			handover := orphanOfLingeringTree(t, pool, func(*record.Issue) {})

			apply(t, pool, admission, "handover", handover, admission.engine)

			assertCarriedOrphan(t, pool, phase.Reviewing, nil)
			if got := storedPlanAt(t, pool, pathsChild, 2); got != nil {
				t.Fatalf("the carried orphan that ran before the release holds the plan %+v at generation 2, want none", got)
			}
			if got, want := workerStarts(t, pool, pathsChild), []string{"reviewer/reviewing@2"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("the worker starts of the carried orphan = %v, want %v", got, want)
			}
		})
	})
}

// A plan given at generation 0 for an issue the daemon never recorded waits through its giver's
// tree leaving the workflow: the root closes, its linger ends, and a person sets it todo
// again. The rows at generation 0 are still there, because the token of a tree's architect comes
// from the key of its root and a re-admitted root has the same one, and the re-admitted root's
// release of the issue starts it at implementing, the first step of that plan, with no notice of
// a plan not taken. The control: the same release of an issue its owner gave no plan starts
// planning on the default steps.
func TestAPlanGivenBeforeTheIssueIsRecordedWaitsThroughTheRootsReadmission(t *testing.T) {
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
			lingeringRoot(t, pool)
			var waiting *record.StoredPlan
			if tc.given {
				waiting = seedOwnersPlan(t, pool, pathsChild, pathsRoot, 0, record.PlanTask{State: record.TaskWaiting})
			}
			if _, err := intake.ApplyFact(context.Background(), pool, "timer", "linger-ends", intake.LingerExpired{Issue: pathsRoot, Generation: 1},
				admission.engine, admission); err != nil {
				t.Fatalf("apply the end of the linger: %v", err)
			}
			closes := 0
			for _, got := range effects(t, pool) {
				if request, ok := got.payload.(record.SuperviseRequest); ok && got.issue == pathsRoot && request.Op == "tree_close" && request.Linger == 1 {
					closes++
				}
			}
			if closes != len(claim.Roles) {
				t.Fatalf("the end of the linger of %s queued %d tree closes, want one for each role (%d): the fixture must close the tree", pathsRoot, closes, len(claim.Roles))
			}
			readmitAndOpen(t, pool, admission)
			assertPlanUnchanged(t, pool, pathsChild, 0, waiting)
			if maybeIssue(t, pool, pathsChild) != nil {
				t.Fatalf("%s is recorded before its release, want it unrecorded", pathsChild)
			}

			// The architect's release_children writes todo on the issue (api's enterTodoChild).
			ownTodoOnChild(t, pool, admission, "release")

			if !tc.given {
				assertTakesDefaultSteps(t, pool, pathsChild, 2)
				return
			}
			assertTakesOwnersPlan(t, pool, pathsChild, pathsRoot, 2)
			assertPlanUnchanged(t, pool, pathsChild, 0, waiting)
			for _, got := range effects(t, pool) {
				if notice, ok := got.payload.(record.Notice); ok && notice.Kind == "plan-not-taken" {
					t.Fatalf("the release told %+v, want no plan passed over", notice)
				}
			}
		})
	}
}
