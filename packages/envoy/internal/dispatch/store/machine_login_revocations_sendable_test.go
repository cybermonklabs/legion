package store

import (
	"context"
	"testing"
	"time"
)

// TestAClaimedMachineLoginRevocationIsSendableWithinItsWindow: a claim answers when the removal that
// owes it was recorded, the time the pass judges each login by, and the claimed row may send broker
// calls only while its claim is younger than the window the pass gives and its generation is still
// the one owed. A row whose claim is older than the window, and a row a removal recorded again since
// the claim, may send none.
//
// Control: Sendable without its claim's age, and the row with the old claim reads sendable, named.
//
// The window is stagedAhead, staged state and not a wait: the claim must still be younger than it
// when Sendable reads it however long a loaded machine takes between the two, and the row ages the
// claim past it on the database's clock rather than waiting.
func TestAClaimedMachineLoginRevocationIsSendableWithinItsWindow(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	revocations := NewMachineLoginRevocations(database.Pool)
	record := func(email string) { recordedMachineLoginRevocation(t, database, email) }
	const email = "erin@other.example"
	var before time.Time
	if err := database.Pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	record(email)
	claimed, err := revocations.Claim(ctx, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %+v, %v; want erin's one revocation", claimed, err)
	}
	var recordedAt time.Time
	if err := database.Pool.QueryRow(ctx, `select recorded_at from machine_login_revocations where email = $1`, email).Scan(&recordedAt); err != nil {
		t.Fatal(err)
	}
	if !claimed[0].RecordedAt.Equal(recordedAt) || claimed[0].RecordedAt.Before(before) {
		t.Fatalf("the claim's RecordedAt = %s, want the row's recorded_at %s, after %s", claimed[0].RecordedAt, recordedAt, before)
	}
	if sendable, err := revocations.Sendable(ctx, claimed[0], stagedAhead); err != nil || !sendable {
		t.Fatalf("Sendable just after the claim = %t, %v; want sendable", sendable, err)
	}

	if _, err := database.Pool.Exec(ctx, `update machine_login_revocations set tried_at = tried_at - make_interval(secs => $2) where email = $1`,
		email, (stagedAhead + time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	if sendable, err := revocations.Sendable(ctx, claimed[0], stagedAhead); err != nil || sendable {
		t.Errorf("Sendable with the claim a second older than its %s window = %t, %v; want not sendable", stagedAhead, sendable, err)
	}

	if _, err := database.Pool.Exec(ctx, `update machine_login_revocations set tried_at = clock_timestamp() where email = $1`, email); err != nil {
		t.Fatal(err)
	}
	record(email)
	if sendable, err := revocations.Sendable(ctx, claimed[0], stagedAhead); err != nil || sendable {
		t.Errorf("Sendable under the claim's generation after a removal recorded it again = %t, %v; want not sendable", sendable, err)
	}
}
