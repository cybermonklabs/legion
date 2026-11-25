package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestSignInSubjectRecordsWhenItChangesTheAccount: a sign-in that changes the address's Google
// account moves people.sub_recorded_at, which the database stamps from its own clock (the trigger
// of migration 20261117175133): the address's first recording (no row, and a row recorded with no
// account) and a replacement, each later than the one before. A sign-in under the account the row
// already records leaves it as it was, and a row no sign-in has recorded an account for has none,
// so nothing predates a recording that never happened.
//
// Control: the trigger dropped (the first recording reads null).
func TestSignInSubjectRecordsWhenItChangesTheAccount(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	recordedAt := func(email string) *time.Time {
		t.Helper()
		var at *time.Time
		if err := database.Pool.QueryRow(ctx, `select sub_recorded_at from people where email = $1`, email).Scan(&at); err != nil {
			t.Fatalf("read %s's recording time: %v", email, err)
		}
		return at
	}
	signIn := func(email, subject string) {
		t.Helper()
		if err := people.SignInSubject(ctx, email, subject, "refresh-"+subject, "", model.MemberFull); err != nil {
			t.Fatalf("the sign-in of %s under %s: %v", email, subject, err)
		}
	}

	if err := people.Record(ctx, "recorded@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := recordedAt("recorded@example.com"); got != nil {
		t.Errorf("a row recorded with no account has recording time %s, want none", got)
	}
	signIn("recorded@example.com", "account-a")
	if recordedAt("recorded@example.com") == nil {
		t.Error("the first Google sign-in of a row recorded with no account set no recording time")
	}

	signIn("first@example.com", "account-a")
	first := recordedAt("first@example.com")
	if first == nil {
		t.Fatal("the address's first sign-in, with no row before it, set no recording time")
	}
	signIn("first@example.com", "account-a")
	if again := recordedAt("first@example.com"); again == nil || !again.Equal(*first) {
		t.Errorf("a sign-in under the account the row records moved its recording time from %s to %v, want it left", first, again)
	}
	signIn("first@example.com", "account-b")
	if replaced := recordedAt("first@example.com"); replaced == nil || !replaced.After(*first) {
		t.Errorf("the replacement by account-b left the recording time %v, want it later than %s", replaced, first)
	}
}
