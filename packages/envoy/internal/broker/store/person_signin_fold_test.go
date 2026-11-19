package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// A sign-in's login is stored as its caller folded it, and the writer folds nothing more: every
// caller passes a login the one ASCII fold made (oidc.WorkspaceLogin for the trade,
// record.CanonicalLogin for the list and the revoke). Postgres's lower() folds Unicode, so a
// writer that lowered U+212A KELVIN SIGN would record "\u212aate@example.com" as kate@example.com,
// a person that sign-in does not name. The table's own check (person = lower(person)) refuses the
// Kelvin login instead, and kate holds no sign-in.
func TestAKelvinLoginIsRefusedNotFoldedOntoAnother(t *testing.T) {
	st := storetest.Open(t)
	err := record(t, st, "\u212aate@example.com", signInBoxOne, signInMachine)
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "23514" || refused.ConstraintName != "person_signins_person_check" {
		t.Errorf("recording a login holding U+212A answered %v, want the table's person check (23514 person_signins_person_check) to refuse it", err)
	}
	live, err := st.PersonSignIns(context.Background(), "kate@example.com")
	if err != nil || len(live) != 0 {
		t.Fatalf("kate@example.com's sign-ins = %+v, %v; want none, since the Kelvin login names someone else", live, err)
	}
}
