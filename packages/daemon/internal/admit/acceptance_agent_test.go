package admit

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// runAtStep writes key's plan of one task, a, at generation, from words (such as `implementer
// acceptance`, or `implementer acceptance:owner` for the owner's acceptance), its agents named as
// a given plan names them (record.NameAgents), and its task running at step position.
func runAtStep(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, position int, words ...string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		v := record.PlanVersion{Issue: key, Generation: generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner, GivenBy: ownersArchitect(t, key), GivenAt: fixedNow}
		task := record.PlanTask{Issue: key, Generation: generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		roles := plan.BuiltinRoles()
		steps := make([]record.PlanStep, 0, len(words))
		for i, word := range words {
			role, agent, _ := strings.Cut(word, ":")
			steps = append(steps, record.PlanStep{Issue: key, Generation: generation, Task: "a", Position: i + 1, Role: role, AgentKey: agent, Type: roles[role].Type})
		}
		for _, put := range []error{
			records.PutPlan(ctx, tx, v, []record.PlanTask{task}, record.NameAgents(nil, nil, nil, steps)),
			records.MoveTask(ctx, tx, key, generation, "a", record.TaskPlace{Kind: record.PlaceStep, Position: position}, record.TaskRunning),
		} {
			if put != nil {
				t.Fatalf("seed step %d of %s: %v", position, key, put)
			}
		}
	})
}

// Admission's repair of a role behind the entry its phases row names (resumeSlottedWorkers,
// through owedEntryRoles and phaseWorkerOf) reaches the fresh agent of acceptance that lost its
// task: a member at accepting on `implementer acceptance`, past its review and holding no slot,
// whose agent of the step `acceptance` with no key holds no claim, is sent the work of the entry
// its phases row names, as the checker of a step is. Under `implementer acceptance:owner` the
// place is the owner's, which no phase worker works, and the same row starts nothing.
func TestTheEntryRepairResumesAFreshAcceptanceAgentThatLostItsTask(t *testing.T) {
	for _, tc := range []struct {
		name   string
		words  []string
		starts bool
	}{
		{name: "the fresh agent's acceptance", words: []string{"implementer", "acceptance"}, starts: true},
		{name: "the owner's acceptance", words: []string{"implementer", "acceptance:owner"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			const root, key = "LEGION-760", "LEGION-761"
			seedTreeAndMember(t, pool, root, key, phase.Accepting, 1, false)
			runAtStep(t, pool, key, 1, 2, tc.words...)
			token := keyedToken(t, key, claim.Agent(claim.RoleAcceptance))
			inTx(t, pool, func(tx pgx.Tx) {
				if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: key, Role: claim.RoleAcceptance, Claim: token,
					Entry: seededEntry, EntryTask: seededEntryTask(key), EntryRaisedAt: fixedNow}); err != nil {
					t.Fatalf("record the acceptance's phases row of %s: %v", key, err)
				}
			})

			tick(t, pool, quiet(t, 1), "converge-pass-1")

			starts := convergeStarts(t, pool, key)
			if !tc.starts {
				if len(starts) != 0 {
					t.Fatalf("the start rows of %s = %+v, want none: the owner works its acceptance, and no phase worker does", key, starts)
				}
				return
			}
			if want := (convergeStart{role: claim.RoleAcceptance, task: seededEntryTask(key), entry: seededEntry}); len(starts) != 1 ||
				(convergeStart{role: starts[0].role, task: starts[0].task, entry: starts[0].entry}) != want {
				t.Fatalf("the start rows of %s = %+v, want the one start of its agent of acceptance carrying the work of entry %d: that agent holds no claim, so it was given nothing", key, starts, seededEntry)
			}
		})
	}
}
