package store

import (
	"context"
	"strings"
	"testing"
)

// mergeHoldsChangedVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const mergeHoldsChangedVersion = 20261126110259

// A merge-holds change is the row that tells the merger of an issue in merging that the project's
// merge holds document changed (record.MergeHoldsChanged). The outbox refuses a kind its
// constraint does not name, so on main's database the row could not be queued before this
// migration, and the transaction that applied the document's event would fail with it: the
// negative control is that refusal, read before the migration, on a database main migrated
// (migrateMainsThrough), which already carries the basis seeds' list (20261122154643), which this
// version stands above. On this stack's own order the row is queued before this migration already,
// and that expectation is the one that moved: the task copies' 20261121165047, the pull requests by
// task's 20261122122826 and the removal of a removed task's working copy's 20261123072431, numbered
// below this version, name merge_holds_changed, since the last migration to write the list owns
// every value of it, main's included, and on a database main migrated those three run late, after
// this one, as the last word on the check. After the migration the row is queued, and so is every
// kind the constraint already named, thirty-three in all, conflict_confirm, merge_forward,
// design_changes, phase_ended, pending_basis_seed and reparent_seed among them, since the migration
// rewrites the list whole and a kind left out would stop its own effect. A kind that is none is
// still refused, and so is a kind of the merge the daemon no longer submits.
func TestTheOutboxTakesTheMergeHoldsKindWith20261126110259(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ours := emptyStore(t)
	migrateThrough(t, ours, mergeHoldsChangedVersion-1)
	if err := queueRowOfKind(ours, "merge_holds_changed"); err != nil {
		t.Fatalf("queueing a merge_holds_changed row on this stack's database before the migration = %v, want it queued: 20261121165047, 20261122122826 and 20261123072431, numbered below it, name the kind, since the last migration to write the list owns every value of it, main's included", err)
	}
	t.Logf("on this stack's database before the migration a merge_holds_changed row is queued, by the lists of 20261121165047, 20261122122826 and 20261123072431")
	store := emptyStore(t)
	migrateMainsThrough(t, store, mergeHoldsChangedVersion-1)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	err := queue("merge_holds_changed")
	if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a merge_holds_changed row on main's database before the migration = %v, want the kind constraint to refuse it: it is the row the migration exists for", err)
	}
	t.Logf("on main's database before the migration a merge_holds_changed row is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the merge-holds kind: %v", err)
	}
	kinds := []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish", "step_record", "plan_record",
		"conflict_confirm", "merge_forward", "design_changes", "pending_basis_seed", "reparent_seed",
		"merge_holds_changed",
	}
	if len(kinds) != 33 {
		t.Fatalf("the list names %d kinds, want the 33 the migration writes", len(kinds))
	}
	for _, kind := range kinds {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes all %d kinds, merge_holds_changed among them", len(kinds))
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}
}
