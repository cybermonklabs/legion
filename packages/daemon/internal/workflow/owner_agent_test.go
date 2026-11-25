package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// startAgentOf is the agent engine's startAgent names for the start of issue's step at at, read in
// a transaction of its own after the boot's open of each run no start counted (applyInTx).
func startAgentOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, issue record.Issue, at phase.Phase) claim.Token {
	t.Helper()
	var agent claim.Token
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		agent, err = engine.startAgent(ctx, tx, issue, at)
		return err
	})
	return agent
}

// A step of the key owner is run, and named on its record, by the architect that owns its issue
// when the step starts (ownerAgent), as an acceptance the owner does is (startAgent): the child's
// own architect while its claim holds its role, or while a start of it the outbox will act on is
// queued (the move into the step starts a failed one again, reviveArchitect), and the nearest
// architect above it whose claim holds its role otherwise, here the tree's. The owner by rule stays
// the child's own architect (record.OwnerClaim) in every case; only the agent that runs the step
// follows who can act.
//
// The child LEGION-209 runs `implementer reviewer:owner`. A move into its step 2 starts the step:
// its record names the child's own architect when that claim is idle or failed (and so started
// again by the move), and the tree's architect when the operator suspended it, which no start
// wakes. A run of step 2 no start of this release counted (the boot's open of it, upgradedStart,
// through startAgent) names the tree's architect when the child's own has failed, since the boot's
// open starts nothing (the daemon starts a failed owner again at its own step only when its claim
// fails there, restartFailedOwner). The control is the step's agent read as the owner by rule
// (record.OwnerClaim): the suspended and the failed cases then name the child's own architect,
// which does not act, and the test fails on them.
func TestAStepOfTheOwnerIsRunByTheArchitectThatOwnsTheIssueNow(t *testing.T) {
	tree, own := treeRoleToken(t, "LEGION-208", claim.RoleArchitect), treeRoleToken(t, "LEGION-209", claim.RoleArchitect)
	seed := func(t *testing.T, pool *pgxpool.Pool, at phase.Phase, place int, state supervise.ClaimState, by supervise.SuspendSource) {
		t.Helper()
		stepRoot(t, pool, phase.Implementing)
		stepChild(t, pool, "LEGION-209", at, nil)
		seedChildPR(t, pool, record.PullRequestOpen)
		seedStepsAt(t, pool, "LEGION-209", 3, place, nil, "implementer", "reviewer:owner")
		seedArchitectClaimIn(t, pool, "LEGION-1", "LEGION-209", 1, state, by)
	}

	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		by    supervise.SuspendSource
		want  string
	}{
		{name: "the child's own architect holds its role", state: supervise.StateIdle, want: own},
		{name: "the child's own architect failed, which the move into the step starts again", state: supervise.StateFailed, want: own},
		{name: "the operator suspended the child's own architect", state: supervise.StateSuspended, by: supervise.SuspendedByOperator, want: tree},
	} {
		t.Run("a move starts the step: "+tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := stepEngine(nil)
			seed(t, pool, phase.Implementing, 1, tc.state, tc.by)
			move(t, pool, engine, "LEGION-209", TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
			writes := writesAt(stepWrites(t, pool, "LEGION-209"), "step", 2)
			if len(writes) != 1 || !isStart(writes[0]) {
				t.Fatalf("the writes of LEGION-209's step 2 = %+v, want its one start", writes)
			}
			if writes[0].Agent != tc.want {
				t.Fatalf("the start of LEGION-209's step reviewer:owner names %q, want %q, the architect that owns the child now", writes[0].Agent, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		want  string
	}{
		{name: "the child's own architect holds its role", state: supervise.StateIdle, want: own},
		{name: "the child's own architect failed", state: supervise.StateFailed, want: tree},
	} {
		t.Run("a run no start counted: "+tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := stepEngine(nil)
			seed(t, pool, phase.Reviewing, 2, tc.state, "")
			issue := issueOf(t, pool, engine, "LEGION-209")
			if got := startAgentOf(t, pool, engine, issue, phase.Reviewing); string(got) != tc.want {
				t.Fatalf("the agent of LEGION-209's step reviewer:owner = %q, want %q, the architect that owns the child now", got, tc.want)
			}
			if owner, err := record.OwnerClaim(issue); err != nil || string(owner) != own {
				t.Fatalf("the owner of LEGION-209's task = %q, %v; want its own architect %q, whatever architect runs the step", owner, err, own)
			}
		})
	}
}
