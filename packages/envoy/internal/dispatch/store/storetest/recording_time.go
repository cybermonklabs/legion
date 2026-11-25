package storetest

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SetRecordingTime sets people.sub_recorded_at of email to at, nil for null, with the trigger that
// stamps it (migration 20261118041916, people_binding_columns) turned off for that one
// transaction: it stands in for a row from before the column, or a recording made at a moment the
// test names, which no statement can write while the trigger runs, since only a change of sub
// moves the time and the database stamps it then.
func SetRecordingTime(t testing.TB, database *store.Store, email string, at any) {
	t.Helper()
	execWithTriggerOff(t, database, "people", "people_binding_columns", `update people set sub_recorded_at = $2 where email = $1`, email, at)
}
