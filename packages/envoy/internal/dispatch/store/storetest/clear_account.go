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
