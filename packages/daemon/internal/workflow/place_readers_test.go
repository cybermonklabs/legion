package workflow

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// assertStandsAtStep fails unless the step key's task runs where it stands as the store holds it
// (stepAt of where it stands) is step n of role and typ, and the task's standing says so
// (standingText).
func assertStandsAtStep(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, at phase.Phase, n int, role stepRole, typ string) {
	t.Helper()
	issue, w := issueAndWalkOf(t, pool, engine, key)
	if step, ok := stepAt(w, issue, standingIn(at)); !ok || step.place.Place != n || step.role != role || string(step.typ) != typ {
		t.Fatalf("stepAt where the task stands = %+v, %t; want step %d, %s, %s", step, ok, n, role, typ)
	}
	text, err := standingText(issue, nil, w, nil, planTold{}, "")
	if want := fmt.Sprintf(" Step %d of %d: %s.", n, len(w.Steps), role); err != nil || !strings.Contains(text.String(), want) {
		t.Fatalf("the standing of %s = %q, %v; want it to say%s", role, text, err, want)
	}
}

// Each reader of where a task stands reads its place, not the first place of its phase: in
// `implementer checker linter` both checker and linter run as step, and with the task moved on to
// step 3 the step it runs is the linter's (stepAt of where it stands), and the linter's task says
// so (standingText). Moved back from step 3 to step 1, the step it runs is the implementer's, and
// its task says so. The known pass: in `implementer reviewer tester`, whose phases each stand
// once, the step of testing is step 3, the tester's.
func TestTheReadersOfWhereATaskStandsReadItsPlace(t *testing.T) {
	types := map[string]string{"checker": "testing", "linter": "testing"}

	t.Run("moved on to step 3", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 2, types, "implementer", "checker", "linter")
		move(t, pool, engine, root.Key, TriggerStepPassed, record.TaskPlace{}, record.PhaseRow{})

		assertStandsAtStep(t, pool, engine, root.Key, phase.Step, 3, "linter", "testing")
	})

	t.Run("moved back from step 3 to step 1", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 3, types, "implementer", "checker", "linter")
		move(t, pool, engine, root.Key, TriggerBackward, stepPlace(1), record.PhaseRow{})

		assertStandsAtStep(t, pool, engine, root.Key, phase.Implementing, 1, "implementer", "implementation")
	})

	t.Run("the known pass: phases that each stand once", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Testing)
		seedStepsAt(t, pool, root.Key, root.Generation, 3, nil, "implementer", "reviewer", "tester")

		issue, w := issueAndWalkOf(t, pool, engine, root.Key)
		if step, ok := stepAt(w, issue, standingIn(phase.Testing)); !ok || step.place.Place != 3 || step.role != "tester" {
			t.Fatalf("stepAt of testing = %+v, %t; want step 3, tester", step, ok)
		}
	})
}

// The record of a return names the place the work goes back to, not the first place of its phase
// and not where the task stood: in `implementer checker linter critic` the critic at step 4 moves
// back to step 3, and the end of step 4 is failed, returned to 3. The known pass: in
// `implementer reviewer`, a rejected review ends the reviewer's step failed, returned to 1.
func TestTheRecordOfAReturnNamesThePlaceItReturnsTo(t *testing.T) {
	endOfStep := func(t *testing.T, pool *pgxpool.Pool, key string, step int) record.StepRecord {
		t.Helper()
		var ends []record.StepRecord
		for _, write := range stepWrites(t, pool, key) {
			if isEnd(write) && write.Step == step {
				ends = append(ends, write)
			}
		}
		if len(ends) != 1 {
			t.Fatalf("the ends of step %d of %s = %+v, want one", step, key, ends)
		}
		return ends[0]
	}
	returnedTo := func(end record.StepRecord) int {
		if end.ReturnedTo == nil {
			return 0
		}
		return *end.ReturnedTo
	}

	t.Run("the critic at step 4 moves back to step 3", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 4, map[string]string{"checker": "testing", "linter": "review", "critic": "review"},
			"implementer", "checker", "linter", "critic")

		move(t, pool, engine, root.Key, TriggerBackward, stepPlace(3), record.PhaseRow{})

		assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(3))
		if end := endOfStep(t, pool, root.Key, 4); resultOf(end) != "failed" || returnedTo(end) != 3 {
			t.Fatalf("the end of step 4 = %s, returned to %d; want failed, returned to 3", resultOf(end), returnedTo(end))
		}
	})

	t.Run("the known pass: a rejected review returns to step 1", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		reviewRequestsChanges(t, pool, engine, root.Key)

		if end := endOfStep(t, pool, root.Key, 2); resultOf(end) != "failed" || returnedTo(end) != 1 {
			t.Fatalf("the end of step 2 = %s, returned to %d; want failed, returned to 1", resultOf(end), returnedTo(end))
		}
	})
}
