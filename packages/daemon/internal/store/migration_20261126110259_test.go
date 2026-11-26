package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// mergeHoldsChangedVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const mergeHoldsChangedVersion = 20261126110259

// A merge-holds change is the row that tells the merger of an issue in merging that the project's
// merge holds document changed (record.MergeHoldsChanged). The outbox refuses a kind its
// constraint does not name, so the row could not be queued before this migration, and the
// transaction that applied the document's event would fail with it: the negative control is that
// refusal, read before the migration, on a database that already carries the basis seeds' list
// (20261122154643), which this version stands above: a database main migrated, so the answer
// loop's six migrations (answerLoopMigrations), which stand below this version and which no
// database main migrated records, are left out of it, since 20261124114223 among them admits the
// kind too (migration_upgrade_from_main_test.go says why), and the upgrade applies them with this
// one. After it the row is queued, and so is every
// kind the constraint already named, thirty-three in all, conflict_confirm, merge_forward,
// design_changes, phase_ended, pending_basis_seed and reparent_seed among them, since the migration
// rewrites the list whole and a kind left out would stop its own effect. A kind that is none is
// still refused, and so is a kind of the merge the daemon no longer submits.
func TestTheOutboxTakesTheMergeHoldsKindWith20261126110259(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version >= mergeHoldsChangedVersion || slices.Contains(answerLoopMigrations, migration.Version) {
			continue
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s as main's own run did: %v", migration.Name, err)
		}
	}
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	err = queue("merge_holds_changed")
	if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a merge_holds_changed row before the migration = %v, want the kind constraint to refuse it: it is the row the migration exists for", err)
	}
	t.Logf("before the migration a merge_holds_changed row is refused: %v", err)
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
