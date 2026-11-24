package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// walkedWrite is one write of a step record as these tests compare it: a start or an end, the
// task, the kind and the place, the role and the type, the end's result and the step the work
// returns to (0 for none: every return of these tests goes to a step, from place 1), and the
// count of starts its id is computed from (StepID).
type walkedWrite struct {
	start      bool
	task, kind string
	step       int
	role, typ  string
	result     string
	returnedTo int
	attempt    int64
}

// assertStepWrites fails unless the step record rows key's outbox holds are want, in order, each
// carrying the id StepID gives its issue, run, task, kind, place and count of starts: so a start
// and the end of its run, which want gives one count, have one id.
func assertStepWrites(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, want []walkedWrite) {
	t.Helper()
	writes := stepWrites(t, pool, key)
	got := make([]walkedWrite, len(writes))
	for i, w := range writes {
		got[i] = walkedWrite{start: isStart(w), task: w.Task, kind: w.Kind, step: w.Step, role: w.Role, typ: w.Type, result: resultOf(w)}
		if w.ReturnedTo != nil {
			got[i].returnedTo = *w.ReturnedTo
		}
		if i < len(want) {
			got[i].attempt = want[i].attempt
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the step record writes of %s =\n%s\nwant\n%s", key, describeWrites(got), describeWrites(want))
	}
	for i, w := range writes {
		if isStart(w) == isEnd(w) {
			t.Fatalf("step record write %d of %s = %+v, want a start or an end", i, key, w)
		}
		if id := StepID(key, generation, record.StepPlace{Task: want[i].task, Kind: want[i].kind, Place: want[i].step}, want[i].attempt); w.ID != id {
			t.Fatalf("step record write %d of %s (%+v) has id %s, want %s: the id of start %d of %s/%s/%d",
				i, key, got[i], w.ID, id, want[i].attempt, want[i].task, want[i].kind, want[i].step)
		}
	}
}

func describeWrites(writes []walkedWrite) string {
	text := ""
	for _, w := range writes {
		text += fmt.Sprintf("  %+v\n", w)
	}
	return text
}

// stepStartCounts is the count of starts step_starts holds for each place of key, as
// `task/kind/place=starts`, in the order of the place.
func stepStartCounts(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select task, kind, place, starts from step_starts where issue = $1 order by task, kind, place`, key)
	if err != nil {
		t.Fatalf("read the starts of %s: %v", key, err)
	}
	defer rows.Close()
	var counts []string
	for rows.Next() {
		var task, kind string
		var place int
		var starts int64
		if err := rows.Scan(&task, &kind, &place, &starts); err != nil {
			t.Fatalf("scan a start count of %s: %v", key, err)
		}
		counts = append(counts, fmt.Sprintf("%s/%s/%d=%d", task, kind, place, starts))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the starts of %s: %v", key, err)
	}
	return counts
}

// The step records of an issue whose owner gave a plan are the plan's: each names the
// plan's task, the step's place in it, from 1, and the role and the type the plan's rows record
// for it, and the retro and the merge are that task's too, at place 0. A start and the end of its
// run have one id, computed from the count of starts of that place (step_starts), which each start
// raises. A step the plan does not hold has no record. A task its owner stops ends the record of
// its running step stopped. An issue with no plan rows records today's steps under the task
// default, at their places in today's line, with the ids they always had.
func TestTheRecordsOfAnOwnersPlanAreItsTaskAndSteps(t *testing.T) {
	implementer := func(attempt int64) walkedWrite {
		return walkedWrite{start: true, task: "a", kind: record.PlaceStep, step: 1, role: "implementer", typ: "implementation", attempt: attempt}
	}
	reviewer := func(attempt int64) walkedWrite {
		return walkedWrite{start: true, task: "a", kind: record.PlaceStep, step: 2, role: "reviewer", typ: "review", attempt: attempt}
	}
	ended := func(start walkedWrite, result string, returnedTo int) walkedWrite {
		start.start, start.result, start.returnedTo = false, result, returnedTo
		return start
	}

	t.Run("the plan implementer reviewer with one rejected review, to its merge", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		reviewRequestsChanges(t, pool, engine, root.Key)
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: root.Key, Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/LEGION-208", HeadSHA: "head", MergeSHA: "merge", Failing: []string{}})
		move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})

		retro := walkedWrite{start: true, task: "a", kind: record.PlaceRetro, role: "implementer", attempt: 1}
		merge := walkedWrite{start: true, task: "a", kind: record.PlaceMerge, role: "merger", attempt: 1}
		assertStepWrites(t, pool, root.Key, root.Generation, []walkedWrite{
			implementer(1), ended(implementer(1), "passed", 0),
			reviewer(1), ended(reviewer(1), "failed", 1),
			implementer(2), ended(implementer(2), "passed", 0),
			reviewer(2), ended(reviewer(2), "passed", 0),
			retro, ended(retro, "passed", 0),
			merge, ended(merge, "passed", 0),
		})
		if got, want := stepStartCounts(t, pool, root.Key), []string{"a/merge/0=1", "a/retro/0=1", "a/step/1=2", "a/step/2=2"}; !slices.Equal(got, want) {
			t.Fatalf("the starts of %s = %v, want %v: each place of task a counted, and no place of the task default", root.Key, got, want)
		}
		if got := issueAt(t, pool, root.Key); got != phase.Done {
			t.Fatalf("%s stands at %s, want done after its merge", root.Key, got)
		}
	})

	t.Run("a stopped task's running record ends stopped", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}
		assertStepWrites(t, pool, root.Key, root.Generation, []walkedWrite{
			implementer(1), ended(implementer(1), "passed", 0),
			reviewer(1), ended(reviewer(1), "stopped", 0),
		})
	})

	t.Run("the retro's rating reaches the record of the task's retro", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
		applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer,
			Claim: workerOr(t, pool, root.Key, claim.RoleImplementer, "implement-claim"), Summary: "retro done", Commit: "head", PlanRating: &record.PlanRating{Rating: 2, Reason: "The plan held no tester."}})
		var rated []record.StepRecord
		for _, write := range stepWrites(t, pool, root.Key) {
			if write.PlanRating != nil {
				rated = append(rated, write)
			}
		}
		if want := StepID(root.Key, 1, record.StepPlace{Task: "a", Kind: record.PlaceRetro}, 1); len(rated) != 1 || rated[0].ID != want || rated[0].Task != "a" ||
			rated[0].Kind != record.PlaceRetro || rated[0].Role != "implementer" {
			t.Fatalf("the ratings = %+v, want one on the record %s of the first run of task a's retro", rated, want)
		}
	})

	t.Run("an issue with no plan rows records today's steps of the task default", func(t *testing.T) {
		assertNoPlanRowsRunTodaysSteps(t, returnEngine())
	})
}

// assertNoPlanRowsRunTodaysSteps fails unless an issue with no plan rows that engine moves out of
// implementing records today's steps of the task default at today's places: its
// implementer's end at step 2 and its tester's start at step 3. The implementer's run started
// under no count of this release, so the daemon's boot before the move opens it at the count 0
// (OpenUncountedRuns) and its end is the record of count 0; the tester's start is the first of its
// place.
func assertNoPlanRowsRunTodaysSteps(t *testing.T, engine *Engine) {
	t.Helper()
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Implementing)
	move(t, pool, engine, "LEGION-208", TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
	assertStepWrites(t, pool, "LEGION-208", 1, []walkedWrite{
		{task: defaultTask, kind: record.PlaceStep, step: 2, role: "implementer", typ: "implementation", result: "passed", attempt: 0},
		{start: true, task: defaultTask, kind: record.PlaceStep, step: 3, role: "tester", typ: "testing", attempt: 1},
	})
	if got, want := stepStartCounts(t, pool, "LEGION-208"), []string{"default/step/2=0", "default/step/3=1"}; !slices.Equal(got, want) {
		t.Fatalf("the starts of LEGION-208 = %v, want %v", got, want)
	}
}

// stepOfRow is the step role's phases row of key names (record.PhaseRow.StepTask, StepPlace), as
// `task/place`, and the row's entry, which says the row was raised at all.
func stepOfRow(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role) (string, int64) {
	t.Helper()
	row := roleRowOf(t, pool, engine, key, role)
	return fmt.Sprintf("%s/%d", row.StepTask, row.StepPlace), row.Entry
}

// assertRowsName fails unless each role's phases row of key was raised and names the step want
// gives it, as `task/place`.
func assertRowsName(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, when string, want map[claim.Role]string) {
	t.Helper()
	for role, step := range want {
		if got, entry := stepOfRow(t, pool, engine, key, role); got != step || entry == 0 {
			t.Fatalf("%s, the %s's row of %s names the step %q at entry %d, want %q on a raised entry", when, role, key, got, entry, step)
		}
	}
}

// Each phases row of an issue with plan rows names the step its role runs: the task and
// the step's place in it, written by the start that raises the row's entry (enter), so the facts
// of that row are the facts of that step's run. The implementer's row names its step 1, and the
// retro's start raises a row of the implementer's own beside it, at the retro's place 0, which the
// engine reads from then on; the merger's names the merge, at 0.
// An issue with no plan rows keeps no task and place 0 on each row, as it always did.
func TestEachPhaseRowNamesItsStep(t *testing.T) {
	t.Run("the plan implementer reviewer", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		assertRowsName(t, pool, engine, root.Key, "after the implementer's start", map[claim.Role]string{claim.RoleImplementer: "a/1"})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		assertRowsName(t, pool, engine, root.Key, "after the reviewer's start",
			map[claim.Role]string{claim.RoleReviewer: "a/2", claim.RoleImplementer: "a/1"})
		move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
		assertRowsName(t, pool, engine, root.Key, "at the retro",
			map[claim.Role]string{claim.RoleImplementer: "a/0", claim.RoleReviewer: "a/2"})
		move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})
		assertPhase(t, pool, phase.Merging)
		assertRowsName(t, pool, engine, root.Key, "at the merge", map[claim.Role]string{claim.RoleMerger: "a/0", claim.RoleImplementer: "a/0"})
	})

	t.Run("an issue with no plan rows keeps no step on each row", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerSignOff, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})
		assertPhase(t, pool, phase.Merging)
		assertRowsName(t, pool, engine, root.Key, "at the merge of an issue with no plan rows", map[claim.Role]string{
			claim.RoleTester: "/0", claim.RoleReviewer: "/0", claim.RoleImplementer: "/0", claim.RoleMerger: "/0"})
	})
}

// The start and the end of one run of a step of an owner's plan are two writes, and a boot of the
// daemon between them rebuilds the engine on the same store. They meet in one record only
// because both compute one id, and the count of starts is what carries it: the one count of the
// place, in step_starts, raised by the start and read unchanged by the end. So the reviewer's
// start, before the boot, and its end, after it, both carry the id of the first start of task a's
// step 2; with that count set to 0 between them the end computes another id.
func TestTheStartAndTheEndOfARunHaveOneIDAcrossABoot(t *testing.T) {
	const key = "LEGION-208"
	run := func(t *testing.T, between func(pool *pgxpool.Pool)) (start, end record.StepRecord) {
		pool := migratedPool(t)
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, key, root.Generation, "implementer", "reviewer")
		move(t, pool, returnEngine(), key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, returnEngine(), key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		between(pool)
		// The boot: a new engine on a new store over the same database.
		move(t, pool, returnEngine(), key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
		for _, w := range stepWrites(t, pool, key) {
			if w.Task != "a" || w.Kind != record.PlaceStep || w.Step != 2 {
				continue
			}
			if isStart(w) {
				start = w
			}
			if isEnd(w) {
				end = w
			}
		}
		return start, end
	}
	want := StepID(key, 1, record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: 2}, 1)
	start, end := run(t, func(*pgxpool.Pool) {})
	if start.ID != want || end.ID != want || resultOf(end) != "passed" {
		t.Fatalf("the reviewer's start has id %q and its end, after the boot, %q (%s); want both %s, the first start of a/step/2",
			start.ID, end.ID, resultOf(end), want)
	}
	t.Run("control: the count of the place set to 0 between the start and the end", func(t *testing.T) {
		start, end := run(t, func(pool *pgxpool.Pool) {
			if _, err := pool.Exec(t.Context(), `update step_starts set starts = 0 where issue = $1 and task = 'a' and kind = 'step' and place = 2`, key); err != nil {
				t.Fatalf("set the count of a/step/2 to 0: %v", err)
			}
		})
		if start.ID != want || end.ID == "" || end.ID == start.ID {
			t.Fatalf("with the count set to 0 between the start and the end, the start has id %q and the end %q; the count is what carries the id, so the end must have another",
				start.ID, end.ID)
		}
	})
}

// assertPlanNodes fails unless the plan node rows key's outbox holds are want, in the order they
// were queued, saying when.
func assertPlanNodes(t *testing.T, pool *pgxpool.Pool, key, when string, want ...record.PlanRecord) {
	t.Helper()
	if got := planWrites(t, pool, key); !reflect.DeepEqual(append([]record.PlanRecord{}, got...), append([]record.PlanRecord{}, want...)) {
		t.Fatalf("%s: the plan nodes of %s = %+v, want %+v", when, key, got, want)
	}
}

// ownersNode is the plan node of LEGION-208's run 1 on version of the owner's plan whose one
// task, a, runs the implementer and the reviewer.
func ownersNode(version int) record.PlanRecord {
	return record.PlanRecord{ID: PlanID("LEGION-208", 1), Generation: 1, Source: record.PlanSourceOwner,
		PlanDocument: &record.PlanDocument{ArtifactID: planArtifact, Version: version},
		Tasks:        []record.PlanRecordTask{{ID: "a", Steps: []string{"implementer", "reviewer"}}}}
}

// The plan node of a run is the plan its walk holds. An owner's plan given before the
// work starts is the node the start writes: the source owner, the document, version 1 and the
// task a with its steps; a version given before the start writes no node of its own, since the
// start writes the version then in force. A version 2 taken while a runs writes the node again,
// whole, at version 2, under the run's one id. A version that replaces the stopped task default
// with a writes the node of a. An issue whose owner gave no plan writes the source default, with
// no document and no version, and the project's default_steps as the task default.
func TestThePlanNodeIsTheOwnersAndIsWrittenAgainAtEachVersionTaken(t *testing.T) {
	t.Run("the owner's plan: version 1 at the start, and version 2 again when it is taken", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)
		task := givenTask(t, "Build the widget.", "implementer", "reviewer")
		mustTake(t, "version 1 before the work starts", apply(planID(root.Key, root.Generation, 1), ownersPlan(t, root.Key, root.Generation, 1, task)))
		assertPlanNodes(t, pool, root.Key, "after version 1, before the work starts")
		firstStep(t, pool, engine, root.Key)
		assertPlanNodes(t, pool, root.Key, "at the start", ownersNode(1))
		mustTake(t, "version 2 while a runs, which changes no task", apply(planID(root.Key, root.Generation, 2), ownersPlan(t, root.Key, root.Generation, 2, task)))
		assertPlanNodes(t, pool, root.Key, "after version 2", ownersNode(1), ownersNode(2))
	})

	t.Run("a version that replaces the stopped task default writes the node of a", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)
		firstStep(t, pool, engine, root.Key)
		defaults := record.PlanRecord{ID: PlanID(root.Key, 1), Generation: 1, Source: record.PlanSourceDefault,
			Tasks: []record.PlanRecordTask{{ID: plan.DefaultTask, Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}}}}
		assertPlanNodes(t, pool, root.Key, "at the start on the default steps", defaults)
		if result := apply("stop", ownersStop(t, root, plan.DefaultTask)); result.Refusal != nil {
			t.Fatalf("the owner's stop of default was refused: %+v", result.Refusal)
		}
		mustTake(t, "version 1, which replaces default with a", apply(planID(root.Key, root.Generation, 1),
			ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		assertPlanNodes(t, pool, root.Key, "after version 1", defaults, ownersNode(1))
	})

	t.Run("an issue whose owner gave no plan: the source default with the project's default_steps", func(t *testing.T) {
		pool := migratedPool(t)
		engine := engineRunningByDefault(t, "implementer", "reviewer")
		root := stepRoot(t, pool, phase.Admitted)
		firstStep(t, pool, engine, root.Key)
		assertPlanNodes(t, pool, root.Key, "at the start", record.PlanRecord{ID: PlanID(root.Key, 1), Generation: 1, Source: record.PlanSourceDefault,
			Tasks: []record.PlanRecordTask{{ID: plan.DefaultTask, Steps: []string{"implementer", "reviewer"}}}})
	})
}

// The default steps are stated in one place, the project's setting (config.Project.RunsByDefault,
// which the daemon wires as Config.DefaultSteps). With default_steps [implementer, reviewer], the
// plan node an issue whose owner gave no plan writes at its first step holds exactly the one task
// default of those two steps, so a list the package held of its own would name other steps there.
// An issue with no plan rows runs today's steps at today's places whatever the project names:
// under the same setting its implementer is step 2 and its tester step 3 of the task default.
func TestThereIsOneDefinitionOfTheDefaultSteps(t *testing.T) {
	t.Run("the plan node holds the project's default_steps", func(t *testing.T) {
		pool := migratedPool(t)
		engine := engineRunningByDefault(t, "implementer", "reviewer")
		root := stepRoot(t, pool, phase.Admitted)
		firstStep(t, pool, engine, root.Key)
		rows := issueOutboxOf(t, pool, root.Key, string(record.OutboxKindPlanRecord))
		if len(rows) != 1 {
			t.Fatalf("the first step queued %d plan nodes, want 1", len(rows))
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rows[0].payload, &fields); err != nil {
			t.Fatalf("decode the plan node: %v", err)
		}
		var tasks bytes.Buffer
		if err := json.Compact(&tasks, fields["tasks"]); err != nil {
			t.Fatalf("compact the plan node's tasks %q: %v", fields["tasks"], err)
		}
		if want := `[{"id":"default","steps":["implementer","reviewer"]}]`; tasks.String() != want {
			t.Fatalf("the plan node's tasks = %s, want %s", tasks.String(), want)
		}
	})

	t.Run("an issue with no plan rows keeps today's places under the project's default_steps", func(t *testing.T) {
		assertNoPlanRowsRunTodaysSteps(t, engineRunningByDefault(t, "implementer", "reviewer"))
	})
}
