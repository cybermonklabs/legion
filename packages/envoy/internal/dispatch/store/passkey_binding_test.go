package store

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// passkeyBindingIssuer is the outside origin the passkey binding rows record passkey accounts under
// (people.sub_issuer).
const passkeyBindingIssuer = "https://outside.binding.example"

// recordPasskeyAccount records subject under passkeyBindingIssuer as email's account, a restricted
// member's, through the account binding (signInAccountTx), as a first enrollment or a passkey's
// sign-in does.
func recordPasskeyAccount(t *testing.T, people *PgPeopleStore, email, subject string) {
	t.Helper()
	ctx := context.Background()
	tx, err := people.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := people.signInAccountTx(ctx, tx, email, signInAccount{issuer: passkeyBindingIssuer, subject: subject}, nil, model.MemberRestricted); err != nil {
		t.Fatalf("record %s's passkey account %s: %v", email, subject, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// endPasskeyAccount runs the owner's Reset's and the last list's end of email's passkey account
// (PgPasskeys.EndAccountTx) in a transaction of its own.
func endPasskeyAccount(t *testing.T, passkeys *PgPasskeys, email string) {
	t.Helper()
	ctx := context.Background()
	tx, err := passkeys.people.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := passkeys.EndAccountTx(ctx, tx, email); err != nil {
		t.Fatalf("end %s's passkey account: %v", email, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// owedRevocation is one broker revocation owed at an address: the account and its issuer.
type owedRevocation struct{ Subject, Issuer string }

// owedRevocations answers email's owed broker revocations with their issuers, in the order they
// were recorded.
func owedRevocations(t *testing.T, s *Store, email string) []owedRevocation {
	t.Helper()
	rows, err := s.Pool.Query(context.Background(), `select subject, issuer from broker_revocations where email = $1 and done_at is null order by recorded_at`, email)
	if err != nil {
		t.Fatal(err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[owedRevocation])
	if err != nil {
		t.Fatal(err)
	}
	return owed
}

// bindingOff registers a live process of a build without the account binding, so the binding does
// not hold, and fails t when it still does: the state each row below is about.
func bindingOff(t *testing.T, s *Store) {
	t.Helper()
	registerFleetProcess(t, s, OwnerLockProtocol, 0)
	if holds, _ := bindingHolds(t, s); holds {
		t.Fatal("the account binding holds with a live process without it registered; want it off")
	}
}

// bindingOn takes every build without the binding out of the fleet, so the binding holds.
func bindingOn(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), `delete from server_processes`); err != nil {
		t.Fatal(err)
	}
	ageBindingMark(t, s, OwnerLockMarkWindow+time.Minute)
	if holds, _ := bindingHolds(t, s); !holds {
		t.Fatal("the account binding does not hold with no build without it in the fleet")
	}
}

// TestAPasskeyAccountChangeRevokesAndOwesAtOnceWhileTheBindingIsOff (merge of #4674's tree, the
// passkey arm of signInAccountTx's gate): while a build without the account binding is in the
// fleet, a change whose old or new account is a passkey's revokes the address's personal tokens and
// owes the broker the old account at once, under the old account's own issuer (people.sub_issuer),
// through the same statements the binding runs when it holds. The owner's Reset and the end at the
// last list (PgPasskeys.EndAccountTx), the hourly end (PgPasskeys.End), and a Google sign-in that
// replaces a passkey's account (SignInSubject) all do, and the Google sign-in deletes the account's
// passkeys too. The transaction marks itself a writer that knows the binding, so the database's
// trigger keeps no change for it, and the catch-up once the binding holds owes nothing more.
//
// Control: gate the passkey arm as the base gates a Google change (revokesNow = binding alone), and
// every row fails: no token revoked, nothing owed, and the change kept for the catch-up.
func TestAPasskeyAccountChangeRevokesAndOwesAtOnceWhileTheBindingIsOff(t *testing.T) {
	ctx := context.Background()
	for _, row := range []struct {
		name   string
		change func(t *testing.T, people *PgPeopleStore, passkeys *PgPasskeys, email string)
		// passkeysLeft is how many passkeys the change leaves the address.
		passkeysLeft int
	}{
		{"the owner's Reset or the end at the last list (EndAccountTx)", func(t *testing.T, _ *PgPeopleStore, passkeys *PgPasskeys, email string) {
			endPasskeyAccount(t, passkeys, email)
		}, 0},
		{"the hourly end of a person no list names (PgPasskeys.End)", func(t *testing.T, _ *PgPeopleStore, passkeys *PgPasskeys, email string) {
			if ended, err := passkeys.End(ctx, email, "passkey-p1"); err != nil {
				t.Fatalf("the hourly end: %v", err)
			} else if ended {
				t.Fatal("the hourly end answered the sign-in standing; want it ended")
			}
		}, 0},
		{"a Google sign-in replacing the passkey's account (SignInSubject)", func(t *testing.T, people *PgPeopleStore, _ *PgPasskeys, email string) {
			if err := people.SignInSubject(ctx, email, "google-b", "refresh-b", "", model.MemberFull); err != nil {
				t.Fatalf("the Google sign-in: %v", err)
			}
		}, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			database := migratedEmptyStore(t)
			people := brokerPeople(database)
			passkeys := NewPgPasskeys(people, passkeyBindingIssuer)
			const email = "outsider@binding.example"
			recordPasskeyAccount(t, people, email, "passkey-p1")
			if _, err := database.Pool.Exec(ctx, `insert into passkeys (credential_id, email, user_handle, public_key, aaguid, flags, project_key, made_by)
				values ('\x01', $1, 'passkey-p1', '\x01', '', 0, 'PRIV', 'alice@example.com')`, email); err != nil {
				t.Fatal(err)
			}
			tokenP1 := mintToken(t, database.Pool, email, "token-p1", "passkey-p1")
			tokenNone := mintToken(t, database.Pool, email, "token-none", "")

			bindingOff(t, database)
			row.change(t, people, passkeys, email)
			for name, id := range map[string]string{"token-p1": tokenP1, "token-none": tokenNone} {
				if _, revoked := tokenState(t, database.Pool, id); !revoked {
					t.Errorf("%s while the binding is off left %s live; want it revoked at once", row.name, name)
				}
			}
			want := []owedRevocation{{Subject: "passkey-p1", Issuer: passkeyBindingIssuer}}
			if got := owedRevocations(t, database, email); !slices.Equal(got, want) {
				t.Errorf("%s while the binding is off owes %+v; want %+v, the old account under its own issuer", row.name, got, want)
			}
			if kept := keptReplacements(t, database, email); len(kept) != 0 {
				t.Errorf("%s while the binding is off left the changes %q kept for the catch-up; want none, its transaction marked", row.name, kept)
			}
			var left int
			if err := database.Pool.QueryRow(ctx, `select count(*) from passkeys where email = $1`, email).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != row.passkeysLeft {
				t.Errorf("%s while the binding is off left %d passkeys; want %d", row.name, left, row.passkeysLeft)
			}

			bindingOn(t, database)
			held, caught, err := CatchUpAccountBindingOf(ctx, database.Pool, email, testRevocationIssuer)
			if err != nil || !held || !reflect.DeepEqual(caught, AccountBindingCatchUp{}) {
				t.Errorf("the catch-up once the binding holds answered held %t, %+v, %v; want nothing more owed", held, caught, err)
			}
			if got := owedRevocations(t, database, email); !slices.Equal(got, want) {
				t.Errorf("after the catch-up %s owes %+v; want %+v, nothing owed twice", email, got, want)
			}
		})
	}
}

// TestTheCatchUpOwesExactlyTheUnmarkedChangeBesideAMarkedPasskeyChange: marking is per
// transaction. While the binding is off, the owner's Reset ends passkey-p1 (a marked change, owed
// at once), and then a build without the binding signs a Google account in to the same address
// (its update of sub, spelled here: unmarked). The trigger keeps the unmarked change alone, and
// the catch-up once the binding holds owes exactly the account that change replaced, the Reset's
// new passkey account, beside passkey-p1's revocation the Reset owed itself.
//
// Control: mark the unmarked write (select set_config('dispatch.account_binding_writer', 'on',
// true) in its transaction), and the trigger keeps nothing and the row fails at "kept".
func TestTheCatchUpOwesExactlyTheUnmarkedChangeBesideAMarkedPasskeyChange(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	passkeys := NewPgPasskeys(people, passkeyBindingIssuer)
	const email = "interleaved@binding.example"
	recordPasskeyAccount(t, people, email, "passkey-p1")

	bindingOff(t, database)
	endPasskeyAccount(t, passkeys, email)
	var p2 string
	if err := database.Pool.QueryRow(ctx, `select sub from people where email = $1`, email).Scan(&p2); err != nil || p2 == "passkey-p1" {
		t.Fatalf("after the Reset people records %q, %v; want a new passkey account", p2, err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set signed_in_at = now(), sub = 'google-c' where email = $1`, email); err != nil {
		t.Fatalf("google-c's sign-in through a build without the binding: %v", err)
	}
	if kept := keptReplacements(t, database, email); !slices.Equal(kept, []string{p2}) {
		t.Fatalf("the changes kept are %q; want the unmarked one alone, which replaced %s", kept, p2)
	}

	bindingOn(t, database)
	held, caught, err := CatchUpAccountBindingOf(ctx, database.Pool, email, testRevocationIssuer)
	if err != nil || !held || !slices.Equal(caught.Replaced, []string{p2}) || !slices.Equal(caught.Owed, []string{p2}) {
		t.Fatalf("the catch-up answered held %t, %+v, %v; want it to owe %s alone", held, caught, err, p2)
	}
	want := []owedRevocation{{Subject: "passkey-p1", Issuer: passkeyBindingIssuer}, {Subject: p2, Issuer: testRevocationIssuer}}
	if got := owedRevocations(t, database, email); !slices.Equal(got, want) {
		t.Errorf("owed after the catch-up %+v; want %+v: passkey-p1, which the Reset owed itself under its own issuer, and %s, which the catch-up owed under the deployment's (account_binding_replacements keeps no issuer)", got, want, p2)
	}
}

// TestAPasskeyAccountReplacingAGoogleAccountRevokesAndOwesAtOnceWhileTheBindingIsOff: the
// account.issuer arm of the gate alone. The address records a Google account (no sub_issuer), and
// a passkey account replaces it while a build without the binding is in the fleet: the change
// revokes the address's personal tokens and owes the Google account at once under the deployment's
// issuer, keeps no change for the catch-up, and leaves the catch-up nothing to owe.
//
// Control: drop the account.issuer arm (revokesNow := binding || storedIssuer != ""), and the row
// fails: no token revoked, nothing owed, the change kept.
func TestAPasskeyAccountReplacingAGoogleAccountRevokesAndOwesAtOnceWhileTheBindingIsOff(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	const email = "replaced@binding.example"
	if err := people.SignInSubject(ctx, email, "google-a", "refresh-a", "", model.MemberRestricted); err != nil {
		t.Fatalf("google-a's sign-in: %v", err)
	}
	tokenA := mintToken(t, database.Pool, email, "token-a", "google-a")

	bindingOff(t, database)
	recordPasskeyAccount(t, people, email, "passkey-p1")
	if _, revoked := tokenState(t, database.Pool, tokenA); !revoked {
		t.Error("the passkey account replacing google-a while the binding is off left token-a live; want it revoked at once")
	}
	// The deployment's issuer as the people store spells it (WithBrokerRevocations canonicalizes it).
	want := []owedRevocation{{Subject: "google-a", Issuer: canonicalIssuer}}
	if got := owedRevocations(t, database, email); !slices.Equal(got, want) {
		t.Errorf("the replacement while the binding is off owes %+v; want %+v", got, want)
	}
	if kept := keptReplacements(t, database, email); len(kept) != 0 {
		t.Errorf("the replacement left the changes %q kept for the catch-up; want none, its transaction marked", kept)
	}

	bindingOn(t, database)
	held, caught, err := CatchUpAccountBindingOf(ctx, database.Pool, email, testRevocationIssuer)
	if err != nil || !held || !reflect.DeepEqual(caught, AccountBindingCatchUp{}) {
		t.Errorf("the catch-up once the binding holds answered held %t, %+v, %v; want nothing more owed", held, caught, err)
	}
	if got := owedRevocations(t, database, email); !slices.Equal(got, want) {
		t.Errorf("after the catch-up %s owes %+v; want %+v, nothing owed twice", email, got, want)
	}
}
