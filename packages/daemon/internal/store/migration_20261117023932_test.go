package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// planRecordKindVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it. Its header also carries the way back of
// the release that rates each plan, which plan_ratings_way_back_test.go runs.
const planRecordKindVersion = 20261117023932

// A plan record is the row that writes the plan one run of an issue carries out onto Dispatch
// (record.PlanRecord). The outbox refuses a kind its constraint does not name, so the row could not
// be queued before this migration, and the transaction that starts the issue's work would fail with
// it: the negative control is that refusal, read before the migration, on a database that already
// carries the step record's list (20261117000528), which this version stands above. After it the
// row is queued, and so is every kind the constraint already named, since the migration rewrites
// the list whole and a kind left out would stop its own effect: the role-holders publication's
// kind among them, whose row the release before left pending comes through the migration, which
// would fail on it with the check's SQLSTATE if the list left that kind out. A kind that is none
// is still refused, with the check's own SQLSTATE.
func TestTheOutboxCheckTakesThePlanRecord(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, planRecordKindVersion-1)
	// queueOn queues a row of kind on issue, and queue on LEGION-208. The pending role-holders row
	// stands on an issue of its own: the outbox holds one such row of an issue
	// (outbox_role_holders_publish_issue_unique), and the row after the migration is LEGION-208's.
	queueOn := func(kind, issue string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, '{}', 0, now(), '')`, kind, issue)
		return err
	}
	queue := func(kind string) error { return queueOn(kind, "LEGION-208") }
	refusedByTheCheck := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "outbox_kind_check"
	}
	if err := queue("step_record"); err != nil {
		t.Fatalf("queue a step_record row before the migration: %v; the database this test starts from carries 20261117000528's list", err)
	}
	if err := queueOn("role_holders_publish", "LEGION-209"); err != nil {
		t.Fatalf("queue a role_holders_publish row before the migration: %v; the database this test starts from carries 20261117000528's list", err)
	}
	if err := queue("plan_record"); !refusedByTheCheck(err) {
		t.Fatalf("queueing a plan_record row before the migration = %v, want the kind constraint to refuse it (23514): it is the row the migration exists for", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the plan record's kind: %v", err)
	}
	for _, kind := range append(slices.Clone(stepRecordReleaseKinds), "plan_record") {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	if err := queue("nope"); !refusedByTheCheck(err) {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it (23514)", err)
	}
}
