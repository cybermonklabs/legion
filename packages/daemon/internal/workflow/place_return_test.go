package workflow

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// stepPlace is the step at place n of a task.
func stepPlace(n int) record.TaskPlace { return record.TaskPlace{Kind: record.PlaceStep, Position: n} }

// seedStepsAt writes the plan of key's run whose task a has the steps words, with the type of
// each role in types stored for it (the plan's rows as a deployment that defines those roles
// stores them), and its task running at step at.
func seedStepsAt(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, at int, types map[string]string, words ...string) {
	t.Helper()
	seedStepsPlan(t, pool, key, generation, words...)
	for role, typ := range types {
		if _, err := pool.Exec(t.Context(), "update plan_steps set type = $3 where issue = $1 and task = 'a' and role = $2", key, role, typ); err != nil {
			t.Fatalf("store the type of %s: %v", role, err)
		}
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return seedMove(t.Context(), tx, key, generation, "a", stepPlace(at), record.TaskRunning)
	})
}

// issueAndWalkOf is key as the store holds it now and the walk of its run.
func issueAndWalkOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) (record.Issue, Walk) {
	t.Helper()
	return issueOf(t, pool, engine, key), walkOf(t, pool, engine, key)
}

// A failed step is judged by where it stands, not by the first place of its phase: in
// `implementer checker linter critic` three steps run as step, and the critic at step 4 may
// name step 3, which the fail then returns the work to, while step 4 itself is not before the
// step that failed. In `implementer checker linter`, the linter at step 3 may name step 2.
func TestAFailedStepReturnsToThePlaceItNames(t *testing.T) {
	types := map[string]string{"checker": "testing", "linter": "review", "critic": "review"}

	t.Run("the critic at step 4", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 4, types, "implementer", "checker", "linter", "critic")

		issue, w := issueAndWalkOf(t, pool, engine, root.Key)
		from := placeNamed(w.Place, issue.Phase)
		got, why := returnStep(issue, w, from, 3, failReport)
		if why != "" || got != stepPlace(3) {
			t.Fatalf("returnStep(3) at step 4 = %+v, %q; want step 3 and no refusal", got, why)
		}
		const refusal = "returnTo is 4, and step 4 (critic) of task a of LEGION-208 is not before step 4 (critic), the step that failed; " +
			"send the fail again naming an earlier step, or without returnTo; nothing was recorded"
		if got, why := returnStep(issue, w, from, 4, failReport); why != refusal || got != (record.TaskPlace{}) {
			t.Fatalf("returnStep(4) at step 4 = %+v, %q; want the refusal %q", got, why, refusal)
		}
		// A move back is judged by the same judge, in its own words.
		const moveRefusal = "returnTo is 4, and step 4 (critic) of task a of LEGION-208 is not before step 4 (critic), the step that moves back; " +
			"send the move again naming an earlier step, or without returnTo; nothing was recorded"
		if _, why := returnStep(issue, w, from, 4, backwardMove); why != moveRefusal {
			t.Fatalf("returnStep(4) of a move back at step 4 refuses %q, want %q", why, moveRefusal)
		}

		move(t, pool, engine, root.Key, TriggerStepFailed, stepPlace(3), record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(3))
	})

	t.Run("the linter at step 3", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 3, types, "implementer", "checker", "linter")

		issue, w := issueAndWalkOf(t, pool, engine, root.Key)
		if got, why := returnStep(issue, w, placeNamed(w.Place, issue.Phase), 2, failReport); why != "" || got != stepPlace(2) {
			t.Fatalf("returnStep(2) at step 3 = %+v, %q; want step 2 and no refusal", got, why)
		}
	})
}

// A failed step with no step of the type implementation before it stops its task, and the stop
// names the step that failed by where it stands: in `critic linter implementer` the linter at
// step 2 fails, and the critic at step 1, of the type review, cannot take the work back, so task a
// stops naming step 2 (linter), the issue is held from step for the stopped task, and the owner is
// told once, in a notice that names the linter. The critic itself, at step 1, has no step before
// it at all, and its fail stops the task the same way.
func TestAStepWithNoImplementationBeforeItStopsNamingItsPlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   int
		role claim.Role
	}{{"the linter at step 2", 2, "linter"}, {"the critic at step 1", 1, "critic"}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			root := stepRoot(t, pool, phase.Step)
			seedStepsAt(t, pool, root.Key, root.Generation, tc.at, map[string]string{"critic": "review", "linter": "testing"}, "critic", "linter", "implementer")

			move(t, pool, engine, root.Key, TriggerStepFailed, record.TaskPlace{}, record.PhaseRow{})

			why := fmt.Sprintf("step %d (%s) failed and no earlier step of the type implementation can take the work", tc.at, tc.role)
			if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.StoppedReason != why || task.Place != stepPlace(tc.at) {
				t.Fatalf("task a of %s = %s at %+v stopped with %q, want stopped at step %d with %q", root.Key, task.State, task.Place, task.StoppedReason, tc.at, why)
			}
			want := record.Hold{From: phase.Step, Reason: record.HoldTaskStopped}
			if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != want {
				t.Fatalf("%s stands in %s with hold %+v, want held %+v", root.Key, issue.Phase, issue.Hold, want)
			}
			if roles := rolesOf(noticesOf(t, pool, root.Key, "task-stopped")); !reflect.DeepEqual(roles, []claim.Role{tc.role}) {
				t.Fatalf("the task-stopped notices name %v, want the %s once", roles, tc.role)
			}
		})
	}
}

// rolesOf is the role each notice names, in order.
func rolesOf(notices []record.Notice) []claim.Role {
	roles := make([]claim.Role, len(notices))
	for i, notice := range notices {
		roles[i] = notice.Role
	}
	return roles
}
