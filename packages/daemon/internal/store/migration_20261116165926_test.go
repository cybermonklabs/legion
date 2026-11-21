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
// (record.RoleHoldersPublish). The outbox refuses a kind its constraint does not name, but the kind
// is no control here: the unreachable claims' 20261116082000 stands below this migration and
// already names `role_holders_publish`, as it has to, so that a database that recorded this
// version before it keeps its pending publications when it applies that list last. The row is
// written before the migration for that reason, on an issue of its own, and the negative control is
// the partial index, which does not exist before the migration. After it the row is queued, and so
// is every kind the constraint already named, since the migration rewrites the list whole and a
// kind left out would stop its own effect. A kind that is none is still refused.
//
// The partial index is the other half: one publish row per issue is what lets the enqueue make the
// pending row due again rather than queue beside it (EnqueueRoleHolders). It holds
// this kind's rows alone, so another issue's publish and another kind's second row of the same issue
// are both untouched by it.
func TestTheOutboxTakesTheRoleHoldersPublishKindWith20261116165926(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, roleHoldersPublishVersion-1)
	queue := func(kind, issue string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, '{}', 0, now(), '')`, kind, issue)
		return err
	}
	if err := queue("role_holders_publish", "LEGION-210"); err != nil {
		t.Fatalf("queueing a role_holders_publish row before the migration = %v, want it taken: this release's 20261116082000 names the kind below this version", err)
	}
	var indexes int
	if err := store.Pool().QueryRow(ctx, `select count(*) from pg_indexes where tablename = 'outbox' and indexname = 'outbox_role_holders_publish_issue_unique'`).Scan(&indexes); err != nil {
		t.Fatalf("look for the publish index before the migration: %v", err)
	}
	if indexes != 0 {
		t.Fatalf("before the migration the outbox holds %d publish indexes, want none: it is the index the migration exists for", indexes)
	}
	t.Logf("before the migration the outbox holds no publish index")
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the role-holders publish kind: %v", err)
	}
	kinds := []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish", "reach_notice", "reach_controller_notice",
	}
	if len(kinds) != 27 {
		t.Fatalf("the list names %d kinds, want the 27 the daemon writes", len(kinds))
	}
	for _, kind := range kinds {
		if err := queue(kind, "LEGION-208"); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes all 27 kinds, role_holders_publish among them")
	if err := queue("quiesce", "LEGION-208"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge", "LEGION-208"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}

	err := queue("role_holders_publish", "LEGION-208")
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
