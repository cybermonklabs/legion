package storetest

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// RecordMachineLoginRevocation records, in a transaction of its own, that Dispatch owes the secrets
// broker the revocation of email's machine logins (store.RecordMachineLoginRevocation), as a
// removal that takes a restricted member off their last access list does in its own.
func RecordMachineLoginRevocation(t testing.TB, database *store.Store, email string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the record of %s's machine-login revocation: %v", email, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.RecordMachineLoginRevocation(ctx, tx, email); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the record of %s's machine-login revocation: %v", email, err)
	}
}
