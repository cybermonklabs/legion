package store

import (
	"context"
	"strings"
	"testing"
)

// roleHoldersPublishVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const roleHoldersPublishVersion = 20261116165926

// A role-holders publish row writes the live role sessions of one issue onto Dispatch
// (record.RoleHoldersPublish). The outbox refuses a kind its constraint does not name, so the row
// could not be queued before this migration, and the ready that queues it would fail with it: the
// negative control is that refusal, read before the migration, on a database that already carries
// the merge-queue kinds (20261114031356), which this version stands above. After it the row is
// queued, and so is every kind the constraint already named, since the migration rewrites the list
// whole and a kind left out would stop its own effect. A kind that is none is still refused.
//
// The partial index is the other half: one publish row per issue is what lets the enqueue make the
// pending row due again rather than queue beside it (EnqueueRoleHolders). It holds
// this kind's rows alone, so another issue's publish and another kind's second row of the same issue
// are both untouched by it.
func TestTheOutboxTakesTheRoleHoldersPublishKindWith20261116165926(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, roleHoldersPublishVersion-1)
	queue := func(kind, issue string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, '{}', 0, now(), '')`, kind, issue)
		return err
	}
	err := queue("role_holders_publish", "LEGION-208")
	if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a role_holders_publish row before the migration = %v, want the kind constraint to refuse it: it is the row the migration exists for", err)
	}
	t.Logf("before the migration the role-holders publish is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the role-holders publish kind: %v", err)
	}
	kinds := []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish",
	}
	if len(kinds) != 25 {
		t.Fatalf("the list names %d kinds, want the 25 the daemon wrote at 20261116165926", len(kinds))
	}
	for _, kind := range kinds {
		if err := queue(kind, "LEGION-208"); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migrations the outbox takes all 25 kinds of 20261116165926, role_holders_publish among them")
	if err := queue("quiesce", "LEGION-208"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge", "LEGION-208"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}

	err = queue("role_holders_publish", "LEGION-208")
	if err == nil || !strings.Contains(err.Error(), "outbox_role_holders_publish_issue_unique") {
		t.Fatalf("a second role-holders publish of one issue = %v, want the partial index to refuse it", err)
	}
	t.Logf("the second publish of one issue is refused: %v", err)
	if err := queue("role_holders_publish", "LEGION-209"); err != nil {
		t.Fatalf("queue the role-holders publish of another issue: %v", err)
	}
	if err := queue("notice", "LEGION-208"); err != nil {
		t.Fatalf("queue a second notice of the issue that has a publish pending: %v", err)
	}
}
