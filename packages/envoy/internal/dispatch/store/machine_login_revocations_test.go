package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// recordedMachineLoginRevocation records, in a transaction of its own, that Dispatch owes the
// broker the revocation of email's machine logins, as a removal does in its own. It is
// storetest.RecordMachineLoginRevocation for this package's own tests, which cannot import
// storetest (it imports this package).
func recordedMachineLoginRevocation(t *testing.T, database *Store, email string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := RecordMachineLoginRevocation(ctx, tx, email); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestARemovalRecordedAgainIsNotSettledByAnEarlierClaim: a machine-login revocation recorded in a
// removal's transaction is owed from its commit, claimed once within the retry window, and settled
// only at the generation the claim read. A removal recorded again after the claim owes it anew, so
// the earlier claim's settlement writes nothing and the next claim pays it; and the row a claim took
// is not claimed again until the window has passed. Every Settle here asks no time past the
// removal (after 0), so the generation alone decides.
//
// Control: Settle without its generation, and the earlier claim settles the later removal and the
// row fails naming it settled.
func TestARemovalRecordedAgainIsNotSettledByAnEarlierClaim(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	revocations := NewMachineLoginRevocations(database.Pool)
	const email = "erin@other.example"
	record := func() { recordedMachineLoginRevocation(t, database, " Erin@Other.example ") }

	if owed, _, err := revocations.Owing(ctx, email); err != nil || owed {
		t.Fatalf("Owing(%s) before any removal = %t, %v; want nothing owed", email, owed, err)
	}
	record()
	claimed, err := revocations.Claim(ctx, time.Minute, 5)
	if err != nil || len(claimed) != 1 || claimed[0].Email != email {
		t.Fatalf("Claim after the removal = %+v, %v; want %s's one revocation", claimed, err, email)
	}
	if again, err := revocations.Claim(ctx, time.Minute, 5); err != nil || len(again) != 0 {
		t.Fatalf("Claim inside the retry window = %+v, %v; want nothing", again, err)
	}
	if _, due, err := revocations.NextDue(ctx, time.Minute); err != nil || !due {
		t.Fatalf("NextDue with a claimed revocation owed = due %t, %v; want due", due, err)
	}

	record()
	if settled, err := revocations.Settle(ctx, claimed[0], 0); err != nil || settled {
		t.Fatalf("Settle of the earlier claim after the removal was recorded again = %t, %v; want nothing settled", settled, err)
	}
	if owed, generation, err := revocations.Owing(ctx, email); err != nil || !owed || generation != claimed[0].Generation+1 {
		t.Fatalf("Owing(%s) = %t at generation %d, %v; want owed at %d", email, owed, generation, err, claimed[0].Generation+1)
	}

	if _, err := database.Pool.Exec(ctx, `update machine_login_revocations set tried_at = now() - interval '2 minutes'`); err != nil {
		t.Fatal(err)
	}
	next, err := revocations.Claim(ctx, time.Minute, 5)
	if err != nil || len(next) != 1 || next[0].Generation != claimed[0].Generation+1 {
		t.Fatalf("Claim after the window = %+v, %v; want the later generation", next, err)
	}
	if settled, err := revocations.Settle(ctx, next[0], 0); err != nil || !settled {
		t.Fatalf("Settle of the later claim = %t, %v; want it settled", settled, err)
	}
	if owed, _, err := revocations.Owing(ctx, email); err != nil || owed {
		t.Fatalf("Owing(%s) after the later claim settled = %t, %v; want nothing owed", email, owed, err)
	}
	if _, due, err := revocations.NextDue(ctx, time.Minute); err != nil || due {
		t.Fatalf("NextDue with nothing owed = due %t, %v; want none", due, err)
	}
}

// TestARecordOfAMachineLoginRevocationTakesThePersonsApprovalLock: a record of a machine-login
// revocation is ordered after an approval of the person's in flight only under the person's
// approval lock, so it takes that lock itself, whatever its caller took first, and never waits for
// it. A record in a transaction that took no lock holds it until that transaction ends, so an
// approval of the person's meanwhile is ErrMachineLoginApprovalInFlight; a record in the
// transaction that holds it already records at once; and a record while an approval in flight
// holds it is ErrMachineLoginApprovalInFlight at once and records nothing.
//
// Control: RecordMachineLoginRevocation taking no lock, and the approval made while the first
// record's transaction is open takes the lock, named.
func TestARecordOfAMachineLoginRevocationTakesThePersonsApprovalLock(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	const email = "erin@other.example"
	begin := func() pgx.Tx {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		return tx
	}

	removal := begin()
	if err := RecordMachineLoginRevocation(ctx, removal, " Erin@Other.example "); err != nil {
		t.Fatalf("a record in a transaction that took no approval lock = %v, want it recorded", err)
	}
	approval := begin()
	if owed, err := HoldMachineLoginApprovals(ctx, approval, email); !errors.Is(err, ErrMachineLoginApprovalInFlight) {
		t.Fatalf("an approval of erin's while the record's transaction is open = owed %t, %v; want ErrMachineLoginApprovalInFlight, the record holding her approval lock", owed, err)
	}
	if err := RecordMachineLoginRevocation(ctx, removal, email); err != nil {
		t.Fatalf("a second record in the transaction that holds erin's approval lock = %v, want it recorded", err)
	}
	for _, tx := range []pgx.Tx{approval, removal} {
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}

	inFlight := begin()
	if owed, err := HoldMachineLoginApprovals(ctx, inFlight, email); err != nil || owed {
		t.Fatalf("an approval of erin's with nothing owed = owed %t, %v; want it to hold her approval lock", owed, err)
	}
	// The bound only ends a record that waits; one that does not answers at once.
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := RecordMachineLoginRevocation(bounded, begin(), email); !errors.Is(err, ErrMachineLoginApprovalInFlight) {
		t.Fatalf("a record while an approval of erin's holds her approval lock = %v, want ErrMachineLoginApprovalInFlight at once", err)
	}
	if err := inFlight.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if owed, _, err := NewMachineLoginRevocations(database.Pool).Owing(ctx, email); err != nil || owed {
		t.Fatalf("Owing(%s) after every record was rolled back or refused = %t, %v; want nothing owed", email, owed, err)
	}
}
