package store

import (
	"context"
	"strings"
	"testing"
)

// kindsAfterTheStateEventsVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const kindsAfterTheStateEventsVersion = 20261127104032

// The merge's union of the state events and the retention end (20261127104032) writes the kind
// check again with every kind of 20261126163320's list, and its version stands above every other,
// so the store's schema version (SchemaVersion), which each boot's state event carries, moves on
// a database whose newest version was 20261126163320: the first boot of the merge announces a
// schema version the earlier daemon did not, which is what has Dispatch read it whole (the
// migration's file says why). The negative control is the version read before the migration,
// 20261126163320's own. A row of each kind, a state row and a retention_end among them, pending
// before the migration is kept through it, and once those rows are gone (some kinds hold one row
// per project or issue) every kind is queued after it; a kind that is none is still refused.
func TestTheUnionKindListMovesTheSchemaVersionAboveTheStateEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, kindsAfterTheStateEventsVersion-1)
	if version, err := store.SchemaVersion(ctx); err != nil || version != stateEventsVersion {
		t.Fatalf("the schema version before the migration = %d, %v; want the state events' %d, the version the earlier daemon announces", version, err, stateEventsVersion)
	}
	kinds := []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish", "step_record", "plan_record",
		"conflict_confirm", "merge_forward", "design_changes", "pending_basis_seed", "reparent_seed",
		"merge_holds_changed", "retention_end", "state_change", "state_admission", "state_boot",
	}
	if len(kinds) != 37 {
		t.Fatalf("the list names %d kinds, want the 37 the constraint names", len(kinds))
	}
	queue := func(kind string) error {
		issue := "LEGION-208"
		if strings.HasPrefix(kind, "state_") {
			issue = "LEGION"
		}
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, '{}', 0, now(), '')`, kind, issue)
		return err
	}
	for _, kind := range kinds {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row before the migration: %v", kind, err)
		}
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the union of the kind lists: %v", err)
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != kindsAfterTheStateEventsVersion {
		t.Fatalf("the schema version after the migration = %d, %v; want %d, above the state events'", version, err, kindsAfterTheStateEventsVersion)
	}
	for _, kind := range kinds {
		var pending int
		if err := store.Pool().QueryRow(ctx, `select count(*) from outbox where kind = $1`, kind).Scan(&pending); err != nil {
			t.Fatalf("count the %s rows the migration left: %v", kind, err)
		}
		if pending != 1 {
			t.Fatalf("%s rows after the migration = %d, want the one queued before it kept", kind, pending)
		}
	}
	if _, err := store.Pool().Exec(ctx, `delete from outbox`); err != nil {
		t.Fatalf("clear the rows kept through the migration: %v", err)
	}
	for _, kind := range kinds {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
}
