package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The owner's report of a step of its own is judged by whether that step runs and its agent is the
// owner (ownerStepRefusal), not by the claim the step's row names. An owner's step is started by
// the architect that owns the issue then (ownerAgent): the nearest one above whose claim holds its
// role, where the issue's own architect did not hold its role at the start. The route
// complete_step takes a report from the issue's own architect alone (record.OwnerClaim), so a check
// of the report's claim against the row's would leave such a step with no claim that can report
// it. On `implementer:owner tester:owner`, with step 1's row naming the architect of LEGION-207,
// the owner's report of step 1 moves the task to step 2, as it does where the row names the owner.
//
// The control is the same report where the row names the owner, which moves the task as well; the
// case is red where the engine holds the owner's report to the row's claim
// (HANDOFF_NOT_THE_STEPS_WORKER).
func TestTheOwnersReportIsTakenWhateverArchitectItsStepsRowNames(t *testing.T) {
	owner := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect))
	above := claim.Token(treeRoleToken(t, "LEGION-207", claim.RoleArchitect))
	for _, tc := range []struct {
		name string
		row  claim.Token
	}{
		{name: "the row names the architect above", row: above},
		{name: "control: the row names the owner", row: owner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "tester:owner")
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			if _, err := pool.Exec(t.Context(), "update phases set claim = $1 where issue = $2 and role = $3", string(tc.row), root.Key,
				string(claim.RoleImplementer)); err != nil {
				t.Fatalf("name %s on step 1's row: %v", tc.row, err)
			}
			if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.Claim != tc.row || row.StepTask == "" {
				t.Fatalf("step 1's row = %+v, want it to run a step under %s: this case proves nothing", row, tc.row)
			}

			result := applyFacts(t, pool, engine)("step-1", ownersReport(root.Key, owner, claim.RoleImplementer, 1, "implement-LEGION-208"))
			if result.Refusal != nil {
				t.Fatalf("the owner's report of step 1, whose row names %s, was refused: %+v", tc.row, result.Refusal)
			}
			assertWalksAt(t, pool, engine, root.Key, phase.Testing, stepPlace(2))
		})
	}
}
