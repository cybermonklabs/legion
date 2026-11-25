package workflow

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// countedClaim is the implementer's claim of the issue these tests count, on its phases row and
// live in the state they project, so the state shows the implementer's view.
const countedClaim = "implement-claim"

// countedReturns is what three reviews that requested changes and two failed tests count, at
// review_round_cap 3 (stepEngine): five passes, three rounds, the cap told at 3, and each move
// under its own pair.
var countedReturns = record.ReturnCounts{Pass: 5, Rounds: 3, CapTold: 3,
	BackwardMoves: map[string]int{"reviewing-implementing": 3, "testing-implementing": 2}}

// testsFail sends the work of key, in implementing, to its tester and back failed, times times.
func testsFail(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, times int) {
	t.Helper()
	for range times {
		move(t, pool, engine, key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, key, TriggerTesterFailed, record.TaskPlace{}, record.PhaseRow{})
	}
}

// reviewsReject sends the work of key, in implementing, past its tester to its reviewer and back
// with changes requested, times times.
func reviewsReject(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, times int) {
	t.Helper()
	for range times {
		move(t, pool, engine, key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, key, TriggerReviewRejected, record.TaskPlace{}, record.PhaseRow{})
	}
}

// implementerCounts is the counts of returns key's implementer's phases row holds.
func implementerCounts(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) record.ReturnCounts {
	t.Helper()
	var row record.PhaseRow
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := engine.store.Phases(ctx, tx, key)
		row = record.PhaseRowOf(rows, claim.RoleImplementer)
		return err
	})

	return record.ReturnCounts{Pass: row.Pass, Rounds: row.Rounds, ConflictRounds: row.ConflictRounds, BackwardMoves: row.BackwardMoves, CapTold: row.CapTold}
}

// sameReturnCounts compares two counts of returns, an empty map of moves equal to none.
func sameReturnCounts(got, want record.ReturnCounts) bool {
	if len(got.BackwardMoves) == 0 && len(want.BackwardMoves) == 0 {
		got.BackwardMoves, want.BackwardMoves = nil, nil
	}
	return reflect.DeepEqual(got, want)
}

// countsAsToday says why key's counts are not want on its implementer's phases row with no task
// row of the plan beside them, nil when they are: where an issue with no plan rows keeps them.
func countsAsToday(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, want record.ReturnCounts) error {
	t.Helper()
	if got := implementerCounts(t, pool, engine, key); !sameReturnCounts(got, want) {
		return fmt.Errorf("the implementer's phases row of %s counts %+v, want %+v", key, got, want)
	}
	var tasks int
	if err := pool.QueryRow(t.Context(), "select count(*) from plan_tasks where issue = $1", key).Scan(&tasks); err != nil {
		return err
	}
	if tasks != 0 {
		return fmt.Errorf("%s has %d plan task rows, want none", key, tasks)
	}
	return nil
}

// implementerView is the implementer's view of key in the served state, its claim live.
func implementerView(t *testing.T, pool *pgxpool.Pool, key string) api.PhaseView {
	t.Helper()
	var state api.State
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		state, err = projection.Project(t.Context(), projection.Inputs{Tx: tx, Store: record.NewStore(), Project: "LEGION", Claims: []supervise.Claim{
			{Token: countedClaim, Issue: key, Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "ses_implementer"},
		}, Now: stepClock})
		return err
	}); err != nil {
		t.Fatalf("project the state: %v", err)
	}
	return state.Issues[key].Workers[claim.RoleImplementer]
}

// An issue with plan rows counts each return on its task: the pass, the rounds, the cap
// told and each move back under its pair live on the task's plan_tasks row, where the walk keeps
// where the task stands, and the implementer's phases row counts nothing. Three reviews that
// request changes and two failed tests on the plan `implementer tester reviewer` count five
// passes and three rounds there, and the cap of 3 is told once. The served state's implementer
// view shows the task's counts.
func TestAnIssueWithPlanRowsCountsOnItsTask(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := stepEngine(nil)
	root := stepRoot(t, pool, phase.Admitted)
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: countedClaim})
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	testsFail(t, pool, engine, root.Key, 2)
	reviewsReject(t, pool, engine, root.Key, 3)

	var counts record.ReturnCounts
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		counts, err = engine.store.ReturnCounts(ctx, tx, root.Key, root.Generation, "a")
		return err
	})

	if !sameReturnCounts(counts, countedReturns) {
		t.Fatalf("the task of %s counts %+v, want %+v", root.Key, counts, countedReturns)
	}
	if got := implementerCounts(t, pool, engine, root.Key); !sameReturnCounts(got, record.ReturnCounts{}) {
		t.Fatalf("the implementer's phases row of %s counts %+v, want nothing: an issue with plan rows counts on its task", root.Key, got)
	}
	if view := implementerView(t, pool, root.Key); view.Pass != 5 || view.Rounds != 3 || !reflect.DeepEqual(view.BackwardMoves, countedReturns.BackwardMoves) {
		t.Fatalf("the served implementer view of %s counts pass %d, rounds %d, moves %v; want the task's 5, 3 and %v", root.Key, view.Pass, view.Rounds, view.BackwardMoves, countedReturns.BackwardMoves)
	}
}

// An issue with no plan rows counts as it did before plans: the same returns on today's line
// count on the implementer's phases row, and no task row is written. The control is the same
// issue given plan rows in the middle of its run: the returns after that count on the task, so
// the first check of this test, the implementer's row, fails for it.
func TestAnIssueWithNoPlanRowsCountsAsToday(t *testing.T) {
	t.Parallel()
	t.Run("no plan rows", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		root := stepRoot(t, pool, phase.Implementing)
		seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: countedClaim})
		testsFail(t, pool, engine, root.Key, 2)
		reviewsReject(t, pool, engine, root.Key, 3)
		if err := countsAsToday(t, pool, engine, root.Key, countedReturns); err != nil {
			t.Fatal(err)
		}
		if view := implementerView(t, pool, root.Key); view.Pass != 5 || view.Rounds != 3 || !reflect.DeepEqual(view.BackwardMoves, countedReturns.BackwardMoves) {
			t.Fatalf("the served implementer view of %s counts pass %d, rounds %d, moves %v; want the phases row's 5, 3 and %v", root.Key, view.Pass, view.Rounds, view.BackwardMoves, countedReturns.BackwardMoves)
		}
	})

	t.Run("control: plan rows given mid-run fail the check of the implementer's row", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepEngine(nil)
		root := stepRoot(t, pool, phase.Implementing)
		seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: countedClaim})
		testsFail(t, pool, engine, root.Key, 2)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
		reviewsReject(t, pool, engine, root.Key, 3)
		if got := implementerCounts(t, pool, engine, root.Key); sameReturnCounts(got, countedReturns) {
			t.Fatalf("the implementer's phases row of %s counts %+v, every return: the check cannot see the returns its plan rows count", root.Key, got)
		}
	})
}
