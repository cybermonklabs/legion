package daemon

import (
	"context"
	"encoding/json"
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
		return records.PutPhase(context.Background(), tx, row)
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
	payload := map[string]any{}
	encoded, err := json.Marshal(firedHook)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	delete(payload, "claim")
	before, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	payload["claim"] = string(named)
	naming, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	// Each row is made now, as the store makes a row it enqueues (created_at): a hook prompt ends at
	// the hour after its row was made (keptRoleMessageEnded), and a row made at the zero time is past it.
	row := record.OutboxRow{ID: 70, Issue: "LEGION-208", Kind: record.OutboxKindHookPrompt, NextAt: time.Now(), CreatedAt: time.Now(), Payload: naming}
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("the hook prompt naming %s: %v", named, err)
	}
	if published := publisher.publishes(); len(published) != 1 || published[0].topic != notify.RoleTopicPrefix+string(named) {
		t.Fatalf("the hook prompt naming %s was published as %+v, want once on its topic", named, published)
	}

	old := record.OutboxRow{ID: 71, Issue: "LEGION-208", Kind: record.OutboxKindHookPrompt, NextAt: time.Now(), CreatedAt: time.Now(), Payload: before}
	if err := runner.execute(ctx, old); err != nil {
		t.Fatalf("the hook prompt of the release before: %v", err)
	}
	if published := publisher.publishes(); len(published) != 2 || published[1].topic != notify.RoleTopicPrefix+string(later) {
		t.Fatalf("the hook prompt of the release before was published as %+v, want it on %s, the worker on the implementer's step", published, later)
	}
}
