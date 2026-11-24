package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// standAtOwnersStep moves key, a member of the finishing tree (seedFinishingTree), to implementing
// on the plan `implementer:owner reviewer`, its task a running at step 1, the step its owner does.
func standAtOwnersStep(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		member, err := records.Issue(ctx, tx, key)
		if err != nil || member == nil {
			t.Fatalf("read %s: %v", key, err)
		}
		member.Phase, member.Hold, member.Status, member.DispatchStatus = phase.Implementing, nil, "in_progress", "in_progress"
		if err := records.PutIssue(ctx, tx, *member); err != nil {
			t.Fatalf("put %s at implementing: %v", key, err)
		}
	})
	runAtStep(t, pool, key, 1, 1, "implementer:owner", "reviewer")
}

// A tree admitted again tells the owner of a member that stands at a step the owner does that step
// again, once: the promotion starts no worker of a place the owner works (startMidPhaseMembers), and
// its own start of the member's step (workflow's StartStepRecord) starts it as every start of an
// owner's step does, its entry raised under the owner's claim with the phase's resume task, its
// run's one start record, and one notice owner-step that carries that task. The control is the
// promotion's start as it was, the run's start record alone: the owner is told nothing, and learns
// of its step only from its catch-up.
func TestATreeAdmittedAgainTellsTheOwnerItsStep(t *testing.T) {
	pool, admission, engine := seedFinishingTree(t)
	const atStep = planning
	closeFinishingTree(t, pool, admission, engine)
	standAtOwnersStep(t, pool, atStep)
	reopenFinishingTree(t, pool, admission, engine)

	if root := issue(t, pool, finishedRoot); root.Generation != 2 || root.Lingers() {
		t.Fatalf("the reopened root = %#v, want generation 2, no longer lingering: the promotion this case reads did not run", root)
	}
	if got := issue(t, pool, atStep); got.Phase != phase.Implementing || got.PlaceRole != claim.RoleArchitect {
		t.Fatalf("%s after the promotion = %s with PlaceRole %q, want implementing with the architect", atStep, got.Phase, got.PlaceRole)
	}
	told := gateNotices(t, pool, atStep, record.OwnerStepNotice)
	if len(told) != 1 || told[0].Role != claim.RoleImplementer || told[0].Phase != phase.Implementing {
		t.Fatalf("the promotion told %+v, want one owner-step of the implementer's step in implementing", told)
	}
	if step := told[0].OwnerStep; step == nil || step.Task != "a" || step.Kind != "step" || step.Place != 1 {
		t.Fatalf("the owner-step names the run %+v, want task a's step 1", told[0].OwnerStep)
	}
	if starts := roleStarts(t, pool, atStep, claim.RoleImplementer); len(starts) != 0 {
		t.Fatalf("the promotion wrote the implementer's starts %+v, want none: the owner does the step", starts)
	}
	writes := stepWrites(t, pool, atStep)
	if len(writes) != 1 || writes[0].Task != "a" || writes[0].Kind != "step" || writes[0].Step != 1 || writes[0].EndedAt != nil {
		t.Fatalf("the step records of %s = %+v, want the one start of step 1 of task a", atStep, writes)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		rows, err := record.NewStore().Phases(context.Background(), tx, atStep)
		if err != nil {
			t.Fatalf("read the phases of %s: %v", atStep, err)
		}
		row := record.PhaseRowOf(rows, claim.RoleImplementer)
		if row.Entry != 1 || row.StepPlace != 1 || row.EntryTask != told[0].Reason || claim.Token(writes[0].Agent) != row.Claim {
			t.Fatalf("the implementer's row of %s = %+v, want entry 1 of step 1 under the record's agent %s, holding the task told", atStep, row, writes[0].Agent)
		}
	})
}
