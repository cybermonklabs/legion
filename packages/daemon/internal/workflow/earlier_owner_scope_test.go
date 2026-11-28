package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// movedCopy is a store whose reading of the Dispatch chains (Store.ChainRowsOf) holds LEGION-210
// right under LEGION-208, as the copy holds it once Dispatch moved it out from under LEGION-209,
// while the database's copy still holds it under LEGION-209: a store with a reading of its own, as
// the daemon hands the engine a store of its own (internal/daemon's projectRecords). The read moves
// the row inside a savepoint it rolls back, so the walk is the store's own and nothing of the move
// outlives it.
type movedCopy struct {
	record.Store
}

func (s movedCopy) ChainRowsOf(ctx context.Context, tx pgx.Tx, keys []string) ([]record.ChainRow, error) {
	if _, err := tx.Exec(ctx, `savepoint moved_copy`); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `update dispatch_issues set parent = 'LEGION-208' where key = 'LEGION-210'`); err != nil {
		return nil, err
	}
	rows, err := s.Store.ChainRowsOf(ctx, tx, keys)
	if _, undo := tx.Exec(ctx, `rollback to savepoint moved_copy`); err == nil {
		err = undo
	}
	return rows, err
}

// A give of a plan before an issue's first step, the wait of a plan given at generation 0, and
// that first step judge an earlier owner by one read of the issue's Dispatch chain: the engine's
// store, which the daemon hands the engine (movedCopy here), as the routes and the owner's
// judgement under the fact lock read it. Here LEGION-210 stands under LEGION-209, under LEGION-208,
// and LEGION-209's architect gave version 1 of LEGION-210's plan while the engine read the
// database's copy; the engine then reads a copy in which Dispatch moved LEGION-210 right under
// LEGION-208, so that architect owns LEGION-210 no more, and its version is an earlier owner's to
// all three:
//   - the root's architect's give of version 1 sets it aside and stores the root's own version;
//   - the first step passes it over and runs the default steps;
//   - a plan the root's architect gives at generation 0 waits for the first step, which passes
//     the version over and runs the plan of generation 0.
//
// The faults it catches are a judgement through a read of its own, each in its row: the give's
// (its control: versionRefusal judging through record.NewStore(), under which version 1 names both
// architects), the first step's (its control: record.PlansOfEarlierOwners judging through
// record.NewStore(), under which the first step runs version 1 of LEGION-209's architect, in the
// second and the third row), and the wait's (its control: waitsAtZero judging through
// record.NewStore(), under which the plan of generation 0 is refused PLAN_STALE_GENERATION).
func TestTheGiveAndTheFirstStepJudgeAnEarlierOwnerByOneRead(t *testing.T) {
	t.Parallel()
	root, other := architectOf(t, "LEGION-208"), claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
	for _, tc := range []struct {
		name string
		then func(t *testing.T, pool *pgxpool.Pool, engine *Engine)
	}{
		{"the root's architect gives the version before the first step", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			mustTake(t, "the root's architect's version 1", applyFacts(t, pool, engine)(planID("LEGION-210", 1, 1),
				ownersPlan(t, "LEGION-210", 1, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
			if got := planStandingOf(t, pool, engine, "LEGION-210", 1).plan.Version.Givers(); !reflect.DeepEqual(got, []claim.Token{root}) {
				t.Errorf("version 1 of LEGION-210 names the givers %q, want the root's architect alone: the version of LEGION-209's architect set aside", got)
			}
		}},
		{"the first step", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			firstStep(t, pool, engine, "LEGION-210")
			if got := issueOf(t, pool, engine, "LEGION-210").Phase; got != phase.Planning {
				t.Errorf("LEGION-210 after its first step stands in %s, want planning, the first of the default steps: the version of LEGION-209's architect passed over", got)
			}
		}},
		{"a plan given at generation 0 before the first step", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			mustTake(t, "the root's architect's version 1 at generation 0", applyFacts(t, pool, engine)(planID("LEGION-210", 0, 1),
				ownersPlan(t, "LEGION-210", 0, 1, givenTask(t, "Build the widget.", "implementer", "tester"))))
			firstStep(t, pool, engine, "LEGION-210")
			if got := planStandingOf(t, pool, engine, "LEGION-210", 1).plan.Version.Givers(); !reflect.DeepEqual(got, []claim.Token{root}) {
				t.Errorf("the first step of LEGION-210 runs a version given by %q, want the root's architect's of generation 0, which waited for it", got)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			recordedAt(t, pool, "LEGION-208", "LEGION-208")
			recordedAt(t, pool, "LEGION-209", "LEGION-208")
			recordedAt(t, pool, "LEGION-210", "LEGION-209")
			given := ownersPlan(t, "LEGION-210", 1, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))
			given.GivenBy = other
			mustTake(t, "premise: version 1 of LEGION-209's architect, read on the database's copy", applyFacts(t, pool, engine)(planID("LEGION-210", 1, 1), given))

			engine.store = movedCopy{Store: engine.store}
			tc.then(t, pool, engine)
		})
	}
}
