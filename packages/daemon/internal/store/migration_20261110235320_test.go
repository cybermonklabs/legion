package store

import (
	"context"
	"strings"
	"testing"
)

// designGatesPublishVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const designGatesPublishVersion = 20261110235320

// A design-gates publish row writes the documents an issue's design-gate steps record onto its
// Dispatch row (record.DesignGatesPublish). The outbox refuses a kind its constraint does not
// name, so the row could not be queued before this migration, and the transaction that changed
// the gate would fail with it: the negative control is that refusal, read before the migration.
// After it the row is queued, and so is every kind the constraint already named, since the
// migration rewrites the list whole and a kind left out would stop its own effect. A kind that is
// none is still refused.
//
// The partial index is the other half: one publish row per issue is what lets the enqueue replace
// the pending row's payload rather than queue beside it (record.Postgres.EnqueueDesignGates). It
// holds this kind's rows alone, so another issue's publish and another kind's second row of the
// same issue are both untouched by it.
func TestTheOutboxTakesTheDesignGatesPublishKindWith20261110235320(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, designGatesPublishVersion-1)
	queue := func(kind, issue string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, '{}', 0, now(), '')`, kind, issue)
		return err
	}
	err := queue("design_gates_publish", "LEGION-208")
	if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a design_gates_publish row before the migration = %v, want the kind constraint to refuse it: it is the row the migration exists for", err)
	}
	t.Logf("before the migration the design-gates publish is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the design-gates publish kind: %v", err)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish",
	} {
		if err := queue(kind, "LEGION-208"); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes every kind, design_gates_publish among them")
	if err := queue("quiesce", "LEGION-208"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge", "LEGION-208"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}

	err = queue("design_gates_publish", "LEGION-208")
	if err == nil || !strings.Contains(err.Error(), "outbox_design_gates_publish_issue_unique") {
		t.Fatalf("a second design-gates publish of one issue = %v, want the partial index to refuse it", err)
	}
	t.Logf("the second publish of one issue is refused: %v", err)
	if err := queue("design_gates_publish", "LEGION-209"); err != nil {
		t.Fatalf("queue the design-gates publish of another issue: %v", err)
	}
	if err := queue("notice", "LEGION-208"); err != nil {
		t.Fatalf("queue a second notice of the issue that has a publish pending: %v", err)
	}
}
