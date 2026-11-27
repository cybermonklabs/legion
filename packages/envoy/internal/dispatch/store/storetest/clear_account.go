package storetest

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestOnlyClearAccount sets people.sub of email to null, standing in for a row that records no
// Google account (a sign-in pool's, header identity's or a dev sign-in's, or one from before the
// column), with the trigger that refuses taking a recorded account off (migration 20261118041916,
// people_binding_columns) turned off for that one transaction. No statement of Dispatch can write
// it while the trigger runs; only a test may, and the name and the testing.TB it takes say so.
func TestOnlyClearAccount(t testing.TB, database *store.Store, email string) {
	t.Helper()
	execWithTriggerOff(t, database, "people", "people_binding_columns", `update people set sub = null where email = $1`, email)
}

// TestOnlyRecordAccountUnkept sets people.sub of email to subject with the trigger that keeps an
// account change for the account binding's catch-up (people_account_binding_replacement,
// migrations 20261121121717 and 20261121180429) turned off for that one transaction, standing in
// for a change the database stamped and nothing kept, which a build without the binding makes by
// signing in while this release's migrations run, after 20261117175133 and before 20261121180429
// (from 20261121121717 on, the trigger kept a change only while the fleet read saw that build; a
// recording made before this release has no recording time at all): the change still ends the
// address's sessions and stamps sub_recorded_at, as every change does (people_binding_columns), but
// no account_binding_replacements row names it. Only a test may write it, and the name and the
// testing.TB it takes say so.
func TestOnlyRecordAccountUnkept(t testing.TB, database *store.Store, email, subject string) {
	t.Helper()
	execWithTriggerOff(t, database, "people", "people_account_binding_replacement", `update people set sub = $2 where email = $1`, email, subject)
}

// TestOnlyWriteBrokerRevocations runs statement with args against broker_revocations with the
// trigger that holds its columns to their writers (migration 20261118152422,
// broker_revocations_binding_columns) turned off for that one transaction: it stands in for a
// revocation recorded long ago, settled at a moment the test names, or naming a holder the address
// no longer records, which no statement of Dispatch can write while the trigger runs.
func TestOnlyWriteBrokerRevocations(t testing.TB, database *store.Store, statement string, args ...any) {
	t.Helper()
	execWithTriggerOff(t, database, "broker_revocations", "broker_revocations_binding_columns", statement, args...)
}

// execWithTriggerOff runs statement with args in one transaction that turns table's trigger off
// first and on again after it, so a test can write what the trigger refuses every other statement.
func execWithTriggerOff(t testing.TB, database *store.Store, table, trigger, statement string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`alter table ` + table + ` disable trigger ` + trigger, nil},
		{statement, args},
		{`alter table ` + table + ` enable trigger ` + trigger, nil},
	} {
		if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("%s: %v", step.sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
