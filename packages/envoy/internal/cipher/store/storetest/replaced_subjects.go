package storetest

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store"
)

// TestOnlyWriteReplacedSubjects runs statement with args against replaced_person_subjects with the
// trigger that holds its rows to the revoke's writes (migration 0047,
// replaced_person_subjects_guarded) turned off for that one transaction: it stands in for a
// replacement recorded long ago, which no statement of the broker can write while the trigger
// runs, since replaced_at never moves. Only a test may, and the name and the testing.TB it takes
// say so.
func TestOnlyWriteReplacedSubjects(t testing.TB, st *store.Store, statement string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`alter table replaced_person_subjects disable trigger replaced_person_subjects_guarded`, nil},
		{statement, args},
		{`alter table replaced_person_subjects enable trigger replaced_person_subjects_guarded`, nil},
	} {
		if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("%s: %v", step.sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
