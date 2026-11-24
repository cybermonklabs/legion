package workflow

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedAToE writes on key the plan of tasks a to e of one step each, each step its own agent's key
// (`implementer:a` … `tester:e`; this release names one fresh agent of each role, record.NameAgents,
// so each task's step carries a key of its own): c needs b, d needs a and c, and e needs d, so e is
// the task that merges. a and c run at their step, b is done, and d and e wait. a works on a branch
// of its own, legion/<key>.a, where its pull request #44 is open.
func seedAToE(t *testing.T, pool *pgxpool.Pool, root record.Issue) {
	t.Helper()
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, needs: []string{"b"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "d", steps: []string{"implementer:d"}, agents: []claim.Agent{"impl-d"}, needs: []string{"a", "c"}, state: record.TaskWaiting},
		planTask{id: "e", steps: []string{"tester:e"}, agents: []claim.Agent{"test-e"}, needs: []string{"d"}, state: record.TaskWaiting})
	seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key+".a")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 44,
		Branch: "legion/" + root.Key + ".a", HeadSHA: "head-a", Failing: []string{}})
}

// A stop holds only the tasks that need the stopped task: of the plan a to e, the owner's stop of c
// while a runs stops c and the step of c's agent, a goes on, d and e, which need c, stay waiting,
// and the issue is not held, since a runs. The owner is told the stop, and which tasks wait on it:
// `Tasks d, e need it and wait; each other task goes on.` Once a is done nothing runs, and the
// issue is held from c's phase for the stopped task.
//
// The fault it catches is a stop that halts every task of the issue (its control: the stop of a
// task of a plan of several holding the issue, as a plan of one task's stop does, which leaves the
// issue held while a runs).
func TestAStoppedTaskHoldsOnlyTheTasksThatNeedIt(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedAToE(t, pool, root)

	stop := ownersStop(t, root, "c")
	if result := applyFacts(t, pool, engine)("stop-c", stop); result.Refusal != nil {
		t.Fatalf("the owner's stop of c was refused: %+v", result.Refusal)
	}

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "c", record.TaskStopped, atStep(1))
	assertTask(t, tasks, "d", record.TaskWaiting, record.TaskPlace{})
	assertTask(t, tasks, "e", record.TaskWaiting, record.TaskPlace{})
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil {
		t.Fatalf("%s after c's stop stands in %s (hold %+v), want implementing, where a runs, and no hold", root.Key, issue.Phase, issue.Hold)
	}
	if stops := superviseRowsOf(t, pool, root.Key, "step_stop"); len(stops) != 1 || stops[0].Role != claim.RoleImplementer ||
		stops[0].Claim != agentClaim(t, root.Key, "impl-c") {
		t.Fatalf("the step stops of %s = %+v, want the one of c's agent impl-c", root.Key, stops)
	}
	notices := noticesOf(t, pool, root.Key, "task-stopped")
	if len(notices) != 1 || !strings.HasSuffix(notices[0].Reason, " Tasks d, e need it and wait; each other task goes on.") ||
		!strings.HasPrefix(notices[0].Reason, "task c of LEGION-208 was stopped by its owner") || notices[0].Role != claim.RoleImplementer {
		t.Fatalf("the task-stopped notices of %s = %+v, want the one of c's stop naming d and e, which wait on it", root.Key, notices)
	}

	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")

	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskDone, atStep(1))
	assertTask(t, tasks, "d", record.TaskWaiting, record.TaskPlace{})
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Held || issue.Hold == nil ||
		*issue.Hold != (record.Hold{From: phase.Implementing, Reason: record.HoldTaskStopped}) {
		t.Fatalf("%s once a is done stands in %s (hold %+v), want held from implementing, c's phase, for the stopped task", root.Key, issue.Phase, issue.Hold)
	}
}

// stepWords is the stored steps of task of key's plan at its generation, each `role:key`.
func stepWords(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string) []string {
	t.Helper()
	issue := issueOf(t, pool, engine, key)
	var words []string
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := engine.store.Plan(ctx, tx, key, issue.Generation)
		if err != nil || stored == nil {
			return err
		}
		for _, step := range stored.Steps[task] {
			words = append(words, step.Role+":"+step.AgentKey)
		}
		return nil
	})
	return words
}

// givenBranches is each branch name the generation of key gave to a task (plan_branches).
func givenBranches(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select branch from plan_branches where issue = $1 order by branch", key)
	if err != nil {
		t.Fatalf("read the branches %s gave: %v", key, err)
	}
	defer rows.Close()
	var branches []string
	for rows.Next() {
		var branch string
		if err := rows.Scan(&branch); err != nil {
			t.Fatalf("read the branches %s gave: %v", key, err)
		}
		branches = append(branches, branch)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the branches %s gave: %v", key, err)
	}
	return branches
}

// keyed is the plan_task block of task id as a version gives it: "Do <id>." with text added, its
// steps as `role:key` words, and needs.
func keyed(t *testing.T, id, text string, words []string, needs ...string) plan.Task {
	t.Helper()
	steps, err := plan.ParseSteps(strings.Join(words, " "))
	if err != nil {
		t.Fatalf("the steps %v: %v", words, err)
	}
	return plan.Task{ID: id, Text: "Do " + id + "." + text, Needs: needs, Steps: steps}
}

// A later version adds, changes and removes tasks by the state of each. Of tasks a (`implementer
// reviewer`, running), g (`implementer`, needs a), k (`tester`, needs a) and z (`implementer`,
// needs g and k, the task that merges), each step its own agent's key, version 2 adds f
// (`implementer`, needing nothing, which z now needs too), changes g's steps to `implementer
// reviewer` and removes k: it is taken, f runs in the same transaction, g waits with its two steps
// and k is gone, never started. Version 3, which changes the running a, is refused with one reason,
// and a keeps the steps it started with. When a is done, g runs its first changed step. A version
// that changes g while g is held is refused. A version that adds s, which runs, then a version that
// removes the stopped s, then one that adds s back, are each taken: s is a new task, running with
// its counts at 0, on a branch the generation gave to no task before.
//
// The fault it catches is a version that changes what runs or is held, or that loses a task it
// adds or changes (its controls: version 3 changing only the waiting g, which is taken, so the
// assertion of the refusal fails; and planGiven taking the change of a held task, which the
// refusal of the held g then fails).
func TestALaterVersionAddsChangesAndRemovesTasks(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a", "reviewer:ar"}, agents: []claim.Agent{"impl-a", "rev-a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "g", steps: []string{"implementer:g"}, agents: []claim.Agent{"impl-g"}, needs: []string{"a"}, state: record.TaskWaiting},
		planTask{id: "k", steps: []string{"tester:k"}, agents: []claim.Agent{"test-k"}, needs: []string{"a"}, state: record.TaskWaiting},
		planTask{id: "z", steps: []string{"implementer:z"}, agents: []claim.Agent{"impl-z"}, needs: []string{"g", "k"}, state: record.TaskWaiting})
	apply := applyFacts(t, pool, engine)
	aBlock := keyed(t, "a", "", []string{"implementer:a", "reviewer:ar"})
	gChanged := keyed(t, "g", "", []string{"implementer:g", "reviewer:gr"}, "a")
	fBlock := keyed(t, "f", "", []string{"implementer:f"})

	mustTake(t, "version 2", apply("version-2", laterVersion(t, root.Key, root.Generation, 2,
		aBlock, fBlock, gChanged, keyed(t, "z", "", []string{"implementer:z"}, "g", "f"))))
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "f", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "g", record.TaskWaiting, record.TaskPlace{})
	if _, ok := tasks["k"]; ok {
		t.Fatalf("the plan of %s holds k after version 2 removed it", root.Key)
	}
	if got := stepWords(t, pool, engine, root.Key, "g"); !slices.Equal(got, []string{"implementer:g", "reviewer:gr"}) {
		t.Fatalf("task g's steps after version 2 = %v, want implementer:g reviewer:gr", got)
	}
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleTester); len(starts) != 0 {
		t.Fatalf("the tester's starts of %s = %+v, want none: k never started", root.Key, starts)
	}

	result := apply("version-3", laterVersion(t, root.Key, root.Generation, 3,
		keyed(t, "a", "", []string{"implementer:a", "tester:at"}), fBlock, gChanged, keyed(t, "z", "", []string{"implementer:z"}, "g", "f")))
	if result.Refusal == nil || !reflect.DeepEqual(result.Refusal.Reasons, []plan.Refusal{{Task: "a", Reason: "task a is running: stop it with stop_task before you change it."}}) {
		t.Fatalf("version 3, which changes the running a, = %+v, want refused with the one reason of a", result)
	}
	if got := stepWords(t, pool, engine, root.Key, "a"); !slices.Equal(got, []string{"implementer:a", "reviewer:ar"}) {
		t.Fatalf("task a's steps after version 3 was refused = %v, want the steps it started with", got)
	}

	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
	taskMoves(t, pool, engine, root.Key, "a", TriggerReviewApproved, "")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskDone, atStep(2))
	assertTask(t, tasks, "g", record.TaskRunning, atStep(1))

	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().HoldTask(t.Context(), tx, root.Key, root.Generation, "g", "")
	})
	result = apply("version-3-held", laterVersion(t, root.Key, root.Generation, 3,
		aBlock, fBlock, keyed(t, "g", " Again.", []string{"implementer:g", "reviewer:gr"}, "a"), keyed(t, "z", "", []string{"implementer:z"}, "g", "f")))
	heldWant := []plan.Refusal{{Task: "g", Reason: "task g is held at step 1 (implementer): retry or escalate its hold with retry_or_escalate, or stop it with stop_task, before you change it."}}
	if result.Refusal == nil || !reflect.DeepEqual(result.Refusal.Reasons, heldWant) {
		t.Fatalf("version 3, which changes the held g, = %+v, want refused with %+v", result, heldWant)
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().ReleaseTask(t.Context(), tx, root.Key, root.Generation, "g")
	})

	// s is a tester's step: this release names one fresh agent of each role of an issue
	// (record.NameAgents), and f's fresh implementer holds the implementer's name.
	sBlock := keyed(t, "s", "", []string{"tester:s"})
	mustTake(t, "version 4", apply("version-4", laterVersion(t, root.Key, root.Generation, 4,
		aBlock, fBlock, gChanged, sBlock, keyed(t, "z", "", []string{"implementer:z"}, "g", "f", "s"))))
	if first := tasksOf(t, pool, engine, root.Key)["s"]; first.State != record.TaskRunning || first.Branch == "" {
		t.Fatalf("task s after version 4 = %+v, want running on a branch of its own", first)
	}
	if result := apply("stop-s", ownersStop(t, root, "s")); result.Refusal != nil {
		t.Fatalf("the owner's stop of s was refused: %+v", result.Refusal)
	}
	mustTake(t, "version 5", apply("version-5", laterVersion(t, root.Key, root.Generation, 5,
		aBlock, fBlock, gChanged, keyed(t, "z", "", []string{"implementer:z"}, "g", "f"))))
	if _, ok := tasksOf(t, pool, engine, root.Key)["s"]; ok {
		t.Fatalf("the plan of %s holds s after version 5 removed it", root.Key)
	}
	given := givenBranches(t, pool, root.Key)
	mustTake(t, "version 6", apply("version-6", laterVersion(t, root.Key, root.Generation, 6,
		aBlock, fBlock, gChanged, sBlock, keyed(t, "z", "", []string{"implementer:z"}, "g", "f", "s"))))
	again := tasksOf(t, pool, engine, root.Key)["s"]
	if again.State != record.TaskRunning || again.Counts.Pass != 0 || again.Counts.Rounds != 0 || again.Branch == "" || slices.Contains(given, again.Branch) {
		t.Fatalf("task s added back = %+v (branches given before: %v), want a new task running with counts of 0 on a branch no task had", again, given)
	}
}

// A version that changes the stopped task of a held issue starts that task again at its first
// step, where its needs are done, and the issue's hold goes: of the plan a to e, c stopped and a
// done, so nothing runs and the issue is held for the stop, a version that changes c starts c
// again at its step and the issue stands in implementing.
//
// The fault it catches is a held issue that a change of its stopped task leaves held.
func TestAVersionThatChangesTheStoppedTaskStartsItAgain(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedAToE(t, pool, root)
	if result := applyFacts(t, pool, engine)("stop-c", ownersStop(t, root, "c")); result.Refusal != nil {
		t.Fatalf("the owner's stop of c was refused: %+v", result.Refusal)
	}
	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Held {
		t.Fatalf("premise: %s once a is done stands in %s, want held for the stopped c", root.Key, issue.Phase)
	}

	mustTake(t, "version 2", applyFacts(t, pool, engine)("version-2", laterVersion(t, root.Key, root.Generation, 2,
		keyed(t, "a", "", []string{"implementer:a"}), keyed(t, "b", "", []string{"implementer:b"}), keyed(t, "c", " Again.", []string{"implementer:c"}, "b"),
		keyed(t, "d", "", []string{"implementer:d"}, "a", "c"), keyed(t, "e", "", []string{"tester:e"}, "d"))))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "c", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "d", record.TaskWaiting, record.TaskPlace{})
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil {
		t.Fatalf("%s after version 2 changed the stopped c stands in %s (hold %+v), want implementing with no hold", root.Key, issue.Phase, issue.Hold)
	}
}

// A version that adds a need of the task that merges is refused while that task stands at its
// retro or its merge, where the work of the issue merges: of the plan a to e at e's retro, a version
// that adds h, needing e, is refused with one reason of h.
//
// The fault it catches is a need on the work the issue merges once it is merging (its control:
// planGiven without the refusal, which takes the version).
func TestAVersionThatNeedsTheMergingTaskAtItsRetroIsRefused(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Retro)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, needs: []string{"b"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "d", steps: []string{"implementer:d"}, agents: []claim.Agent{"impl-d"}, needs: []string{"a", "c"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "e", steps: []string{"tester:e"}, agents: []claim.Agent{"test-e"}, needs: []string{"d"}, state: record.TaskRunning,
			place: record.TaskPlace{Kind: record.PlaceRetro}})
	result := applyFacts(t, pool, engine)("version-2", laterVersion(t, root.Key, root.Generation, 2,
		keyed(t, "a", "", []string{"implementer:a"}), keyed(t, "b", "", []string{"implementer:b"}), keyed(t, "c", "", []string{"implementer:c"}, "b"),
		keyed(t, "d", "", []string{"implementer:d"}, "a", "c"), keyed(t, "e", "", []string{"tester:e"}, "d"), keyed(t, "h", "", []string{"implementer:h"}, "e")))
	want := []plan.Refusal{{Task: "h", Reason: "task h needs task e, which stands at its retro, where the work of the issue merges. This release of Legion does not carry it out yet."}}
	if result.Refusal == nil || !reflect.DeepEqual(result.Refusal.Reasons, want) {
		t.Fatalf("the version that adds h, needing e at its retro, = %+v, want refused with %+v", result, want)
	}
	if _, ok := tasksOf(t, pool, engine, root.Key)["h"]; ok {
		t.Fatalf("the plan of %s holds h after its version was refused", root.Key)
	}
}

// toldABase is what an implementation step is told of the base of its task's pull request where
// that base is not the issue's default branch (pullRequestBase).
const toldABase = "Open the pull request of this task against"

// cNeedsAB are the blocks of a version holding a (`implementer:a`), b (`implementer:b`) and c
// (`planner:cp implementer:c`, needing a and b), as seedTasks writes them, and then extra.
func cNeedsAB(t *testing.T, extra ...plan.Task) []plan.Task {
	t.Helper()
	return append([]plan.Task{keyed(t, "a", "", []string{"implementer:a"}), keyed(t, "b", "", []string{"implementer:b"}),
		keyed(t, "c", "", []string{"planner:cp", "implementer:c"}, "a", "b")}, extra...)
}

// A version that moves the base of a task that has started and is not stopped is refused: of tasks
// a and b, which are done, and c, which needs both and is the task that merges, standing at its
// first step on its own branch legion/LEGION-208.c, so its pull request is based on main, a version
// that adds d, needing c, would base c on its join branch legion/LEGION-208.c.join, which c's start
// as the task that merges never made. It is refused with one reason of c, which names both bases,
// d is not added, and c's implementation step is told no base. The same holds while c is held at
// its first step.
//
// The fault it catches is a version that moves the base of a running task, whose later steps are
// then told a branch that does not exist (its control: planGiven without the refusal, which takes
// the version, and c's implementation step is told legion/LEGION-208.c.join).
func TestAVersionThatMovesTheBaseOfAStartedTaskIsRefused(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "held"}[held], func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Planning)
			seedTasks(t, pool, root.Key, root.Generation,
				planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskDone, place: atStep(1)},
				planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskDone, place: atStep(1)},
				planTask{id: "c", steps: []string{"planner:cp", "implementer:c"}, agents: []claim.Agent{"plan-c", "impl-c"}, needs: []string{"a", "b"},
					state: record.TaskRunning, place: atStep(1)})
			seedTaskBranch(t, pool, root.Key, "c", "legion/"+root.Key+".c")
			if held {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					return record.NewStore().HoldTask(t.Context(), tx, root.Key, root.Generation, "c", "")
				})
			}

			result := applyFacts(t, pool, engine)("version-2", laterVersion(t, root.Key, root.Generation, 2,
				cNeedsAB(t, keyed(t, "d", "", []string{"tester:d"}, "c"))...))
			want := []plan.Refusal{{Task: "c", Reason: "task c has started on the base main, and this version would base it on legion/LEGION-208.c.join: " +
				"stop it with stop_task before you change its base."}}
			if result.Refusal == nil || !reflect.DeepEqual(result.Refusal.Reasons, want) {
				t.Errorf("the version that adds d, needing the started c, = %+v, want refused with %+v", result, want)
			}
			if _, ok := tasksOf(t, pool, engine, root.Key)["d"]; ok {
				t.Errorf("the plan of %s holds d after its version was refused", root.Key)
			}
			lacks(t, "implementer of c", stepLines(t, pool, engine, root.Key, "c", atStep(2)), toldABase)
		})
	}
}

// A version may move the base of a task that waits, and not of one that has started: of tasks a,
// running at its one step on its own branch legion/LEGION-208.a with its pull request #44, b, done,
// and c, which needs both and is the task that merges, waiting, version 2 adds d, needing c, and is
// taken, since c has not started: c's implementation step is told its join branch. Once a is done c
// starts on its own branch, legion/LEGION-208.c, and version 3, which removes d, would make c the
// task that merges again, based on main: it is refused with one reason of c, which names both
// bases, and d stays.
//
// The fault it catches is a version that moves the merge back to a task that started as no task
// that merges, on its join branch (its control: planGiven without the refusal, which takes version
// 3), and a refusal of a version that moves the base of a task that waits (its control: the refusal
// of every task whatever its state, which refuses version 2).
func TestAVersionMovesTheBaseOfAWaitingTaskAndNotOfAStartedOne(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "c", steps: []string{"planner:cp", "implementer:c"}, agents: []claim.Agent{"plan-c", "impl-c"}, needs: []string{"a", "b"},
			state: record.TaskWaiting})
	seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key+".a")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 44,
		Branch: "legion/" + root.Key + ".a", HeadSHA: "head-a", Failing: []string{}})
	apply := applyFacts(t, pool, engine)
	dBlock := keyed(t, "d", "", []string{"tester:d"}, "c")

	mustTake(t, "version 2, which adds d while c waits", apply("version-2", laterVersion(t, root.Key, root.Generation, 2, cNeedsAB(t, dBlock)...)))
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "c", record.TaskWaiting, record.TaskPlace{})
	assertTask(t, tasks, "d", record.TaskWaiting, record.TaskPlace{})

	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
	c := tasksOf(t, pool, engine, root.Key)["c"]
	if c.State != record.TaskRunning || c.Branch != "legion/"+root.Key+".c" {
		t.Fatalf("task c once a is done = %+v, want running on legion/%s.c", c, root.Key)
	}
	endsWith(t, "implementer of c", stepLines(t, pool, engine, root.Key, "c", atStep(2)),
		" Open the pull request of this task against legion/LEGION-208.c.join: gh pr create --base legion/LEGION-208.c.join.")

	result := apply("version-3", laterVersion(t, root.Key, root.Generation, 3, cNeedsAB(t)...))
	want := []plan.Refusal{{Task: "c", Reason: "task c has started on the base legion/LEGION-208.c.join, and this version would base it on main: " +
		"stop it with stop_task before you change its base."}}
	if result.Refusal == nil || !reflect.DeepEqual(result.Refusal.Reasons, want) {
		t.Errorf("version 3, which removes d while c runs, = %+v, want refused with %+v", result, want)
	}
	if _, ok := tasksOf(t, pool, engine, root.Key)["d"]; !ok {
		t.Errorf("the plan of %s lost d to a version that was refused", root.Key)
	}
}
