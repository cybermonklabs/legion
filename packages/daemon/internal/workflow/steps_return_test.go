package workflow

import (
	"context"
	"reflect"
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
	"github.com/sjawhar/legion/daemon/internal/record/fencetest"
)

// changesBody is the request for changes the review of these tests makes: the reason the review
// gives, which the step it returns the work to is told.
const changesBody = "Name the rollback path."

// returnEngine is stepEngine with the hook a deployment that writes no `hooks:` key runs
// (config.DefaultHooks), as the daemon hands it over (engineHooks in the daemon package), so a
// return fires it as it would in production.
func returnEngine() *Engine {
	engine := stepEngine(nil)
	for _, hook := range config.DefaultHooks() {
		engine.cfg.Hooks = append(engine.cfg.Hooks, Hook{Name: hook.Name, On: hook.On, Moves: hook.Moves, Every: hook.Every,
			Roles: hook.Roles, Prompt: hook.Prompt})
	}
	return engine
}

// seedStepsPlan writes the plan key's owner gave for its run generation: version 2, one task a
// whose steps are words, each `role` or `role:key`, with the type plan.BuiltinRoles gives its
// role, waiting at no act.
func seedStepsPlan(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, words ...string) {
	t.Helper()
	v := record.PlanVersion{Issue: key, Generation: generation, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: claim.Token(treeRoleToken(t, key, claim.RoleArchitect)), GivenAt: stepClock}
	task := record.PlanTask{Issue: key, Generation: generation, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", State: record.TaskWaiting}
	roles := plan.BuiltinRoles()
	steps := make([]record.PlanStep, 0, len(words))
	for i, word := range words {
		role, key, _ := strings.Cut(word, ":")
		steps = append(steps, record.PlanStep{Issue: v.Issue, Generation: generation, Task: "a", Position: i + 1, Role: role,
			AgentKey: key, Type: roles[role].Type})
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutPlan(t.Context(), tx, v, []record.PlanTask{task}, steps)
	})
}

// reviewRequestsChanges ends key's review round, in reviewing, with a request for changes whose
// body is changesBody, as the review's settlement moves it (settleRound).
func reviewRequestsChanges(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		pr, err := engine.store.PullRequest(ctx, tx, key)
		if err != nil {
			return err
		}
		reviewer, err := engine.phaseRow(ctx, tx, key, claim.RoleReviewer)
		if err != nil {
			return err
		}
		return engine.transition(ctx, tx, *issue, TriggerReviewRejected, "", reviewer, pr, changesBody)
	})

}

// issueOf is key as the store holds it now.
func issueOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) record.Issue {
	t.Helper()
	var issue *record.Issue
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = engine.store.Issue(ctx, tx, key)
		return err
	})

	if issue == nil {
		t.Fatalf("the store holds no %s", key)
	}
	return *issue
}

// taskOf is task a of key's plan at its generation as the store holds it now.
func taskOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) record.PlanTask {
	t.Helper()
	issue := issueOf(t, pool, engine, key)
	var stored *record.StoredPlan
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = engine.store.Plan(ctx, tx, key, issue.Generation)
		return err
	})

	if stored == nil || len(stored.Tasks) != 1 {
		t.Fatalf("the plan of %s at generation %d = %+v, want one task", key, issue.Generation, stored)
	}
	return stored.Tasks[0]
}

// roleRowOf is role's phases row of key as the store holds it now.
func roleRowOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role) record.PhaseRow {
	t.Helper()
	var row record.PhaseRow
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = engine.phaseRow(ctx, tx, key, role)
		return err
	})

	return row
}

// startsOf is every start of role key's outbox holds, oldest first.
func roleStartsOf(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, start := range superviseRowsOf(t, pool, key, "start") {
		if start.Role == role {
			starts = append(starts, start)
		}
	}
	return starts
}

// noticesOf is every notice of kind key's outbox holds, oldest first: the rows of key alone, so a
// notice on another issue is not read as key's. Each is read without the fence it was queued at
// (fencetest.Unfenced), as noticesOfKind reads it: a case of the fence reads record.FenceOf.
func noticesOf(t *testing.T, pool *pgxpool.Pool, key string, kind record.NoticeKind) []record.Notice {
	t.Helper()
	var notices []record.Notice
	for _, row := range issueOutboxOf(t, pool, key, string(record.OutboxKindNotice)) {
		payload, err := record.DecodeOutboxPayload(record.OutboxRow{ID: row.id, Kind: record.OutboxKindNotice, Issue: key, Payload: row.payload})
		if err != nil {
			t.Fatalf("decode notice row %d of %s: %v", row.id, key, err)
		}
		if notice, ok := payload.(record.Notice); ok && notice.Kind == kind {
			notices = append(notices, fencetest.Unfenced(notice).(record.Notice))
		}
	}
	return notices
}

// assertWalksAt fails unless key stands in want with task a running at place.
func assertWalksAt(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, want phase.Phase, place record.TaskPlace) {
	t.Helper()
	if got := issueOf(t, pool, engine, key); got.Phase != want || got.Hold != nil {
		t.Fatalf("%s stands in %s (hold %+v), want %s", key, got.Phase, got.Hold, want)
	}
	if task := taskOf(t, pool, engine, key); task.State != record.TaskRunning || task.Place != place {
		t.Fatalf("task a of %s is %s at %+v, want running at %+v", key, task.State, task.Place, place)
	}
}

// A failed step returns the task's work to the step of the type implementation before it, read
// from the type the plan's rows record for each step (record.PlanStep.Type), and no count stops a
// task: each return counts and tells as today, on the task. A plan whose one step is the tester is
// refused, since it has no implementer.
func TestAFailedStepReturnsAndNoCountStopsATask(t *testing.T) {
	t.Parallel()
	t.Run("a review that requests changes returns to the implementer, the same agents on", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, "", record.PhaseRow{})
		implementerBefore, reviewerBefore := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer)), len(roleStartsOf(t, pool, root.Key, claim.RoleReviewer))

		reviewRequestsChanges(t, pool, engine, root.Key)

		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
		if len(starts) != implementerBefore+1 {
			t.Fatalf("the return wrote %d starts of the implementer, want one", len(starts)-implementerBefore)
		}
		// The return goes to the agent the step ran before, in its session, and the
		// reviewer keeps its own: no start of the return asks for a fresh session, and the
		// reviewer is started nothing.
		if start := starts[len(starts)-1]; start.FreshSession || start.Phase != phase.Implementing || !strings.Contains(start.Task, "Reason: "+changesBody) {
			t.Fatalf("the implementer's start = %+v, want a start of implementing in its own session whose task holds %q", start, "Reason: "+changesBody)
		}
		// An issue's one pending phase start the outbox has not run is replaced by the next one
		// (record.Store.EnqueueStart), so the count can fall; it must not rise.
		if got := len(roleStartsOf(t, pool, root.Key, claim.RoleReviewer)); got > reviewerBefore {
			t.Fatalf("the return wrote %d starts of the reviewer, want none: the reviewer keeps its session for the next round", got-reviewerBefore)
		}

		// The review of the next round approves, and the task walks on to Legion's own retro.
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
	})

	t.Run("a plan whose one step is the tester is refused: it has no implementer", func(t *testing.T) {
		given := plan.Plan{ArtifactID: "plan-artifact", Version: 2, Tasks: []plan.Task{{ID: "a", Steps: []plan.Step{{Role: "tester"}}, Text: "Test it."}}}
		refusals := append(plan.Validate(given, builtinDeployment), plan.Supported(given)...)
		want := []plan.Refusal{
			{Task: "a", Reason: "task a has no step implementer. This release of Legion does not carry it out yet."},
			{Task: "a", Reason: "step 1 of task a names tester, and no step before it names the role implementer, whose pull request tester reads. This release of Legion does not carry it out yet."},
		}
		if !reflect.DeepEqual(refusals, want) {
			t.Fatalf("the plan `tester` is refused with %+v, want the two reasons %+v", refusals, want)
		}
	})

	t.Run("three rejected reviews, then two failed tests, and the task runs on", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		for range 3 {
			move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
			move(t, pool, engine, root.Key, TriggerTesterPassed, "", record.PhaseRow{})
			reviewRequestsChanges(t, pool, engine, root.Key)
		}
		if prompts := hookPrompts(t, pool); len(prompts) != 0 {
			t.Fatalf("the hook fired %d prompts after 3 returns, want none before the 5th", len(prompts))
		}
		testsFail(t, pool, engine, root.Key, 2)

		if blocked := noticesOf(t, pool, root.Key, "pr-blocked"); len(blocked) != 1 || !strings.Contains(blocked[0].Reason, "review_round_cap=3") {
			t.Fatalf("the pr-blocked notices = %+v, want one naming review_round_cap=3", blocked)
		}
		prompts := hookPrompts(t, pool)
		if want := []string{"implementer", "tester", "reviewer"}; !sameStrings(promptRoles(prompts), want) {
			t.Fatalf("the hook prompted %v at the 5th return, want %v", promptRoles(prompts), want)
		}
		for _, prompt := range prompts {
			if prompt.Hook != "work-back" || prompt.Count != 5 || prompt.From != phase.Testing || prompt.To != phase.Implementing {
				t.Fatalf("the %s's prompt = %+v, want work-back fired at the 5th return, testing to implementing", prompt.Role, prompt)
			}
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
	})

	t.Run("the owner's failed acceptance returns to the tester and counts no round", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "acceptance:owner")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, "", record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, record.TaskPlace{Kind: record.PlaceStep, Position: 3})
		entry := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer).Entry

		moveBack := rootsMoveBack(t, pool, engine, root.Key, phase.Testing, "the widget does not render")
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			result, err := engine.Apply(ctx, tx, moveBack)
			if err == nil && result.Refusal != nil {
				t.Fatalf("the owner's move back was refused: %+v", result.Refusal)
			}
			return err
		})

		assertWalksAt(t, pool, engine, root.Key, phase.Testing, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
		if got := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer).Entry; got != entry {
			t.Fatalf("the implementer's entry is %d after the owner's move back to testing, want %d: the implementer is started nothing", got, entry)
		}
		if counts := taskOf(t, pool, engine, root.Key).Counts; counts.Rounds != 0 || counts.Pass != 1 || counts.BackwardMoves["accepting-testing"] != 1 {
			t.Fatalf("the task counts %+v, want one pass under accepting-testing and no round: the owner's own failed acceptance is no review round", counts)
		}
	})

	// The owner's move back from accepting to the implementer counts a pass and no review round:
	// only a review that requests changes counts one (recordRound). The review before it is the
	// control: it raises the round count, so a read that cannot see the count fails there.
	t.Run("the owner's move back to the implementer counts a pass and no round", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer", "acceptance:owner")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		reviewRequestsChanges(t, pool, engine, root.Key)
		if counts := taskOf(t, pool, engine, root.Key).Counts; counts.Rounds != 1 || counts.Pass != 1 {
			t.Fatalf("after the review's request for changes the task counts %+v, want one round and one pass", counts)
		}
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, record.TaskPlace{Kind: record.PlaceStep, Position: 3})
		before := taskOf(t, pool, engine, root.Key).Counts

		applyRefusingNothing(t, pool, engine, rootsMoveBack(t, pool, engine, root.Key, phase.Implementing, "the migration is missing"))

		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		after := taskOf(t, pool, engine, root.Key).Counts
		if after.Rounds != before.Rounds || after.Pass != before.Pass+1 || after.BackwardMoves["accepting-implementing"] != 1 {
			t.Fatalf("the owner's move back took the task's counts from %+v to %+v, want the same rounds, one more pass and accepting-implementing 1", before, after)
		}
	})
}

// The control of TestAFailedStepReturnsAndNoCountStopsATask: its review that requests changes
// again, with its implementer step's stored type set to review before the review rejects. No step
// before the reviewer is of the type implementation, so nothing can take the work back, and the
// task stops as stop_task stops it: the task stopped with why, the issue held from reviewing for
// the stopped task, the reviewer's step ended failed and returned nowhere, its process stopped
// (step_stop) and its session kept, and the owner told once (task-stopped). The first test's
// assertion of that review, that the issue stands in implementing, fails on it.
func TestAReturnWithNoImplementationStepStopsTheTask(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerTesterPassed, "", record.PhaseRow{})
	// The type is forged: no plan of this release writes it. Each failed step a plan can hold comes
	// after its implementer (plan.Supported), so only a stored type that no plan gives reaches the
	// stop this test asserts (returnTarget).
	if _, err := pool.Exec(t.Context(), "update plan_steps set type = 'review' where issue = $1 and task = 'a' and position = 1", root.Key); err != nil {
		t.Fatalf("store the implementer step's type as review: %v", err)
	}
	implementerBefore := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer))

	reviewRequestsChanges(t, pool, engine, root.Key)

	issue := issueOf(t, pool, engine, root.Key)
	if issue.Phase == phase.Implementing {
		t.Fatalf("%s stands in implementing: the first test's assertion of a review that requests changes would pass on a task that no step can take back", root.Key)
	}
	if issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Reviewing, Reason: record.HoldTaskStopped}) {
		t.Fatalf("%s stands in %s with hold %+v, want held from reviewing for the stopped task", root.Key, issue.Phase, issue.Hold)
	}
	const why = "step 3 (reviewer) failed and no earlier step of the type implementation can take the work"
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.StoppedReason != why ||
		task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 3}) {
		t.Fatalf("task a is %s (%q) at %+v, want stopped at step 3 with the reason %q", task.State, task.StoppedReason, task.Place, why)
	}
	if got := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer)); got != implementerBefore {
		t.Fatalf("the stop started the implementer %d times, want none", got-implementerBefore)
	}
	// A notice is the architect's to read, and names the role and the phase it is about, as the
	// held notice of a failed claim does.
	notices := noticesOf(t, pool, root.Key, "task-stopped")
	if len(notices) != 1 || notices[0].Role != claim.RoleReviewer || notices[0].Phase != phase.Reviewing ||
		!strings.Contains(notices[0].Reason, "task a") || !strings.Contains(notices[0].Reason, "step 3 (reviewer)") {
		t.Fatalf("the task-stopped notices = %+v, want one to the owner about the reviewer, naming task a and step 3 (reviewer)", notices)
	}
	stops := superviseRowsOf(t, pool, root.Key, "step_stop")
	if len(stops) != 1 || stops[0].Role != claim.RoleReviewer || stops[0].Generation != root.Generation ||
		stops[0].Reason != "task a of LEGION-208 was stopped: "+why {
		t.Fatalf("the step stops = %+v, want one of the reviewer naming why the task stopped", stops)
	}
	writes := stepWrites(t, pool, root.Key)
	if end := writes[len(writes)-1]; !isEnd(end) || end.Kind != "step" || end.Role != "reviewer" || resultOf(end) != "failed" || end.ReturnedTo != nil {
		t.Fatalf("the last step record write = %+v (result %q), want the reviewer's end, failed and returned nowhere", end, resultOf(end))
	}
	if counts := taskOf(t, pool, engine, root.Key).Counts; counts.Pass != 0 || counts.Rounds != 0 {
		t.Fatalf("the task counts %+v after a stop, want nothing: no work went back", counts)
	}
}

// failedTest is the tester's completion of testing on key, generation 1, that fails its step and
// names step returnTo of the task as the one the task returns to (0 names none), with handoff as
// its handoff record.
func failedTest(key string, returnTo int, handoff string) intake.HandoffComplete {
	return intake.HandoffComplete{Generation: 1, Issue: key, Role: claim.RoleTester, Claim: "test-claim", Summary: "the widget does not render",
		Verdict: "fail", HandoffRecord: handoff, HandoffVersion: 1, ReturnTo: returnTo}
}

// recordAt is what a refused completion must leave as it found: the issue, its task where it has
// plan rows, the tester's phases row and the number of outbox rows.
type recordAt struct {
	issue  record.Issue
	task   *record.PlanTask
	tester record.PhaseRow
	outbox int
}

func standingOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, planned bool) recordAt {
	t.Helper()
	now := recordAt{issue: issueOf(t, pool, engine, key), tester: roleRowOf(t, pool, engine, key, claim.RoleTester), outbox: outboxRows(t, pool)}
	if planned {
		task := taskOf(t, pool, engine, key)
		now.task = &task
	}
	return now
}

// assertRefusedReturnTo fails unless result is the 409 RETURN_REFUSED whose message is want, and
// key stands as it stood before the completion (before).
func assertRefusedReturnTo(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, result intake.Result, want string, before recordAt) {
	t.Helper()
	if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "RETURN_REFUSED" || result.Refusal.Message != want {
		t.Fatalf("the completion = %+v, want the 409 RETURN_REFUSED %q", result.Refusal, want)
	}
	if after := standingOf(t, pool, engine, key, before.task != nil); !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused completion changed %s:\n before %+v\n after  %+v", key, before, after)
	}
}

// A failed step's report can name the step of its task that the task returns to (returnTo):
// any earlier step of the same task, not only the step of the type implementation that the
// steps' types give, and the agent of that step gets the report. A report that names the failed
// step itself or a later one, or a place no step of the task holds, is refused and changes
// nothing, so the agent reports again (RETURN_REFUSED), as a backward move that no row serves is
// refused today. An issue with no plan rows has no step to name: a report naming one is refused,
// and the same report naming none returns to implementing as today.
func TestReturnToNamesAnEarlierStep(t *testing.T) {
	t.Parallel()
	// planned is LEGION-208 walking the steps planner implementer tester, standing in testing at
	// step 3, its implementer started once.
	planned := func(t *testing.T) (*pgxpool.Pool, *Engine, record.Issue) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "planner", "implementer", "tester")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerPlannerCompleted, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Testing, record.TaskPlace{Kind: record.PlaceStep, Position: 3})
		return pool, engine, root
	}

	t.Run("returnTo 1 returns the task to the planner, which gets the tester's report", func(t *testing.T) {
		pool, engine, root := planned(t)
		planners, implementers := len(roleStartsOf(t, pool, root.Key, claim.RolePlanner)), len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer))

		if result := applyFacts(t, pool, engine)("fail-1", failedTest(root.Key, 1, "test-1")); result.Refusal != nil {
			t.Fatalf("the tester's fail naming step 1 was refused: %+v", result.Refusal)
		}

		assertWalksAt(t, pool, engine, root.Key, phase.Planning, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		starts := roleStartsOf(t, pool, root.Key, claim.RolePlanner)
		if len(starts) != planners+1 {
			t.Fatalf("the return wrote %d starts of the planner, want one", len(starts)-planners)
		}
		report := "Handoff: test version 1 (dispatch handoff-read --issue LEGION-208 --phase test --version 1). " + testerVerdictLabel + ": fail."
		if start := starts[len(starts)-1]; start.FreshSession || start.Phase != phase.Planning || !strings.Contains(start.Task, report) {
			t.Fatalf("the planner's start = %+v, want a start of planning in its own session whose task holds the tester's report %q", start, report)
		}
		if got := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer)); got > implementers {
			t.Fatalf("the return wrote %d starts of the implementer, want none: the report named the planner", got-implementers)
		}
		if counts := taskOf(t, pool, engine, root.Key).Counts; counts.Pass != 1 || counts.BackwardMoves[phase.Pair(phase.Testing, phase.Planning)] != 1 {
			t.Fatalf("the task counts %+v, want one pass under %s", counts, phase.Pair(phase.Testing, phase.Planning))
		}
		writes := stepWrites(t, pool, root.Key)
		var testEnd *record.StepRecord
		for i := range writes {
			if isEnd(writes[i]) && writes[i].Role == "tester" {
				testEnd = &writes[i]
			}
		}
		if testEnd == nil || resultOf(*testEnd) != "failed" || testEnd.ReturnedTo == nil || *testEnd.ReturnedTo != 1 {
			t.Fatalf("the tester's step end = %+v, want failed and returned to step 1", testEnd)
		}
	})

	for _, tc := range []struct {
		name     string
		returnTo int
		want     string
	}{
		{"returnTo naming the failed step itself is not earlier", 3,
			"returnTo is 3, and step 3 (tester) of task a of LEGION-208 is not before step 3 (tester), the step that failed; send the fail again naming an earlier step, or without returnTo; nothing was recorded"},
		{"returnTo naming a place no step of the task holds names another task's", 4,
			"returnTo is 4, and task a of LEGION-208 has steps 1 to 3, so it names no step of the task whose step 3 (tester) failed; send the fail again naming an earlier step of that task, or without returnTo; nothing was recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, engine, root := planned(t)
			before := standingOf(t, pool, engine, root.Key, true)
			apply := applyFacts(t, pool, engine)

			assertRefusedReturnTo(t, pool, engine, root.Key, apply("fail-refused", failedTest(root.Key, tc.returnTo, "test-1")), tc.want, before)

			// The refusal recorded nothing, its handoff record among it, so the tester reports
			// again with the same record, and the report naming no step returns to the
			// implementer, the step of the type implementation before it.
			if result := apply("fail-again", failedTest(root.Key, 0, "test-1")); result.Refusal != nil {
				t.Fatalf("the tester's fail sent again without returnTo was refused: %+v", result.Refusal)
			}
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
		})
	}

	t.Run("an issue with no plan rows has no step to name, and returns as today", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Testing)
		before := standingOf(t, pool, engine, root.Key, false)
		apply := applyFacts(t, pool, engine)

		assertRefusedReturnTo(t, pool, engine, root.Key, apply("fail-refused", failedTest(root.Key, 1, "test-1")),
			"returnTo is 1, and LEGION-208 has no plan of tasks in Legion's record, so it has no step to name; send the fail again without returnTo; nothing was recorded", before)

		if result := apply("fail-again", failedTest(root.Key, 0, "test-1")); result.Refusal != nil {
			t.Fatalf("the tester's fail sent again without returnTo was refused: %+v", result.Refusal)
		}
		if got := issueOf(t, pool, engine, root.Key); got.Phase != phase.Implementing || got.Hold != nil {
			t.Fatalf("%s stands in %s (hold %+v) after the tester's fail, want implementing as today", root.Key, got.Phase, got.Hold)
		}
		if pass := implementerPass(t, pool); pass != 1 {
			t.Fatalf("the implementer's pass is %d, want 1: an issue with no plan rows counts its returns on the implementer's row as today", pass)
		}
	})
}

// A tester's fail can arrive after a later version of the plan took its step off the task's line:
// the owner stopped task a in testing and gave version 3, `implementer reviewer`, which started
// the task again at its implementer. The fail is a report of a run the daemon ended without it
// (leftItsPhase), and returnTo names no step of the task now, so it is refused 409 RETURN_REFUSED
// with what to send instead, and records nothing, as every returnTo the task's steps cannot take
// is. The same fail naming no step is then taken on the tester's own row and moves nothing. Before,
// the walk's error answered the route 500 FACT_APPLY_FAILED at every retry of the same fail.
func TestALateFailNamingAStepOnALineThatLostItsPhaseIsRefused(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	assertWalksAt(t, pool, engine, root.Key, phase.Testing, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
	// The claim the daemon started the tester under, which is what makes its fail the report of a
	// run (record.Store.RoleRun).
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('test-claim', 'LEGION', 'LEGION-208', 'LEGION-208', 'tester', 1, 'ses-tester', '', 'working', 0, 0, 0, 0)`); err != nil {
		t.Fatalf("seed the tester's claim: %v", err)
	}
	apply := applyFacts(t, pool, engine)
	if result := apply("stop", ownersStop(t, issueOf(t, pool, engine, root.Key), "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop of task a in testing was refused: %+v", result.Refusal)
	}
	later := intake.PlanGiven{Issue: root.Key, Generation: root.Generation, GivenBy: claim.Token(treeRoleToken(t, root.Key, claim.RoleArchitect)),
		Plan: plan.Plan{ArtifactID: "plan-artifact", Version: 3, Tasks: []plan.Task{{ID: "a", Steps: []plan.Step{{Role: "implementer"}, {Role: "reviewer"}},
			Text: "Build the widget."}}}}
	if result := apply("version 3", later); result.Refusal != nil {
		t.Fatalf("version 3 of the plan, over the stopped task, was refused: %+v", result.Refusal)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
	before := standingOf(t, pool, engine, root.Key, true)

	assertRefusedReturnTo(t, pool, engine, root.Key, apply("late fail", failedTest(root.Key, 1, "test-1")),
		"returnTo is 1, and task a of LEGION-208 has no step that runs as testing now (its steps are implementer reviewer), so this fail names no step of the task to return to; send the fail again without returnTo; nothing was recorded",
		before)

	if result := apply("late fail again", failedTest(root.Key, 0, "test-1")); result.Refusal != nil {
		t.Fatalf("the tester's late fail sent again without returnTo was refused: %+v", result.Refusal)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleTester); !row.Completed() || row.HandoffRecord != "test-1" {
		t.Fatalf("the tester's row = %+v, want its late fail recorded, naming test-1", row)
	}
}

// The walk takes the task a run is on by the store's one rule (record.StoredPlan.Task): the plan's
// one task. A plan of two tasks, which no plan of this release writes, is no walk, and the error
// is the rule's own, naming the plan and its count of tasks. The control is the same issue with
// its plan of one task, which walks it. Each walk is read in a transaction of its own and with no
// boot pass, which would read the same walk first.
func TestTheWalkTakesItsTaskByTheStoresOneRule(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
	walkOf := func() (Walk, error) {
		var w Walk
		var walked error
		seedRecord(t, pool, func(tx pgx.Tx) error {
			w, walked = engine.walk(t.Context(), tx, root)
			return nil
		})
		return w, walked
	}
	if w, err := walkOf(); err != nil || !w.Stored || w.Task != "a" {
		t.Fatalf("the walk of a plan of one task = %+v, %v, want task a", w, err)
	}

	first := record.PlanTask{Issue: root.Key, Generation: root.Generation, Task: "a", PlanVersion: 3, BlockText: "Build the widget.", State: record.TaskWaiting}
	second := record.PlanTask{Issue: root.Key, Generation: root.Generation, Task: "b", PlanVersion: 3, BlockText: "Document the widget.", State: record.TaskWaiting}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		v := record.PlanVersion{Issue: root.Key, Generation: root.Generation, Version: 3, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: claim.Token(treeRoleToken(t, root.Key, claim.RoleArchitect)), GivenAt: stepClock}
		steps := []record.PlanStep{
			{Issue: root.Key, Generation: root.Generation, Task: "a", Position: 1, Role: "implementer", Type: plan.TypeImplementation},
			{Issue: root.Key, Generation: root.Generation, Task: "b", Position: 1, Role: "implementer", Type: plan.TypeImplementation},
		}
		return record.NewStore().PutPlan(t.Context(), tx, v, []record.PlanTask{first, second}, steps)
	})
	_, err := walkOf()
	if want := "walk LEGION-208: the plan of LEGION-208 at generation 1 holds 2 tasks, and this release walks one"; err == nil || err.Error() != want {
		t.Fatalf("the walk of a plan of two tasks = %v, want the error %q", err, want)
	}
}
