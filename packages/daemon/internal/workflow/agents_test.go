package workflow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// agentsOfTask is the name of the agent of each stored step of task, in place order.
func agentsOfTask(stored *record.StoredPlan, task string) []claim.Agent {
	var names []claim.Agent
	for _, step := range stored.Steps[task] {
		names = append(names, step.Agent)
	}
	return names
}

// A name a key held is that key's alone: a later version in which a step with no key, or another
// key, would take it would run that agent in the name's claim and resume the key's session. Such a
// version is refused as a plan, 409 PLAN_REFUSED with R14's ending, naming the key that holds the
// name, before anything is written; the fact stays unspent, and version 1's rows, with their names,
// stand.
func TestALaterVersionThatWouldGiveAKeysNameToAnotherAgentIsRefusedAsAPlan(t *testing.T) {
	for _, tc := range []struct {
		name      string
		first     []string
		firstWant []claim.Agent
		later     []string
		place     int
		takes     string
		holder    string
	}{
		{name: "a step with no key", first: []string{"implementer:a", "tester:a", "reviewer"}, firstWant: []claim.Agent{"implementer", "implementer", "reviewer"},
			later: []string{"implementer", "tester:a", "reviewer"}, place: 1, takes: "implementer", holder: "a"},
		{name: "the key dropped and its step given to no key", first: []string{"implementer", "tester:k", "reviewer:k"}, firstWant: []claim.Agent{"implementer", "tester", "tester"},
			later: []string{"implementer", "tester", "reviewer"}, place: 2, takes: "tester", holder: "k"},
		{name: "the key dropped and its step given to another key", first: []string{"implementer", "tester:k", "reviewer:k"}, firstWant: []claim.Agent{"implementer", "tester", "tester"},
			later: []string{"implementer", "tester:m", "reviewer"}, place: 2, takes: "tester", holder: "k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			apply := applyFacts(t, pool, engine)
			root := stepRoot(t, pool, phase.Admitted)
			key, gen := root.Key, root.Generation
			mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", tc.first...))))
			move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
				t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
			}
			before := planStandingOf(t, pool, engine, key, gen)
			if got := agentsOfTask(before.plan, "a"); !slices.Equal(got, tc.firstWant) {
				t.Fatalf("the agents of version 1 = %q, want %q", got, tc.firstWant)
			}

			id := planID(key, gen, 2)
			reason := intake.AgentUnnamed("a", tc.place, tc.takes, tc.holder)
			if !strings.Contains(reason.Reason, "key "+tc.holder) || !strings.Contains(reason.Reason, " "+tc.takes+",") {
				t.Fatalf("the refusal's reason %q names neither key %s nor the name %s", reason.Reason, tc.holder, tc.takes)
			}
			want := &intake.Refusal{Status: 409, Code: "PLAN_REFUSED", Message: "Legion refused this plan and recorded nothing: " + reason.Reason,
				Reasons: []plan.Refusal{reason}}
			assertPlanRefusal(t, pool, engine, key, gen, id,
				apply(id, ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", tc.later...))), want, before)
		})
	}
}

// A version that gives a task again names its keys from the rows read before it replaces the
// task's steps, so a key keeps the name it had, even where its first step is now of another role.
func TestARegivenTaskKeepsTheNamesOfItsKeys(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "tester:a", "reviewer"))))
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "tester", "reviewer"}) {
		t.Fatalf("the agents of version 1 = %q, want implementer, key a named tester, and reviewer", got)
	}
	mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget, and its manual.", "implementer:a", "tester:a", "reviewer"))))
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"tester", "tester", "reviewer"}) {
		t.Fatalf("the agents of version 2 = %q, want key a still named tester at steps 1 and 2, and reviewer", got)
	}
}

// The project's default steps are named from their own steps alone when the first step of a run
// with no owner's plan writes them: each agent has a name, the steps of a key share it.
func TestTheDefaultStepsAreNamedWhenTheyAreWritten(t *testing.T) {
	engine := returnEngine()
	engine.cfg.DefaultSteps = []plan.Step{{Role: "implementer"}, {Role: "tester", Key: "r"}, {Role: "reviewer", Key: "r"}}
	_, _, steps, handed := engine.defaultRows(record.Issue{Key: "LEGION-208", Generation: 1}, nil, nil)
	if len(handed) != 0 {
		t.Fatalf("the default steps with no earlier rows handed on %+v, want no name", handed)
	}
	var names []claim.Agent
	for _, step := range steps {
		names = append(names, step.Agent)
	}
	if want := []claim.Agent{"implementer", "tester", "tester"}; !slices.Equal(names, want) {
		t.Fatalf("the default steps are named %q, want %q", names, want)
	}
}

// rolesRows is every phases row of key's role, by place.
func rolesRows(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role) map[int]record.PhaseRow {
	t.Helper()
	rows := map[int]record.PhaseRow{}
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		all, err := engine.store.Phases(ctx, tx, key)
		for _, row := range all {
			if row.Role == role {
				rows[row.StepPlace] = row
			}
		}
		return err
	})
	return rows
}

// roleRowNow is role's row of key now, the one with the highest entry (record.PhaseRowOf), read
// with no walk: where an entry rose since, it is the row that rose.
func roleRowNow(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role) record.PhaseRow {
	t.Helper()
	var row record.PhaseRow
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := engine.store.Phases(ctx, tx, key)
		row = record.PhaseRowOf(rows, role)
		return err
	})
	return row
}

// A role's row of no step, the row a new generation cleared, is the row its first step's entry
// raises: the role keeps one row, which carries the record it last named across the generation as
// it did, and no row of no step stands beside the step's. Before, the entry wrote a second row at
// the step and the cleared row stood on beside it, read by any reader of the role's first row.
func TestARowOfNoStepBecomesTheRowOfTheRolesStep(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: agentToken(t, "implementer"), Entry: 4,
		LastHandoff: "implement-of-the-generation-before", EntryRaisedAt: stepClock})
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	rows := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)
	if row, ok := rows[1]; len(rows) != 1 || !ok || row.StepTask != "a" || row.Entry != 5 || row.LastHandoff != "implement-of-the-generation-before" {
		t.Fatalf("the implementer's rows = %+v, want one, at step 1 of task a, raised to entry 5, keeping the record it last named", rows)
	}
}

// agentToken is the claim token of agent on LEGION-208.
func agentToken(t *testing.T, agent claim.Agent) claim.Token {
	t.Helper()
	token, err := claim.AgentToken("legion", "LEGION-208", agent)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// A role stands at two steps of `implementer:a reviewer implementer:a`, and each step's run keeps a
// row of its own, so the task of step 3 names the report of step 1 and of step 2, each from the row
// of its place, and step 3 runs in step 1's claim. A fourth step reads the report of each of the
// implementer's two steps once, under its own place. Before, the implementer's one row was step
// 1's until step 3's start cleared its report, so step 3 named no report of step 1, and a match of
// the rows by role names each implementer's report under both of its places.
func TestTheThirdStepsTaskNamesTheReportsOfStepsOneAndTwo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
	}{
		{"implementer:a reviewer implementer:a", []string{"implementer:a", "reviewer", "implementer:a"}},
		{"implementer:a reviewer implementer:a tester", []string{"implementer:a", "reviewer", "implementer:a", "tester"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := textEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, tc.steps...)
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			completesAs(t, pool, engine, claim.RoleImplementer, agentToken(t, "implementer"), "implement", 1, "head")
			completesAs(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), "review", 1, "head")
			move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))

			third := lastStart(t, pool, root.Key, claim.RoleImplementer)
			for _, says := range []string{" Step 3 of " + string(rune('0'+len(tc.steps))) + ": implementer:a.",
				" Step 1 (implementer:a) reported: implement version 1.", " Step 2 (reviewer) reported: review version 1."} {
				if !strings.Contains(third, says) {
					t.Fatalf("step 3's task = %q, want it to say %q", third, says)
				}
			}
			implementer := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)
			if first, again := implementer[1], implementer[3]; first.Claim != agentToken(t, "implementer") || again.Claim != first.Claim {
				t.Fatalf("the implementer's rows name the claims %q at step 1 and %q at step 3, want both %q: one key is one agent",
					first.Claim, again.Claim, agentToken(t, "implementer"))
			}
			if len(tc.steps) < 4 {
				return
			}
			completesAs(t, pool, engine, claim.RoleImplementer, agentToken(t, "implementer"), "implement", 2, "head")
			fourth := lastStart(t, pool, root.Key, claim.RoleTester)
			want := " Step 1 (implementer:a) reported: implement version 1. Step 2 (reviewer) reported: review version 1." +
				" Step 3 (implementer:a) reported: implement version 2."
			if !strings.Contains(fourth, want) || strings.Count(fourth, "reported: implement") != 2 {
				t.Fatalf("step 4's task = %q, want each step's report once: %q", fourth, want)
			}
		})
	}
}

// While step 2 of `implementer:a reviewer implementer:a` runs, the engine's row of the implementer
// is the row of step 1, the implementer's latest step before it, which holds the report step 2
// reads; while step 3 runs it is the row of step 3. The reviewer's row read at step 3 is step 2's.
func TestAReviewerReadsTheLatestImplementersRowBeforeIt(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer", "implementer:a")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completes(t, pool, engine, claim.RoleImplementer, "implement", 1)
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.StepPlace != 1 || row.HandoffRecord != "implement-1" {
		t.Fatalf("the implementer's row while step 2 runs = %+v, want step 1's, with its report", row)
	}
	completes(t, pool, engine, claim.RoleReviewer, "review", 1)
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	first := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[1]
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.StepPlace != 3 || row.HandoffRecord != "" || row.Entry <= first.Entry {
		t.Fatalf("the implementer's row while step 3 runs = %+v, want step 3's, raised above step 1's entry %d with no report yet", row, first.Entry)
	}
	if first.HandoffRecord != "implement-1" {
		t.Fatalf("step 1's row after step 3 started = %+v, want its report kept", first)
	}
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer); row.StepPlace != 2 || row.HandoffRecord != "review-1" {
		t.Fatalf("the reviewer's row while step 3 runs = %+v, want step 2's", row)
	}
}

// A role written before its first step is written on the row of that step, as the one row of a
// role was: a reviewer's row read and written back while the task stands at the implementer's step
// is the row of the reviewer's step, which the reviewer's start then raises, so the role keeps one
// row and what was written stands on it.
func TestAWriteBeforeARolesFirstStepLandsOnThatStepsRow(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		row, err := engine.phaseRow(ctx, tx, root, claim.RoleReviewer)
		if err != nil {
			return err
		}
		row.ReviewedHead = "head-read-before"
		return engine.store.PutPhase(ctx, tx, row)
	})
	completes(t, pool, engine, claim.RoleImplementer, "implement", 1)
	rows := rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)
	if row, ok := rows[2]; len(rows) != 1 || !ok || row.ReviewedHead != "head-read-before" || row.Entry == 0 {
		t.Fatalf("the reviewer's rows = %+v, want one, at step 2, raised, with the commit written before its step", rows)
	}
}

// A role that stands at two steps runs its phase at two places, and a move back to that phase
// reaches the nearest of them before the task's place, while a failed step's work returns to the
// first step of the type implementation before it (Walk.returnTarget): in
// `implementer:a reviewer implementer:a tester`, the tester's move back to implementing reaches
// step 3, the implementer's step the tester tested, and a fail of the tester that names no step
// returns to step 1. Before, a phase that ran at two earlier places served no backward row, and the
// move was refused as one no row serves.
func TestAMoveBackToAPhaseThatRunsTwiceReachesTheNearest(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer", "implementer:a", "tester")
	implementer := agentToken(t, "implementer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 2, "head")
	assertWalksAt(t, pool, engine, root.Key, phase.Testing, stepPlace(4))
	if result := applyFacts(t, pool, engine)("tester-back", intake.BackwardMove{Issue: root.Key, Requester: claim.RoleTester, To: phase.Implementing,
		Reason: "the harness needs a fixture"}); result.Refusal != nil {
		t.Fatalf("the tester's move back to implementing was refused: %+v", result.Refusal)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))
}

// The claim of a role's worker read with no walk (record.WorkerClaim) is the claim of the row the
// engine reads where the task stands (record.PhaseRowAt): in `implementer:a tester:b implementer:b
// reviewer`, where key a is named implementer and key b tester, the implementer's worker is key a
// at places 1 and 2, key b at places 3 and 4, and key a again once a failed review returns the task
// to step 1. Before, the first row of the role answered, key a at every place.
func TestWorkerClaimPicksTheRowThatPhaseRowAtGives(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "tester:b", "implementer:b", "reviewer")
	keyA, keyB := agentToken(t, "implementer"), agentToken(t, "tester")
	worker := func(in phase.Phase, at record.TaskPlace, want claim.Token) {
		t.Helper()
		assertWalksAt(t, pool, engine, root.Key, in, at)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := engine.store.Phases(ctx, tx, root.Key)
			if err != nil {
				return err
			}
			row, err := engine.phaseRow(ctx, tx, root, claim.RoleImplementer)
			if err != nil {
				return err
			}
			got, err := record.WorkerClaim("legion", root.Key, claim.RoleImplementer, rows)
			if err != nil {
				return err
			}
			if got != want || row.Claim != want {
				t.Errorf("at %+v the implementer's worker = %q and the engine's row names %q, want %q", at, got, row.Claim, want)
			}
			return nil
		})
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	worker(phase.Implementing, stepPlace(1), keyA)
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, "head")
	worker(phase.Testing, stepPlace(2), keyA)
	completesAs(t, pool, engine, claim.RoleTester, keyB, "test", 1, "head")
	worker(phase.Implementing, stepPlace(3), keyB)
	completesAs(t, pool, engine, claim.RoleImplementer, keyB, "implement", 2, "head")
	worker(phase.Reviewing, stepPlace(4), keyB)
	move(t, pool, engine, root.Key, TriggerReviewRejected, stepPlace(1), record.PhaseRow{})
	worker(phase.Implementing, stepPlace(1), keyA)
}

// Two agents of one role keep a row of it each, and the entry a step's start raises is above every
// row of its role: in `implementer:a tester:b implementer:b reviewer`, the failed review returns
// the task to step 1, whose row rises above step 3's, so key b's completion at the entry it was
// handed for step 3 is behind step 1's, and step 1's row keeps key a and no report. Step 1 is key
// a's work, so the refusal is HANDOFF_NOT_CURRENT_PHASE naming key a, and it tells key b that no
// task of that step comes to it, unspent. Before, the entry rose only above the rows of its own
// agent, so step 1's row and step 3's stood on one entry, and key b's completion passed the fence
// and was recorded as step 1's, its claim and its record on that row; and then it was refused
// HANDOFF_STALE_ENTRY, whose words told key b that step 1's task was owed to it and came by itself,
// though that task goes to key a. A turn of the same agent's other step is still refused
// HANDOFF_STALE_ENTRY (TestATurnOfARolesEarlierStepDoesNotCompleteItsLaterStep).
func TestAStepsEntryRisesAboveEveryRowOfItsRole(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "tester:b", "implementer:b", "reviewer")
	keyA, keyB := agentToken(t, "implementer"), agentToken(t, "tester")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleTester, keyB, "test", 1, "head")
	completesAs(t, pool, engine, claim.RoleImplementer, keyB, "implement", 2, "head")
	served := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[3].Entry
	move(t, pool, engine, root.Key, TriggerReviewRejected, stepPlace(1), record.PhaseRow{})
	result := applyFacts(t, pool, engine)("implementer-late-key-b", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer,
		Claim: keyB, Entry: served, Summary: "done", Commit: "head", HandoffRecord: "implement-3", HandoffVersion: 3})
	rows := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)
	if result.Refusal == nil || result.Refusal.Code != "HANDOFF_NOT_CURRENT_PHASE" || !result.Unspent {
		t.Fatalf("key b's completion at entry %d after the return to step 1 = %+v (unspent %t), want HANDOFF_NOT_CURRENT_PHASE, unspent; step 1's row names %q with the record %q at entry %d",
			served, result.Refusal, result.Unspent, rows[1].Claim, rows[1].HandoffRecord, rows[1].Entry)
	}
	for _, says := range []string{"is the work of another agent's claim, " + string(keyA), "no task of that step comes to this claim", "do not wait for one"} {
		if !strings.Contains(result.Refusal.Message, says) {
			t.Fatalf("the refusal = %q, want it to say %q", result.Refusal.Message, says)
		}
	}
	if strings.Contains(result.Refusal.Message, intake.StaleEntryInstruction) {
		t.Fatalf("the refusal = %q, want no promise of step 1's task to key b", result.Refusal.Message)
	}
	if first, third := rows[1], rows[3]; first.Claim != keyA || first.HandoffRecord != "" || first.Entry <= third.Entry {
		t.Fatalf("step 1's row after the refusal names %q with the record %q at entry %d, want %q with none, above step 3's entry %d",
			first.Claim, first.HandoffRecord, first.Entry, keyA, third.Entry)
	}
}

// A completion counts only for the step its turn was given. In `implementer:a reviewer
// implementer:a`, the reviewer's approval enters step 3 by a forward move at the head step 1 left,
// and key a's turn of step 1, still open on step 1's entry, completes at the pull request's head
// with a record of its own. Its entry is behind step 3's and is step 1's, another step of the role,
// so it is refused HANDOFF_STALE_ENTRY, step 3's row records nothing and the task stays at step 3.
// Before, a completion behind its row at the head the record holds was taken unless a move back
// raised the row's entry, so the turn of step 1 was recorded as step 3's and moved the task on to
// its retro before step 3's task reached the agent.
func TestATurnOfARolesEarlierStepDoesNotCompleteItsLaterStep(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer", "implementer:a")
	implementer := agentToken(t, "implementer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	served := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[1].Entry
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))
	result := applyFacts(t, pool, engine)("implementer-step-1-turn", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer,
		Claim: implementer, Entry: served, Summary: "done", Commit: "head", HandoffRecord: "implement-9", HandoffVersion: 9})
	third := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[3]
	if result.Refusal == nil || result.Refusal.Code != intake.StaleEntryCode {
		t.Fatalf("the turn of step 1 completing at entry %d and the head after step 3 was entered = %+v, want %s; step 3's row names the record %q",
			served, result.Refusal, intake.StaleEntryCode, third.HandoffRecord)
	}
	if third.HandoffRecord != "" || third.Completed() {
		t.Fatalf("step 3's row after the refusal = %+v, want no report", third)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))
}

// A role's late report of its later step is the report of that step's run: the run whose entry it
// serves. In `implementer:a reviewer:r implementer:a reviewer:r`, step 4's run ends before the
// reviewer completes: the review's request for changes returns the task to step 1, its first
// implementation step, and the reviewer's move back to implementing returns it to step 3, between
// the reviewer's two steps. Either way, the reviewer's completion at step 4's entry is then taken,
// moving nothing: step 4's row records it, step 4's record names its report, and step 2's row and
// record keep their own. Before, the row read was the reviewer's place nearest where the task
// stands, step 2's, whose entry is below the one served, so the completion was refused
// HANDOFF_NOT_CURRENT_PHASE, and past that fence its report would have been written onto step 2's
// record.
func TestALateReportOfARolesLaterStepIsTakenOnThatStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		ends func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string)
		at   record.TaskPlace
	}{
		{"the review requests changes", func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
			reviewRequestsChanges(t, pool, engine, key)
		}, stepPlace(1)},
		{"the reviewer moves the task back to implementing", func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
			if result := applyFacts(t, pool, engine)("reviewer-back", intake.BackwardMove{Issue: key, Requester: claim.RoleReviewer, To: phase.Implementing,
				Reason: "the change needs a migration"}); result.Refusal != nil {
				t.Fatalf("the reviewer's move back to implementing was refused: %+v", result.Refusal)
			}
		}, stepPlace(3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer:r", "implementer:a", "reviewer:r")
			implementer := agentToken(t, "implementer")
			reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 1, "head")
			completesAs(t, pool, engine, claim.RoleReviewer, reviewer, "review", 1, "head")
			move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
			completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 2, "head")
			assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(4))
			served := rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)[4].Entry
			tc.ends(t, pool, engine, root.Key)
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, tc.at)
			before := reportsAt(t, pool, root.Key, 2)

			report := reportOf(claim.RoleReviewer, reviewer, "review-2")
			report.Entry = served
			if result := applyFacts(t, pool, engine)("reviewer-late-step-4", report); result.Refusal != nil {
				t.Fatalf("the reviewer's completion at step 4's entry %d after the task returned to %+v = %d %s %q, want it taken on step 4",
					served, tc.at, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
			}
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, tc.at)
			rows := rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)
			if rows[4].HandoffRecord != "review-2" || rows[4].Entry != served || rows[2].HandoffRecord != "review-1" {
				t.Fatalf("the reviewer's rows after its late report: step 2 %+v, step 4 %+v; want review-1 kept on step 2 and review-2 on step 4 at entry %d",
					rows[2], rows[4], served)
			}
			if got := reportsAt(t, pool, root.Key, 4); !slices.Equal(got, []string{"a:review-2"}) {
				t.Fatalf("the reports of step 4 = %v, want the late report a:review-2", got)
			}
			if got := reportsAt(t, pool, root.Key, 2); !slices.Equal(got, before) {
				t.Fatalf("the reports of step 2 = %v, want them as they stood, %v", got, before)
			}
		})
	}
}

// A return to a role's earlier step carries the record the role named last, as the first run of a
// later step does. In `implementer:a reviewer implementer:a tester`, the tester fails naming step 1,
// and agent a's completion of step 1 then names the record it wrote for step 3: it is refused
// HANDOFF_NOT_NEW, and a record written anew is taken. Before, the return raised step 1's row as it
// stood, still naming step 1's first record, so the record of step 3 passed as new work and later
// steps read it as step 1's report.
func TestAReturnToARolesEarlierStepRefusesTheRecordOfItsLaterStep(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer", "implementer:a", "tester")
	implementer := agentToken(t, "implementer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 3, "head")
	assertWalksAt(t, pool, engine, root.Key, phase.Testing, stepPlace(4))
	apply := applyFacts(t, pool, engine)
	if result := apply("tester-fails-to-step-1", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleTester, Claim: agentToken(t, "tester"),
		Summary: "fails", Commit: "head", Verdict: "fail", ReturnTo: 1, HandoffRecord: "test-1", HandoffVersion: 1}); result.Refusal != nil {
		t.Fatalf("the tester's fail naming step 1 was refused: %+v", result.Refusal)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))

	stale := intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer, Claim: implementer, Summary: "done", Commit: "head",
		HandoffRecord: "implement-3", HandoffVersion: 3}
	result := apply("implementer-step-1-names-step-3", stale)
	if result.Refusal == nil || result.Refusal.Code != "HANDOFF_NOT_NEW" {
		t.Fatalf("agent a's completion of step 1 naming its step-3 record implement-3 = %+v, want HANDOFF_NOT_NEW; step 1's row is %+v",
			result.Refusal, rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[1])
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
	fresh := stale
	fresh.HandoffRecord, fresh.HandoffVersion = "implement-4", 4
	if result := apply("implementer-step-1-anew", fresh); result.Refusal != nil {
		t.Fatalf("agent a's completion of step 1 naming a record written anew = %+v, want it taken", result.Refusal)
	}
	if first := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[1]; first.HandoffRecord != "implement-4" {
		t.Fatalf("step 1's row after the completion = %+v, want implement-4 recorded", first)
	}
}

// A keyed agent's next step after a new generation is numbered above every entry its claim was
// given, whatever rows the generation's clear folded away. In `implementer:a reviewer:b
// implementer:b`, key b, named reviewer, works steps 2 and 3 and the retro, the last of them on the
// implementer's row at place 0, so its claim is last given the retro's entry, the role's highest.
// The re-admission folds the implementer's rows into that one, and step 1 of the new generation
// raises it and writes key a's claim on it, so no row names key b above the reviewer's. Step 2's
// start must still carry an entry above the one key b's claim was given: the supervisor reads a
// start at or below that number as given already and queues nothing (supervise.Machine.given), and
// the admission pass reads the claim as level with its row (OwedEntry). Before, step 2 was numbered
// on the rows alone, at the reviewer's entry plus one, below the retro's, and its task never
// reached key b.
func TestAKeyedAgentsStepAfterANewGenerationRisesAboveWhatItsClaimWasGiven(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	steps := []string{"implementer:a", "reviewer:b", "implementer:b"}
	seedStepsPlan(t, pool, root.Key, root.Generation, steps...)
	keyA, keyB := agentToken(t, "implementer"), agentToken(t, "reviewer")
	if got := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, ""); got != keyB {
		t.Fatalf("the reviewer's claim seeded is %q, want key b's %q", got, keyB)
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleReviewer, keyB, "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, keyB, "implement", 2, "head")
	if got := issueOf(t, pool, engine, root.Key).Phase; got != phase.Retro {
		t.Fatalf("%s stands in %s after step 3, want its retro", root.Key, got)
	}
	retro := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[0]
	if retro.Claim != keyB {
		t.Fatalf("the retro's row names %q, want key b's claim %q", retro.Claim, keyB)
	}
	// The supervisor writes what each start queued to the claim (supervise.Machine.queue); the
	// retro's task is the last key b was queued.
	seedGivenEntry(t, pool, keyB, retro.Entry)

	readmitRoot(t, pool, engine, root.Key)
	next := root.Generation + 1
	seedStepsPlan(t, pool, root.Key, next, steps...)
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	if result := applyFacts(t, pool, engine)("implementer-implement-next", intake.HandoffComplete{Generation: next, Issue: root.Key,
		Role: claim.RoleImplementer, Claim: keyA, Summary: "done", Commit: "head", HandoffRecord: "implement-3", HandoffVersion: 3}); result.Refusal != nil {
		t.Fatalf("key a's completion of step 1 of generation %d was refused: %+v", next, result.Refusal)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
	starts := roleStartsOf(t, pool, root.Key, claim.RoleReviewer)
	start := starts[len(starts)-1]
	reviewer := roleRowNow(t, pool, engine, root.Key, claim.RoleReviewer)
	if start.Generation != next || reviewer.Claim != keyB || start.Entry != reviewer.Entry || start.Entry <= retro.Entry {
		t.Fatalf("step 2's start of generation %d carries the entry %d (generation %d) and the reviewer's row names %q at %d, want key b's claim %q "+
			"at an entry above %d, the retro's entry key b's claim was given, which the supervisor reads as given already",
			next, start.Entry, start.Generation, reviewer.Claim, reviewer.Entry, keyB, retro.Entry)
	}
}

// Each start of a phase worker names the claim it was queued for (record.SuperviseRequest.Claim),
// the claim of the agent of the step the task stands at, so the outbox acts on that claim and a
// start the task has left since never reaches the claim the role's row names by then. In
// `implementer:a reviewer:b implementer:b`, step 1's start names key a, step 2's and step 3's key
// b, and so does the retro's, which the agent of the last implementation step runs; the retry of
// the held retro names key b again. Before, every start named no claim, and the outbox gave each
// to the worker the role's row named when the row ran.
func TestEachStartNamesTheClaimItWasQueuedFor(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer:b", "implementer:b")
	keyA, keyB := agentToken(t, "implementer"), agentToken(t, "reviewer")
	named := func(what string, role claim.Role, want claim.Token) {
		t.Helper()
		starts := roleStartsOf(t, pool, root.Key, role)
		if start := starts[len(starts)-1]; start.Claim != want {
			t.Fatalf("%s names the claim %q, want %q", what, start.Claim, want)
		}
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	named("step 1's start", claim.RoleImplementer, keyA)
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, "head")
	named("step 2's start", claim.RoleReviewer, keyB)
	completesAs(t, pool, engine, claim.RoleReviewer, keyB, "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	named("step 3's start", claim.RoleImplementer, keyB)
	completesAs(t, pool, engine, claim.RoleImplementer, keyB, "implement", 2, "head")
	named("the retro's start", claim.RoleImplementer, keyB)
	holdFrom(t, pool, root.Key, phase.Retro)
	if result := applyFacts(t, pool, engine)("retry-retro", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision}); result.Refusal != nil {
		t.Fatalf("the retry of the held retro was refused: %+v", result.Refusal)
	}
	named("the held retro's retry", claim.RoleImplementer, keyB)
}

// completesAs applies role's completion of its step of LEGION-208 at commit, with the handoff
// record word-n at version n, under the claim token, the agent's own, as the route names the claim
// whose grant reported, which the completion writes on the role's row.
func completesAs(t *testing.T, pool *pgxpool.Pool, engine *Engine, role claim.Role, token claim.Token, word string, n int, commit string) {
	t.Helper()
	fact := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: role, Claim: token,
		Summary: "done", Commit: commit, HandoffRecord: word + "-" + string(rune('0'+n)), HandoffVersion: n}
	if role == claim.RoleTester {
		fact.Verdict = "pass"
	}
	if result := applyFacts(t, pool, engine)(string(role)+"-"+word+"-"+string(rune('0'+n)), fact); result.Refusal != nil {
		t.Fatalf("the %s's completion %s version %d was refused: %+v", role, word, n, result.Refusal)
	}
}

// A row with no key that shows a key's name says the default steps handed that name on before
// only where it is no older than the key's hold of the name. Generation 1 ran the default steps,
// whose tester took the name tester; generation 2's owner gave key k that name, and those rows went
// when generation 3's first step passed over that owner's plan, so only the lasting record keeps
// k's hold, at generation 2. Generation 3's default tester takes the name from key k for the first
// time: it is handed on, not handed before, so its claim leaves k's session and the issue is told.
// The control: key k held the name at generation 1 and the default steps took it at generation 2,
// so generation 3's tester was handed it before.
func TestADefaultStepIsHandedAKeysNameAnEarlierDefaultRunShowedBeforeTheKeyHeldIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		earlier []record.PlanStep
		held    record.KeyName
		before  bool
	}{
		{"the default rows are older than the key's hold", []record.PlanStep{
			{Issue: "LEGION-208", Generation: 1, Task: defaultTask, Position: 1, Role: "tester", Agent: "tester"},
			{Issue: "LEGION-208", Generation: 1, Task: defaultTask, Position: 2, Role: "reviewer", Agent: "reviewer"},
		}, record.KeyName{Name: "tester", AgentKey: "k", Generation: 2}, false},
		{"the control: the default rows came after the key's hold", []record.PlanStep{
			{Issue: "LEGION-208", Generation: 1, Task: "a", Position: 1, Role: "tester", AgentKey: "k", Agent: "tester"},
			{Issue: "LEGION-208", Generation: 2, Task: defaultTask, Position: 1, Role: "tester", Agent: "tester", LeaveSession: "ses-key-k"},
			{Issue: "LEGION-208", Generation: 2, Task: defaultTask, Position: 2, Role: "reviewer", Agent: "reviewer"},
		}, record.KeyName{Name: "tester", AgentKey: "k", Generation: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := returnEngine()
			engine.cfg.DefaultSteps = []plan.Step{{Role: "tester"}, {Role: "reviewer"}}
			_, _, steps, handed := engine.defaultRows(record.Issue{Key: "LEGION-208", Generation: 3}, tc.earlier, []record.KeyName{tc.held})
			if len(handed) != 1 || handed[0].Name != "tester" || handed[0].Holder != "k" || handed[0].before != tc.before {
				t.Fatalf("the default steps handed on %+v, want key k's name tester handed on with before %t", handed, tc.before)
			}
			if steps[0].Agent != "tester" {
				t.Fatalf("the default tester is named %q, want tester", steps[0].Agent)
			}
		})
	}
}

// A name a key held stays that key's after a later version drops the key: version 1 names key k
// tester, version 2 drops k and its step, which writes over the rows that showed k's name, and
// version 3, whose tester step has no key, would take the name tester and so run in k's claim and
// resume k's session. Version 3 is refused as a plan, 409 PLAN_REFUSED naming key k, before
// anything is written: the name is read from the lasting record of the names keys held
// (record.KeyNames, plan_key_names), since no plan_steps row shows it any more.
func TestANameAKeyHeldStaysItsAfterAVersionDropsTheKey(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "tester:k", "reviewer:k"))))
	move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "tester", "tester"}) {
		t.Fatalf("the agents of version 1 = %q, want key k named tester", got)
	}
	if result := apply("stop-1", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's first stop was refused: %+v", result.Refusal)
	}
	mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
	if result := apply("stop-2", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's second stop was refused: %+v", result.Refusal)
	}
	before := planStandingOf(t, pool, engine, key, gen)
	if got := agentsOfTask(before.plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "reviewer"}) {
		t.Fatalf("the agents of version 2 = %q, want implementer and reviewer, and no row showing key k's name", got)
	}

	id := planID(key, gen, 3)
	reason := intake.AgentUnnamed("a", 2, "tester", "k")
	want := &intake.Refusal{Status: 409, Code: "PLAN_REFUSED", Message: "Legion refused this plan and recorded nothing: " + reason.Reason,
		Reasons: []plan.Refusal{reason}}
	assertPlanRefusal(t, pool, engine, key, gen, id,
		apply(id, ownersPlan(t, key, gen, 3, givenTask(t, "Build the widget.", "implementer", "tester", "reviewer"))), want, before)
}

// The project's default steps after a change of owner are named from the lasting record of the
// names keys held, but they cannot be refused as an owner's version is. LEGION-209's first owner,
// the architect of LEGION-208, gave `implementer tester:k reviewer:k`, key k named tester, whose
// claim records key k's session; LEGION-209 is then admitted as a root of its own, its first step
// passes over the earlier owner's plan and deletes its rows, and it runs the default steps, whose
// tester step takes the name tester all the same. So the step is written with that claim's session
// to leave (record.PlanStep.LeaveSession), its start carries it (record.SuperviseRequest's
// LeaveSession), and the issue and the controller are told once that the agent starts in a session
// of its own, which was key k's. The reviewer's step, whose name no key held, carries none.
//
// The issue's next run takes the default steps again under the same owner, and nobody is told a
// second time. Where the claim still records key k's session, since no start of the tester ran to
// leave it, the tester's step leaves it again; where the claim records a session of its own, the
// tester's step leaves nothing.
func TestTheDefaultStepsAfterAChangeOfOwnerLeaveTheSessionOfAKeysName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session string
		leaves  string
	}{
		{"the claim still records key k's session at the next run", "ses-key-k", "ses-key-k"},
		{"the claim records a session of its own at the next run", "ses-own", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			engine.cfg.DefaultSteps = []plan.Step{{Role: "tester"}, {Role: "reviewer"}}
			apply := applyFacts(t, pool, engine)
			const key = "LEGION-209"
			recordedAt(t, pool, key, "LEGION-208")
			mustTake(t, "the first owner's version 1", apply(planID(key, 1, 1), ownersPlan(t, key, 1, 1, givenTask(t, "Build the widget.", "implementer", "tester:k", "reviewer:k"))))
			named := claim.Token(token(t, key, claim.RoleTester))
			if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
				state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
				values ($1, 'legion', $2, $2, 'reviewer', 3, 'ses-key-k', '/sessions/ses-key-k.jsonl', 'suspended', 0, 0, 0, 0, 'daemon')`,
				string(named), key); err != nil {
				t.Fatalf("seed key k's claim: %v", err)
			}
			recordedAt(t, pool, key, key)

			firstStep(t, pool, engine, key)

			steps := planStandingOf(t, pool, engine, key, 1).plan.Steps[defaultTask]
			if len(steps) != 2 || steps[0].Role != "tester" || steps[0].Agent != "tester" || steps[0].LeaveSession != "ses-key-k" || steps[1].LeaveSession != "" {
				t.Fatalf("the default steps after the change of owner are %+v, want the tester's step named tester leaving key k's session ses-key-k, and the reviewer's leaving none", steps)
			}
			var tester []record.SuperviseRequest
			for _, start := range superviseRowsOf(t, pool, key, "start") {
				if start.Role == claim.RoleTester {
					tester = append(tester, start)
				}
			}
			if len(tester) != 1 || tester[0].LeaveSession != "ses-key-k" {
				t.Fatalf("the tester's starts are %+v, want one carrying the session to leave, ses-key-k", tester)
			}
			toldOnce := func(when string) {
				t.Helper()
				var told []string
				for _, body := range messageBodies(t, pool) {
					if strings.Contains(body, "key k") {
						told = append(told, body)
					}
				}
				if len(told) != 1 || !strings.Contains(told[0], `"ses-key-k"`) {
					t.Fatalf("%s the issue was told %q, want once, naming key k and its session ses-key-k", when, told)
				}
				if got := noticesOf(t, pool, key, "session-lost"); len(got) != 1 {
					t.Fatalf("%s the architect was told %+v, want one session-lost notice", when, got)
				}
				var controller int
				if err := pool.QueryRow(t.Context(), `select count(*) from outbox where issue = $1 and kind = 'controller_notice' and payload->>'kind' = 'session-lost'`,
					key).Scan(&controller); err != nil || controller != 1 {
					t.Fatalf("%s the controller was told %d session-lost notices (%v), want one", when, controller, err)
				}
			}
			toldOnce("after the first run's default steps,")

			if _, err := pool.Exec(t.Context(), `update claims set session = $2 where token = $1`, string(named), tc.session); err != nil {
				t.Fatalf("set the claim's session: %v", err)
			}
			next := issueOf(t, pool, engine, key)
			next.Generation, next.Phase, next.Status = 2, phase.Admitted, "in_progress"
			seedIssue(t, pool, next)

			firstStep(t, pool, engine, key)

			steps = planStandingOf(t, pool, engine, key, 2).plan.Steps[defaultTask]
			if len(steps) != 2 || steps[0].Agent != "tester" || steps[0].LeaveSession != tc.leaves {
				t.Fatalf("the next run's default steps are %+v, want the tester's step named tester leaving %q", steps, tc.leaves)
			}
			toldOnce("after the next run's default steps,")
		})
	}
}
