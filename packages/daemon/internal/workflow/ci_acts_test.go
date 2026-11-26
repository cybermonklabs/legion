package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The owner's acts before its task's CI passes are one value (ciActs), from which the ci-failed
// notice, the notice of a fix whose branch cannot be named, the retro-held notice of a hold for CI
// and the CI_PENDING refusal each offer them (ciActsOf): the move back always, and stop_task only
// where the stop is taken, by the one predicate the stop's refusal reads (takesNoStop). A group that
// runs the project's default steps has plan rows, its default task, and is offered no stop, since
// its stop_task answers PLAN_NOT_TAKEN; a plan's task, in the group whose owner gives the plan, is
// offered the stop. The check against the refusal itself: each case's stop_task is applied, and it
// is refused PLAN_NOT_TAKEN exactly where the stop is not offered.
func TestTheStopIsOfferedOnlyWhereItIsTaken(t *testing.T) {
	for _, tc := range []struct {
		name, arm, plan string
		want            ciActs
	}{
		{name: "a default-steps group", arm: "b", plan: "default", want: ciActs{}},
		{name: "a plan's task", arm: "a", plan: "owner", want: ciActs{Stop: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			drawnInto(t, pool, root.Key, "planning", tc.arm, tc.plan)
			firstStep(t, pool, engine, root.Key)

			var got ciActs
			onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
				w, err := engine.walk(ctx, tx, issue)
				if err != nil {
					return err
				}
				if !w.Stored {
					return fmt.Errorf("%s has no plan rows after its first step", issue.Key)
				}
				got, err = engine.ciActsOf(ctx, tx, issue, w)
				return err
			})
			if got != tc.want {
				t.Fatalf("the acts offered = %+v, want %+v", got, tc.want)
			}

			result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, plan.DefaultTask))
			if refused := result.Refusal != nil && result.Refusal.Code == "PLAN_NOT_TAKEN"; refused == got.Stop {
				t.Fatalf("stop_task offered: %v; the stop answered %+v: the offer and the refusal disagree", got.Stop, result.Refusal)
			}
		})
	}
}
