package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// linter is a second role the deployment defines, which runs as the phase step as checker does.
const linter claim.Role = "linter"

// An owed start names the phase and the role, not the step, and every role the deployment defines
// runs as the phase step. The linter's start of step 3 of `implementer checker linter` was owed
// while the task stood there; the task now stands at the checker's step 2, at the same phase and
// generation. The pass's repair of a start that can no longer act (finishFencedStarts) ends it,
// since it is not the work of the step that runs (record.Issue.HoldsQueued), so the line offers it
// no slot and no linter starts. The control is the same row with the task at the linter's step 3,
// which the pass keeps.
func TestAnOwedStartOfAnotherStepsRoleIsEnded(t *testing.T) {
	const key = "LEGION-101"
	for _, tc := range []struct {
		name string
		at   int
		kept bool
	}{
		{"the linter's start with the task at the checker's step 2", 2, false},
		{"the control: the linter's start with the task at its step 3", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 4)
			seedSlotted(t, pool, "LEGION-100", "A")
			capMember(t, pool, "LEGION-100", key, phase.Step, "in_progress", "A1")
			inTx(t, pool, func(tx pgx.Tx) {
				ctx, records := context.Background(), record.NewStore()
				project, err := claim.ProjectToken(testProject)
				if err != nil {
					t.Fatal(err)
				}
				architect, err := claim.NewToken(project, key, claim.RoleArchitect)
				if err != nil {
					t.Fatal(err)
				}
				v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner, GivenBy: architect, GivenAt: fixedNow}
				task := record.PlanTask{Issue: key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
				steps := record.NameAgents(nil, nil, []record.PlanStep{
					{Issue: key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
					{Issue: key, Generation: 1, Task: "a", Position: 2, Role: string(checker), Type: "testing"},
					{Issue: key, Generation: 1, Task: "a", Position: 3, Role: string(linter), Type: "testing"},
				}, nil)
				if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
					t.Fatalf("seed the plan of %s: %v", key, err)
				}
				if err := records.MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: tc.at}, record.TaskRunning); err != nil {
					t.Fatalf("seed the task of %s at step %d: %v", key, tc.at, err)
				}
				if err := workflow.StartOrOwe(ctx, tx, records, key, record.SuperviseRequest{Op: "start", Tree: "LEGION-100", Role: linter,
					Generation: 1, Phase: phase.Step, Task: "Lint " + key + ".", Entry: 1}, fixedNow); err != nil {
					t.Fatalf("owe %s the linter's start: %v", key, err)
				}
			})
			if owed := owedStarts(t, pool, key); len(owed) != 1 || owed[0].role != linter {
				t.Fatalf("the starts %s owes before the pass = %#v, want the linter's", key, owed)
			}

			tick(t, pool, admission, "tick")

			linters := 0
			for _, start := range startsOf(t, pool, key) {
				if start.role == linter {
					linters++
				}
			}
			if kept := linters == 1; kept != tc.kept || linters > 1 {
				t.Fatalf("the linter's starts of %s after the pass, with the task at step %d: %d (%#v), want kept %t",
					key, tc.at, linters, startsOf(t, pool, key), tc.kept)
			}
		})
	}
}
