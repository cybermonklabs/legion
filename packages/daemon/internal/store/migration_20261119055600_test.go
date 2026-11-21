package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// waitDueVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const waitDueVersion = 20261119055600

// A wait_due row brings a phase worker's wait to its time (record.WaitDue), queued by the
// declaration's own transaction. The outbox refuses a kind its constraint does not name, so the
// row could not be queued before this migration, and every declaration would fail with it: the
// negative control is that refusal, read before the migration, on a database that already carries
// the conflict's kind list (20261119003157), which this version stands above. After it the row is
// queued, and so is every kind the constraint already named, since the migration rewrites the list
// whole and a kind left out would stop its own effect. A kind that is none is still refused, with
// the check's own SQLSTATE, and so is a merge-lane kind, which went with the merge the daemon no
// longer submits. The rows the release before it left pending — the merge forward, the conflict's
// second read, the plan record, the step record, the role-holders publication and the merge-queue
// kinds its lists added among them, each on an issue and with a payload of its own — come through
// the migration whole: the same id, kind, issue, payload, attempts, due time and error.
func TestTheOutboxTakesTheWaitDueKindWith20261119055600(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, waitDueVersion-1)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	refusedByTheCheck := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "outbox_kind_check"
	}
	if err := queue("wait_due"); !refusedByTheCheck(err) {
		t.Fatalf("queueing a wait_due row before the migration = %v, want the kind constraint to refuse it (23514): it is the row the migration exists for", err)
	}
	// The pending rows, whole: each field the release before wrote, as text.
	const pendingRows = `select row(id, kind, issue, payload, attempts, next_at, last_error)::text from outbox order by id`
	for i, kind := range []string{"merge_forward", "conflict_confirm", "plan_record", "step_record", "role_holders_publish", "merge_queue_removal", "merge_queue_read", "update_ask", "supervise", "dispatch_status"} {
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, jsonb_build_object('pending', $1::text), $3, now() + make_interval(mins => $3), $4)`,
			kind, fmt.Sprintf("LEGION-%d", 300+i), i, fmt.Sprintf("attempt %d failed", i)); err != nil {
			t.Fatalf("queue a pending %s row under the release before: %v", kind, err)
		}
	}
	before := rowsAsText(t, store, pendingRows)
	if len(before) != 10 {
		t.Fatalf("the outbox holds %d rows before the migration, want the ten pending rows of the release before", len(before))
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the wait_due kind: %v", err)
	}
	if after := rowsAsText(t, store, pendingRows); !reflect.DeepEqual(after, before) {
		t.Fatalf("the pending rows after the migration = %+v, want the release before's rows whole: %+v", after, before)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish", "step_record", "plan_record",
		"conflict_confirm", "merge_forward", "wait_due",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes every kind, wait_due among them")
	if err := queue("wait_ends"); !refusedByTheCheck(err) {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it (23514)", err)
	}
	if err := queue("ready_merge"); !refusedByTheCheck(err) {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}
}
