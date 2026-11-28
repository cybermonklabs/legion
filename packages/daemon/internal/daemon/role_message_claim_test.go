package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// putPhaseRow writes row to the phases record of pool.
func putPhaseRow(t *testing.T, pool *store.Pool, records record.Store, row record.PhaseRow) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutPhase(ctx, tx, row)
	}); err != nil {
		t.Fatalf("write the %s's phases row: %v", row.Role, err)
	}
}

// A hook prompt names the claim of the agent that works its role's step in the round the move
// back opened, and the outbox publishes on that claim's topic, also where a later entry of the
// role's step names another agent by the time the row runs. A prompt the release before wrote
// names no claim, and reaches the claim of the worker on its role's step when it runs.
func TestAHookPromptReachesTheClaimItNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	publisher := &outboxPublisher{}
	runner, _, pool := hookRunner(t, publisher)
	named := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	later := mustClaimToken(t, "LEGION-208", claim.RoleReviewer)
	putPhaseRow(t, pool, runner.records, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: later, StepTask: "t", StepPlace: 3, Entry: 4})
	withClaim := firedHook
	withClaim.Claim = named

	row := mustOutboxRow(t, "LEGION-208", withClaim, time.Now())
	// The hour a kept row waits runs from its making (keptRoleMessageEnded), and a row this test
	// builds holds the zero moment until it says otherwise, which is past every hour.
	row.ID, row.CreatedAt = 70, time.Now()
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("the hook prompt naming %s: %v", named, err)
	}
	if published := publisher.publishes(); len(published) != 1 || published[0].topic != notify.RoleTopicPrefix+string(named) {
		t.Fatalf("the hook prompt naming %s was published as %+v, want once on its topic", named, published)
	}

	old := mustOutboxRow(t, "LEGION-208", firedHook, time.Now())
	old.ID, old.CreatedAt = 71, time.Now()
	if err := runner.execute(ctx, old); err != nil {
		t.Fatalf("the hook prompt of the release before: %v", err)
	}
	if published := publisher.publishes(); len(published) != 2 || published[1].topic != notify.RoleTopicPrefix+string(later) {
		t.Fatalf("the hook prompt of the release before was published as %+v, want it on %s, the worker on the implementer's step", published, later)
	}
}
