package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// testRevocationIssuer is the sign-in issuer the tests' people stores record a replaced account
// under, as Google's ID tokens spell it without a scheme: WithBrokerRevocations records it as
// oidc.CanonicalIssuer spells it.
const (
	testRevocationIssuer = "accounts.google.com"
	canonicalIssuer      = "https://accounts.google.com"
)

// brokerRevocationRow is one broker_revocations row as the table holds it.
type brokerRevocationRow struct {
	Issuer, Subject, HolderSubject string
	Generation                     int64
	Done                           bool
}

// brokerRevocationsOf is every broker_revocations row of email, in the order they are paid.
func brokerRevocationsOf(t *testing.T, database *Store, email string) []brokerRevocationRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select issuer, subject, holder_subject, generation, done_at is not null from broker_revocations
		where email = $1 order by recorded_at, issuer, subject
	`, email)
	if err != nil {
		t.Fatalf("read the broker revocations of %s: %v", email, err)
	}
	defer rows.Close()
	var read []brokerRevocationRow
	for rows.Next() {
		var row brokerRevocationRow
		if err := rows.Scan(&row.Issuer, &row.Subject, &row.HolderSubject, &row.Generation, &row.Done); err != nil {
			t.Fatal(err)
		}
		read = append(read, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return read
}

// owedRow is the owed row of the account subject of email that holder took, at generation.
func owedRow(subject, holder string, generation int64) brokerRevocationRow {
	return brokerRevocationRow{Issuer: canonicalIssuer, Subject: subject, HolderSubject: holder, Generation: generation}
}

// sessionGenerationOf is email's session generation as its session store reads it
// (sessionGeneration), failing the test when user_sessions holds no row for email.
func sessionGenerationOf(t *testing.T, pool *Pool, email string) int64 {
	t.Helper()
	generation, found := sessionGeneration(t, NewPgSessionStore(pool), email)
	if !found {
		t.Fatalf("user_sessions holds no generation of %s", email)
	}
	return generation
}

// owedText prints owed revocations with their session generations by value.
func owedText(owed ...OwedBrokerRevocation) string {
	var text []string
	for _, revocation := range owed {
		generation := "none"
		if revocation.SessionGeneration != nil {
			generation = fmt.Sprint(*revocation.SessionGeneration)
		}
		text = append(text, fmt.Sprintf("{%s %s %s held by %s, generation %d, session generation %s}",
			revocation.Email, revocation.Issuer, revocation.Subject, revocation.HolderSubject, revocation.Generation, generation))
	}
	return "[" + strings.Join(text, " ") + "]"
}

// sessionGenerationsOf is the session generation each broker_revocations row of email records, by
// the replaced account's `sub`, -1 for none.
func sessionGenerationsOf(t *testing.T, database *Store, email string) map[string]int64 {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select subject, coalesce(session_generation, -1) from broker_revocations where email = $1`, email)
	if err != nil {
		t.Fatalf("read the session generations of %s's revocations: %v", email, err)
	}
	defer rows.Close()
	recorded := map[string]int64{}
	for rows.Next() {
		var subject string
		var generation int64
		if err := rows.Scan(&subject, &generation); err != nil {
			t.Fatal(err)
		}
		recorded[subject] = generation
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// A revocation records the session generation of its address that its replacing sign-in left
// (recordBrokerRevocationSQL, read in the recording's own statement, after the sign-in's people
// write moved it): at the insert, and at the upsert of an account replaced again, which names the
// generation that later sign-in left, however the generation moved in between (here a session
// revoke). Each is the generation a check of an approve made after that sign-in reads, and below
// the one any check before it read. Owing answers it, as Claim and Clear do.
func TestARecordingNamesTheSessionGenerationItsReplacementLeft(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	const email = "generation@example.com"
	signInAll(t, people, email, "member-1")
	before := sessionGenerationOf(t, database.Pool, email)
	signInAll(t, people, email, "member-2")
	first := sessionGenerationOf(t, database.Pool, email)
	if got := sessionGenerationsOf(t, database, email); first <= before || len(got) != 1 || got["member-1"] != first {
		t.Fatalf("after member-2 took %s at session generation %d (from %d), its revocations record %v; want member-1's at %d", email, first, before, got, first)
	}
	if owing, err := revocations.Owing(ctx, email); err != nil || len(owing) != 1 || owing[0].SessionGeneration == nil || *owing[0].SessionGeneration != first {
		t.Fatalf("Owing(%s) = %s, %v; want member-1's at session generation %d", email, owedText(owing...), err, first)
	}

	if _, err := database.Pool.Exec(ctx, `update user_sessions set generation = generation + 1 where login = $1`, email); err != nil {
		t.Fatalf("a session revoke of %s: %v", email, err)
	}
	signInAll(t, people, email, "member-1")
	back := sessionGenerationOf(t, database.Pool, email)
	signInAll(t, people, email, "member-3")
	second := sessionGenerationOf(t, database.Pool, email)
	got := sessionGenerationsOf(t, database, email)
	want := map[string]int64{"member-2": back, "member-1": second}
	if back <= first+1 || second <= back || len(got) != len(want) || got["member-2"] != back || got["member-1"] != second {
		t.Fatalf("after member-1 took %s back at %d and member-3 took it at %d, its revocations record %v; want %v", email, back, second, got, want)
	}
	if rows := brokerRevocationsOf(t, database, email); !slices.Contains(rows, owedRow("member-1", "member-3", 2)) {
		t.Fatalf("member-1's revocation was not recorded again at generation 2: %+v", rows)
	}
}

// brokerPeople is a people store that records broker revocations under testRevocationIssuer.
func brokerPeople(database *Store) *PgPeopleStore {
	return NewPgPeopleStore(database.Pool, testSigningKey, nil, WithBrokerRevocations(testRevocationIssuer))
}

// signInAll signs each subject in to email in turn, as full members.
func signInAll(t *testing.T, people *PgPeopleStore, email string, subjects ...string) {
	t.Helper()
	for _, subject := range subjects {
		if err := people.SignInSubject(context.Background(), email, subject, "refresh-"+subject, model.MemberFull); err != nil {
			t.Fatalf("the sign-in of %s under %s: %v", email, subject, err)
		}
	}
}

// A sign-in under another Google account than the account the address's row records owes the
// secrets broker a revocation of that account, recorded in the sign-in's transaction beside the
// revoke of its sessions and personal tokens: the replaced account's issuer, as
// oidc.CanonicalIssuer spells it, and `sub`, and the `sub` of the account that took the address,
// whose credentials the broker leaves. The replaced account's member kind does not matter: a
// restricted row can name an account that made broker credentials while it was full, under the
// same `sub`, before the hourly confirmation restricted it, so a member who reclaims an outside
// account's address owes its revocation too, and so does an outside account that replaces
// another. A sign-in that replaces no account owes none: the account's own sign-in again, an
// address's first sign-in, and the first Google sign-in of a row that recorded no Google account.
func TestSignInSubjectRecordsABrokerRevocationOfTheAccountItReplaces(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	for _, test := range []struct {
		name, email string
		// held is the account the row records before the sign-in, as heldKind: "" for a row
		// recorded with no Google account (Record), "-" for no row at all.
		held     string
		heldKind model.MemberKind
		subject  string
		kind     model.MemberKind
		owes     bool
	}{
		{"another Workspace account replaces a full member's", "reassigned@example.com", "member-1", model.MemberFull, "member-2", model.MemberFull, true},
		{"a member reclaims an outside account's address", "alice@second.example", "outsider", model.MemberRestricted, "member", model.MemberFull, true},
		{"another outside account replaces an outside account", "taken@other.example", "outsider-1", model.MemberRestricted, "outsider-2", model.MemberRestricted, true},
		{"the account the row records signs in again", "again@example.com", "member", model.MemberFull, "member", model.MemberFull, false},
		{"the address's first sign-in", "first@example.com", "-", "", "member", model.MemberFull, false},
		{"a row that recorded no Google account", "recorded@example.com", "", "", "member", model.MemberFull, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			switch test.held {
			case "-":
			case "":
				if err := people.Record(ctx, test.email); err != nil {
					t.Fatal(err)
				}
			default:
				if err := people.SignInSubject(ctx, test.email, test.held, "refresh-held", test.heldKind); err != nil {
					t.Fatalf("seed %s: %v", test.email, err)
				}
			}
			if err := people.SignInSubject(ctx, test.email, test.subject, "refresh-new", test.kind); err != nil {
				t.Fatalf("the sign-in of %s under %s: %v", test.email, test.subject, err)
			}
			got := brokerRevocationsOf(t, database, test.email)
			switch want := []brokerRevocationRow{owedRow(test.held, test.subject, 1)}; {
			case test.owes && !slices.Equal(got, want):
				t.Errorf("%s: the broker revocations of %s read %+v, want %+v", test.name, test.email, got, want)
			case !test.owes && len(got) != 0:
				t.Errorf("%s: a sign-in that replaced no account recorded a broker revocation of %s: %+v", test.name, test.email, got)
			}
		})
	}
}

// An address Dispatch records no Google account for (no row at all, or a row recorded with no
// account: header identity, a dev sign-in, a Cognito pool's) can still hold personal tokens of an
// account: one that held the address at the broker and never signed in to Dispatch, whose broker
// token minted them and recorded its account on each. The address's first Google sign-in, under
// another account, revokes those tokens in its transaction and owes the broker a revocation of
// that account, once however many tokens it minted. An account whose only token its owner revoked
// before is owed a revocation too, since its broker credentials may still be live, and that token
// keeps the revoked_at it had.
//
// A token of the address recording no account is revoked by that first recording as well: it was
// minted by a credential bound to no account, before the recording, so it is as much another
// account's as one naming it, and until it was revoked here the only thing refusing it was
// api.tokenAccountReplaced's comparison of its created_at with people.sub_recorded_at, two
// readings of one clock with no margin, which a backward step of the database's clock defeats. It
// names no account to revoke at the broker, so nothing is owed for it. A token recording the
// account that signs in, and another address's token, stay live, and a sign-in that revoked no
// token owes nothing (the row above).
//
// Controls: SignInSubject revoking no token on a first recording (its revokeOtherAccountsTokens
// call removed): account-a's tokens stay live and nothing is owed; the owed accounts read from the
// live tokens alone, as round 12 did: account-c is not owed.
func TestSignInSubjectFirstRecordingRevokesTheTokensOfAnotherAccount(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	tokenOf := func(owner, name, subject string) string {
		t.Helper()
		hash := sha256.Sum256([]byte(owner + "\x00" + name))
		var id string
		if err := database.Pool.QueryRow(ctx, `
			insert into agent_tokens (owner, name, token_hash, prefix, subject) values ($1, $2, $3, 'test', nullif($4, ''))
			returning id::text`, owner, name, hash[:], subject).Scan(&id); err != nil {
			t.Fatalf("store the token %q of %s: %v", name, owner, err)
		}
		return id
	}
	for _, test := range []struct {
		name, email string
		row         bool
	}{
		{"no people row", "first@example.com", false},
		{"a row recorded with no Google account", "recorded@example.com", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.row {
				if err := people.Record(ctx, test.email); err != nil {
					t.Fatal(err)
				}
			}
			revoked := []string{tokenOf(test.email, "account-a's agent", "account-a"), tokenOf(test.email, "account-a's other agent", "account-a")}
			earlier := tokenOf(test.email, "account-c's agent its owner revoked", "account-c")
			var revokedBefore time.Time
			if err := database.Pool.QueryRow(ctx, `update agent_tokens set revoked_at = now() - interval '1 day' where id = $1 returning revoked_at`, earlier).Scan(&revokedBefore); err != nil {
				t.Fatal(err)
			}
			noAccount := tokenOf(test.email, "an agent recording no account", "")
			live := []string{
				tokenOf(test.email, "account-b's agent", "account-b"),
				tokenOf("other-"+test.email, "account-a's agent at another address", "account-a"),
			}
			if err := people.SignInSubject(ctx, test.email, "account-b", "refresh-b", model.MemberFull); err != nil {
				t.Fatalf("the sign-in of %s under account-b: %v", test.email, err)
			}
			for _, id := range revoked {
				if tokenRevokedAt(t, database, id) == nil {
					t.Errorf("%s: account-a's token %s is live after account-b's first sign-in recorded itself for the address, want it revoked", test.email, id)
				}
			}
			if got := tokenRevokedAt(t, database, earlier); got == nil || !got.Equal(revokedBefore) {
				t.Errorf("%s: account-c's token revoked at %s reads revoked at %v, want it kept", test.email, revokedBefore, got)
			}
			if tokenRevokedAt(t, database, noAccount) == nil {
				t.Errorf("%s: the token recording no account is live after account-b's first sign-in recorded itself for the address, want it revoked: nothing but a clock comparison refused it", test.email)
			}
			for _, id := range live {
				if got := tokenRevokedAt(t, database, id); got != nil {
					t.Errorf("%s: the token %s was revoked at %s, want it live", test.email, id, got)
				}
			}
			got := brokerRevocationsOf(t, database, test.email)
			want := []brokerRevocationRow{owedRow("account-a", "account-b", 1), owedRow("account-c", "account-b", 1)}
			if !slices.Equal(got, want) {
				t.Errorf("%s: the broker revocations read %+v, want %+v: account-a and account-c owed once each", test.email, got, want)
			}
		})
	}
}

// Two replacements of one address before the first is paid are two revocations, each of the
// account it replaced, both owed and paid in the order their sign-ins committed: member-1, which
// member-2 took, then member-2, which member-3 took. One account replaced again is its own
// revocation owed again, under the next generation, naming the account that took it this time, and
// paid after every revocation recorded before it. Member-4 takes it, not member-2: an account that
// takes the address back is owed nothing, as the next test requires.
func TestSignInSubjectRecordsOneRevocationPerReplacedAccount(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	const email = "reassigned@example.com"
	signInAll(t, people, email, "member-1", "member-2", "member-3")
	if got, want := brokerRevocationsOf(t, database, email), []brokerRevocationRow{owedRow("member-1", "member-2", 1), owedRow("member-2", "member-3", 1)}; !slices.Equal(got, want) {
		t.Fatalf("after two replacements, the broker revocations of %s read %+v, want %+v", email, got, want)
	}
	owed, err := revocations.Claim(ctx, time.Minute, 10)
	if err != nil || len(owed) != 1 || owed[0].Subject != "member-1" {
		t.Fatalf("the first claim: %+v, %v; want the revocation of member-1 alone, recorded first", owed, err)
	}
	if settled, err := revocations.Settle(ctx, owed[0]); err != nil || !settled {
		t.Fatalf("settle member-1: %t, %v", settled, err)
	}

	signInAll(t, people, email, "member-1")
	want := []brokerRevocationRow{
		{Issuer: canonicalIssuer, Subject: "member-2", HolderSubject: "member-3", Generation: 1},
		{Issuer: canonicalIssuer, Subject: "member-3", HolderSubject: "member-1", Generation: 1},
	}
	if got := brokerRevocationsOf(t, database, email); !slices.Equal(got, append([]brokerRevocationRow{{Issuer: canonicalIssuer, Subject: "member-1", HolderSubject: "member-2", Generation: 1, Done: true}}, want...)) {
		t.Fatalf("after member-1 took the address back, the broker revocations of %s read %+v", email, got)
	}
	signInAll(t, people, email, "member-4")
	want = append(want, brokerRevocationRow{Issuer: canonicalIssuer, Subject: "member-1", HolderSubject: "member-4", Generation: 2})
	if got := brokerRevocationsOf(t, database, email); !slices.Equal(got, want) {
		t.Fatalf("after member-1 was replaced again, the broker revocations of %s read %+v, want %+v, member-1's owed again last", email, got, want)
	}
}

// An account that takes the address back while a revocation of it is still owed owes the broker
// nothing: paid then, the revocation would end what the account made once it held the address
// again, and have the broker refuse it until the revocation of the account it took the address
// from was paid, the retry window later. So the sign-in that takes the address back settles, in its
// own transaction, every revocation of the address still owed for its own account, whether never
// claimed, claimed for a call the broker did not confirm, or claimed for a call still in flight,
// which cannot be recalled but whose late answer then settles nothing. The revocation the sign-in
// records stays owed and is the one a claim takes, though not inside the retry window after a claim
// of the address, so it is paid after a call still in flight is over. The account's revocation at
// another address is never touched.
func TestSignInSubjectThatTakesTheAddressBackSettlesTheRevocationOfItsAccount(t *testing.T) {
	ctx := context.Background()
	const email, other = "reassigned@example.com", "other@example.com"
	reclaimed := []brokerRevocationRow{
		{Issuer: canonicalIssuer, Subject: "member-1", HolderSubject: "member-2", Generation: 1, Done: true},
		owedRow("member-2", "member-1", 1),
	}
	claimOf := func(t *testing.T, revocations *BrokerRevocations, address string) []OwedBrokerRevocation {
		t.Helper()
		owed, err := revocations.Claim(ctx, time.Minute, 10)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return slices.DeleteFunc(owed, func(o OwedBrokerRevocation) bool { return o.Email != address })
	}
	requirePaysTheHolderNext := func(t *testing.T, revocations *BrokerRevocations, holder string) {
		t.Helper()
		owed := claimOf(t, revocations, email)
		want := OwedBrokerRevocation{Email: email, Issuer: canonicalIssuer, Subject: holder, HolderSubject: "member-1", Generation: 1,
			SessionGeneration: new(sessionGenerationOf(t, revocations.pool, email))}
		if len(owed) != 1 || !reflect.DeepEqual(owed[0], want) {
			t.Fatalf("the claim after member-1 took %s back: %s, want %s alone", email, owedText(owed...), owedText(want))
		}
	}

	t.Run("never claimed", func(t *testing.T) {
		database := migratedEmptyStore(t)
		people, revocations := brokerPeople(database), NewBrokerRevocations(database.Pool)
		signInAll(t, people, other, "member-1", "member-2")
		signInAll(t, people, email, "member-1", "member-2", "member-1")
		if got := brokerRevocationsOf(t, database, email); !slices.Equal(got, reclaimed) {
			t.Fatalf("after member-1 took %s back, its revocations read %+v, want %+v", email, got, reclaimed)
		}
		if got, want := brokerRevocationsOf(t, database, other), []brokerRevocationRow{owedRow("member-1", "member-2", 1)}; !slices.Equal(got, want) {
			t.Fatalf("member-1's taking %s back touched %s's revocations: %+v, want %+v", email, other, got, want)
		}
		requirePaysTheHolderNext(t, revocations, "member-2")
	})
	t.Run("claimed for a call the broker did not confirm", func(t *testing.T) {
		database := migratedEmptyStore(t)
		people, revocations := brokerPeople(database), NewBrokerRevocations(database.Pool)
		signInAll(t, people, email, "member-1", "member-2")
		if owed := claimOf(t, revocations, email); len(owed) != 1 || owed[0].Subject != "member-1" {
			t.Fatalf("the first claim: %+v, want member-1's", owed)
		}
		ageClaims(t, database, email)
		signInAll(t, people, email, "member-1")
		if got := brokerRevocationsOf(t, database, email); !slices.Equal(got, reclaimed) {
			t.Fatalf("after member-1 took %s back, its revocations read %+v, want %+v", email, got, reclaimed)
		}
		requirePaysTheHolderNext(t, revocations, "member-2")
	})
	t.Run("claimed for a call still in flight", func(t *testing.T) {
		database := migratedEmptyStore(t)
		people, revocations := brokerPeople(database), NewBrokerRevocations(database.Pool)
		signInAll(t, people, email, "member-1", "member-2")
		inFlight := claimOf(t, revocations, email)
		if len(inFlight) != 1 || inFlight[0].Subject != "member-1" {
			t.Fatalf("the first claim: %+v, want member-1's", inFlight)
		}
		signInAll(t, people, email, "member-1")
		if got := brokerRevocationsOf(t, database, email); !slices.Equal(got, reclaimed) {
			t.Fatalf("after member-1 took %s back, its revocations read %+v, want %+v", email, got, reclaimed)
		}
		if owed := claimOf(t, revocations, email); len(owed) != 0 {
			t.Fatalf("a claim while member-1's call is in flight: %+v, want none", owed)
		}
		if settled, err := revocations.Settle(ctx, inFlight[0]); err != nil || settled {
			t.Fatalf("the late answer to member-1's call settled %t, %v; want nothing, the revocation settled already", settled, err)
		}
		ageClaims(t, database, email)
		requirePaysTheHolderNext(t, revocations, "member-2")
	})
	t.Run("taken back from a third account", func(t *testing.T) {
		database := migratedEmptyStore(t)
		people, revocations := brokerPeople(database), NewBrokerRevocations(database.Pool)
		signInAll(t, people, email, "member-1", "member-2", "member-3", "member-1")
		want := []brokerRevocationRow{
			{Issuer: canonicalIssuer, Subject: "member-1", HolderSubject: "member-2", Generation: 1, Done: true},
			owedRow("member-2", "member-3", 1),
			owedRow("member-3", "member-1", 1),
		}
		if got := brokerRevocationsOf(t, database, email); !slices.Equal(got, want) {
			t.Fatalf("after member-1 took %s back from member-3, its revocations read %+v, want %+v", email, got, want)
		}
		if owed := claimOf(t, revocations, email); len(owed) != 1 || owed[0].Subject != "member-2" {
			t.Fatalf("the first claim: %+v, want member-2's, which member-3 took", owed)
		}
	})
	t.Run("taken back from an account restricted since", func(t *testing.T) {
		database := migratedEmptyStore(t)
		people := brokerPeople(database)
		signInAll(t, people, email, "member-1", "member-2")
		if _, err := database.Pool.Exec(ctx, `update people set member_kind = 'restricted' where email = $1`, email); err != nil {
			t.Fatal(err)
		}
		signInAll(t, people, email, "member-1")
		if got := brokerRevocationsOf(t, database, email); !slices.Equal(got, reclaimed) {
			t.Fatalf("after member-1 took %s back from member-2, restricted by then, its revocations read %+v, want %+v: member-1's settled and member-2's owed", email, got, reclaimed)
		}
	})
}

// An account restricted since it took the address is revoked when the address is taken back, like
// a full member's. member-1 holds the address, member-2 takes it, and the pass pays member-1's
// revocation, after which the broker refuses member-1 at the address. member-2's hourly
// confirmation then restricts it under the same `sub` (ConfirmSubject), and member-1 takes the
// address back. That sign-in settles nothing - member-1's revocation is paid already - and records
// member-2's, naming member-1 as the holder: paying it ends what member-2 made at the broker while
// it was full and deletes the broker's refusal of member-1, which nothing else lifts. The next
// claim, past the retry window after member-1's, takes it.
func TestSignInSubjectRecordsTheRevocationOfAnAccountRestrictedSinceItTookTheAddress(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people, revocations := brokerPeople(database), NewBrokerRevocations(database.Pool)
	const email = "reassigned@second.example"
	signInAll(t, people, email, "member-1", "member-2")
	paid, err := revocations.Claim(ctx, time.Minute, 10)
	if err != nil || len(paid) != 1 || paid[0].Subject != "member-1" {
		t.Fatalf("the first claim: %+v, %v; want member-1's revocation alone", paid, err)
	}
	if settled, err := revocations.Settle(ctx, paid[0]); err != nil || !settled {
		t.Fatalf("settle member-1's revocation: %t, %v", settled, err)
	}
	if err := people.ConfirmSubject(ctx, email, "member-2", "refresh-member-2", "refresh-member-2-confirmed", model.MemberRestricted); err != nil {
		t.Fatalf("member-2's confirmation as restricted: %v", err)
	}
	if got := storedMemberKind(t, database, email); got != string(model.MemberRestricted) {
		t.Fatalf("after member-2's confirmation as restricted: member_kind = %q, want restricted", got)
	}

	signInAll(t, people, email, "member-1")
	want := []brokerRevocationRow{
		{Issuer: canonicalIssuer, Subject: "member-1", HolderSubject: "member-2", Generation: 1, Done: true},
		owedRow("member-2", "member-1", 1),
	}
	if got := brokerRevocationsOf(t, database, email); !slices.Equal(got, want) {
		t.Fatalf("after member-1 took %s back from member-2, restricted since, its revocations read %+v, want %+v: member-2's owed, naming member-1 as the holder", email, got, want)
	}
	ageClaims(t, database, email)
	owed, err := revocations.Claim(ctx, time.Minute, 10)
	wantOwed := OwedBrokerRevocation{Email: email, Issuer: canonicalIssuer, Subject: "member-2", HolderSubject: "member-1", Generation: 1,
		SessionGeneration: new(sessionGenerationOf(t, database.Pool, email))}
	if err != nil || len(owed) != 1 || !reflect.DeepEqual(owed[0], wantOwed) {
		t.Fatalf("the claim after member-1 took %s back: %s, %v; want %s alone", email, owedText(owed...), err, owedText(wantOwed))
	}
}

// A store built with no WithBrokerRevocations, as main builds one for a deployment with no secrets
// broker configured, records no broker revocation when a full member's account is replaced: no
// pass of it would pay one. The sessions and personal tokens end as they always did.
func TestSignInSubjectWithNoBrokerRecordsNoBrokerRevocation(t *testing.T) {
	database := migratedEmptyStore(t)
	const email = "reassigned@example.com"
	signInAll(t, NewPgPeopleStore(database.Pool, testSigningKey, nil), email, "member-1", "member-2")
	if got := brokerRevocationsOf(t, database, email); len(got) != 0 {
		t.Fatalf("a store with no broker recorded a broker revocation of %s: %+v, want none", email, got)
	}
}

// A revocation is recorded at the moment the replacing sign-in's transaction is about to commit, on
// the database's clock, and not at the moment the transaction began: the order of an address's
// replacements is the order their sign-ins committed, which their revocations are paid in. The
// people row's own update is held a second here, so the two moments differ by that second.
func TestSignInSubjectRecordsTheRevocationAtItsCommit(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	const email = "reassigned@example.com"
	signInAll(t, people, email, "member-1")
	if _, err := database.Pool.Exec(ctx, `
		create function hold_the_sign_in() returns trigger language plpgsql as $$ begin perform pg_sleep(1); return new; end $$;
		create trigger hold_the_sign_in before update on people for each row execute function hold_the_sign_in();
	`); err != nil {
		t.Fatal(err)
	}
	signInAll(t, people, email, "member-2")
	var began, recorded time.Time
	if err := database.Pool.QueryRow(ctx, `select p.signed_in_at, r.recorded_at from people p join broker_revocations r using (email) where email = $1`, email).Scan(&began, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Sub(began) < time.Second {
		t.Fatalf("the revocation of %s is recorded at %s, %s after its transaction began at %s, want at least the second the people row's update was held", email, recorded, recorded.Sub(began), began)
	}
}

// The revocation commits with the sign-in that owes it: a sign-in whose commit fails after the
// revocation was recorded rolls the revocation back with it, and the retry records it. The failure
// is a deferred trigger on the people row's new account, which fires as the sign-in commits, after
// its last statement, the revocation's insert: it raises one message when the transaction holds
// that revocation, and another when it holds none, so the test passes only over a revocation the
// transaction recorded and its rollback removed.
//
// Control in people.go: the revocation recorded after the commit, on the pool (the trigger finds
// none in the transaction, and the sign-in fails with the other message). The earlier form of this
// test, whose trigger refused the people update before the revocation was written, passed under it.
func TestSignInSubjectRecordsNoBrokerRevocationWhenItRollsBack(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	const email = "a.person@example.com"
	signInAll(t, people, email, "subject-1")
	if _, err := database.Pool.Exec(ctx, `
		create function refuse_the_commit() returns trigger language plpgsql as $$
		begin
			if exists (select 1 from broker_revocations where email = new.email and subject = 'subject-1' and done_at is null) then
				raise exception 'the test refuses the commit of a sign-in that recorded its revocation';
			end if;
			raise exception 'the test found no revocation recorded before the sign-in commits';
		end $$;
		create constraint trigger refuse_the_commit after update on people deferrable initially deferred
			for each row when (new.sub = 'subject-2') execute function refuse_the_commit();
	`); err != nil {
		t.Fatal(err)
	}
	if err := people.SignInSubject(ctx, email, "subject-2", "refresh-2", model.MemberFull); err == nil || !strings.Contains(err.Error(), "the test refuses the commit of a sign-in that recorded its revocation") {
		t.Fatalf("a sign-in whose commit fails after it recorded its revocation: err = %v, want the commit's error naming the recorded revocation", err)
	}
	if got := brokerRevocationsOf(t, database, email); len(got) != 0 {
		t.Fatalf("a sign-in that rolled back left a broker revocation of %s: %+v", email, got)
	}
	if _, err := database.Pool.Exec(ctx, `drop trigger refuse_the_commit on people`); err != nil {
		t.Fatal(err)
	}
	signInAll(t, people, email, "subject-2")
	if got, want := brokerRevocationsOf(t, database, email), []brokerRevocationRow{owedRow("subject-1", "subject-2", 1)}; !slices.Equal(got, want) {
		t.Fatalf("the retry's broker revocations of %s read %+v, want %+v", email, got, want)
	}
}

// A recorded revocation announces itself on BrokerRevocationsChannel when its transaction commits,
// which is what wakes the server's revocation loop; the loop's own claims and settlements announce
// nothing, so they do not wake it again.
func TestSignInSubjectAnnouncesTheBrokerRevocationAtCommit(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	listener, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `listen `+BrokerRevocationsChannel); err != nil {
		t.Fatal(err)
	}
	announced := func() bool {
		wait, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		_, err := listener.Conn().WaitForNotification(wait)
		return err == nil
	}
	const email = "a.person@example.com"
	signInAll(t, people, email, "subject-1")
	if announced() {
		t.Fatal("a first sign-in announced a broker revocation")
	}
	signInAll(t, people, email, "subject-2")
	if !announced() {
		t.Fatalf("the replacing sign-in of %s committed without announcing its broker revocation on %s", email, BrokerRevocationsChannel)
	}
	revocations := NewBrokerRevocations(database.Pool)
	owed, err := revocations.Claim(ctx, time.Minute, 10)
	if err != nil || len(owed) != 1 {
		t.Fatalf("claim: %+v, %v; want the one owed", owed, err)
	}
	if settled, err := revocations.Settle(ctx, owed[0]); err != nil || !settled {
		t.Fatalf("settle: %t, %v", settled, err)
	}
	if announced() {
		t.Fatal("the loop's claim or settlement announced a broker revocation, which would wake the loop again")
	}
}

// ageClaims moves every claim of email's revocations two minutes back, as the loop waits out its
// retry window.
func ageClaims(t *testing.T, database *Store, email string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update broker_revocations set tried_at = tried_at - interval '2 minutes' where email = $1`, email); err != nil {
		t.Fatal(err)
	}
}

// TestARevocationWithNoGenerationHoldsBackNoOther: member-2 takes the address from member-1, and
// member-3 from member-2, so two revocations are owed. Member-1's holds no session generation, as
// one an earlier head of this branch recorded holds none (written with the guard off), which the
// broker answers 400 on every call. It holds back no other row: a claim takes member-2's, and only
// once that settles does a claim take member-1's, whose call the broker refuses until an operator
// records it again.
//
// Control: let a row with no generation sort first and hold back the rest (the pay order before),
// and the first claim takes member-1's.
func TestARevocationWithNoGenerationHoldsBackNoOther(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	revocations := NewBrokerRevocations(database.Pool)
	const email = "nogeneration@example.com"
	signInAll(t, brokerPeople(database), email, "member-1", "member-2", "member-3")
	for _, statement := range []string{
		`alter table broker_revocations disable trigger broker_revocations_binding_columns`,
		`update broker_revocations set session_generation = null where email = '` + email + `' and subject = 'member-1'`,
		`alter table broker_revocations enable trigger broker_revocations_binding_columns`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	first, err := revocations.Claim(ctx, time.Minute, 10)
	if err != nil || len(first) != 1 || first[0].Subject != "member-2" {
		t.Fatalf("the first claim takes %s, %v; want member-2's alone: the row with no generation holds back no other", owedText(first...), err)
	}
	if settled, err := revocations.Settle(ctx, first[0]); err != nil || !settled {
		t.Fatalf("settle member-2's: %t, %v", settled, err)
	}
	ageClaims(t, database, email)
	if second, err := revocations.Claim(ctx, time.Minute, 10); err != nil || len(second) != 1 || second[0].Subject != "member-1" || second[0].SessionGeneration != nil {
		t.Fatalf("the next claim takes %s, %v; want member-1's, with no generation", owedText(second...), err)
	}
}

// TestAClaimedRevocationWithNoGenerationHoldsBackNoOther: member-2 takes the address from
// member-1, so member-1's revocation is owed; it holds no session generation, as one an earlier
// head of this branch recorded holds none (written with the guard off), and the broker answers it
// 400 on every call. A pass claims it, which stamps it tried_at, and then member-3 takes the
// address from member-2, recording member-2's revocation. That row is claimed at once, inside the
// retry window after the claim of the null row: a claim of a row that is never applied must hold
// back no other row of its address, by its claim as much as by being owed, or the address's later
// revocations would wait for it for ever, one retry window at a time.
//
// Control: drop the line of payableBrokerRevocation that leaves a row with no session generation
// out of the rows a claim counts against another (`other.session_generation is not null or ...`),
// and the second claim takes nothing.
func TestAClaimedRevocationWithNoGenerationHoldsBackNoOther(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	const email = "claimednogeneration@example.com"
	signInAll(t, people, email, "member-1", "member-2")
	for _, statement := range []string{
		`alter table broker_revocations disable trigger broker_revocations_binding_columns`,
		`update broker_revocations set session_generation = null where email = '` + email + `' and subject = 'member-1'`,
		`alter table broker_revocations enable trigger broker_revocations_binding_columns`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	claimed, err := revocations.Claim(ctx, time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].Subject != "member-1" || claimed[0].SessionGeneration != nil {
		t.Fatalf("the first claim takes %s, %v; want member-1's alone, with no generation", owedText(claimed...), err)
	}
	signInAll(t, people, email, "member-3")
	next, err := revocations.Claim(ctx, time.Minute, 10)
	if err != nil || len(next) != 1 || next[0].Subject != "member-2" || next[0].SessionGeneration == nil {
		t.Fatalf("the claim after the one of the row with no generation takes %s, %v; want member-2's, at its own generation: a claim of a row the broker never applies holds back no other", owedText(next...), err)
	}
	if wait, due, err := revocations.NextDue(ctx, time.Minute); err != nil || !due || wait <= 0 {
		t.Fatalf("NextDue after both claims = %s, due %t, %v; want both rows owed and neither claimable inside the window", wait, due, err)
	}
}

// TestAnAddressesRevocationsArePaidInGenerationOrderAfterTheClockStepsBack: member-2 takes the
// address from member-1, and member-3 from member-2, so two revocations are owed, member-1's at the
// lower session generation. The database's clock then stepped back between the two replacements:
// member-2's revocation, the later one, holds the earlier recorded_at (written with the guard off,
// as no statement of Dispatch can write a recording time). The broker judges a revoke by its
// generation, so the lower one is paid first: a claim, the operator's read (Owing) and NextDue
// take member-1's, and member-2's only once member-1's is settled.
//
// Control: order by recorded_at alone (the order before), and the claim takes member-2's.
func TestAnAddressesRevocationsArePaidInGenerationOrderAfterTheClockStepsBack(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	revocations := NewBrokerRevocations(database.Pool)
	const email = "stepped@example.com"
	signInAll(t, brokerPeople(database), email, "member-1", "member-2", "member-3")
	for _, statement := range []string{
		`alter table broker_revocations disable trigger broker_revocations_binding_columns`,
		`update broker_revocations set recorded_at = (select min(recorded_at) from broker_revocations where email = '` + email + `') - interval '2 minutes'
			where email = '` + email + `' and subject = 'member-2'`,
		`alter table broker_revocations enable trigger broker_revocations_binding_columns`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	owing, err := revocations.Owing(ctx, email)
	if err != nil || len(owing) != 2 || owing[0].Subject != "member-1" || owing[1].Subject != "member-2" ||
		owing[0].SessionGeneration == nil || owing[1].SessionGeneration == nil || *owing[0].SessionGeneration >= *owing[1].SessionGeneration {
		t.Fatalf("Owing(%s) = %s, %v; want member-1's, at the lower generation, before member-2's", email, owedText(owing...), err)
	}
	if _, due, err := revocations.NextDue(ctx, time.Minute); err != nil || !due {
		t.Fatalf("NextDue = due %t, %v; want one due", due, err)
	}
	first, err := revocations.Claim(ctx, time.Minute, 10)
	if err != nil || len(first) != 1 || first[0].Subject != "member-1" {
		t.Fatalf("the first claim takes %s, %v; want member-1's alone, the lower generation, though member-2's was recorded at the earlier time", owedText(first...), err)
	}
	if settled, err := revocations.Settle(ctx, first[0]); err != nil || !settled {
		t.Fatalf("settle member-1's: %t, %v", settled, err)
	}
	ageClaims(t, database, email)
	if second, err := revocations.Claim(ctx, time.Minute, 10); err != nil || len(second) != 1 || second[0].Subject != "member-2" {
		t.Fatalf("the next claim takes %s, %v; want member-2's", owedText(second...), err)
	}
}

// The loop claims a revocation once a retry window: a claim stamps it tried, so another process's
// pass inside the window, or this one's, leaves it, and one after the window takes it again. A
// settlement names the generation the claim read, so an account replaced again while the broker
// was being called stays owed. NextDue says when the next claim can take one.
func TestPeopleBrokerRevocationsAreClaimedOnceAndSettledByGeneration(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	const email = "a.person@example.com"
	if _, due, err := revocations.NextDue(ctx, time.Minute); err != nil || due {
		t.Fatalf("NextDue with nothing owed: due %t, %v; want none", due, err)
	}
	signInAll(t, people, email, "subject-1", "subject-2")
	if wait, due, err := revocations.NextDue(ctx, time.Minute); err != nil || !due || wait > 0 {
		t.Fatalf("NextDue with one never tried: %s due %t, %v; want due now", wait, due, err)
	}
	owed, err := revocations.Claim(ctx, time.Minute, 10)
	want := OwedBrokerRevocation{Email: email, Issuer: canonicalIssuer, Subject: "subject-1", HolderSubject: "subject-2", Generation: 1,
		SessionGeneration: new(sessionGenerationOf(t, database.Pool, email))}
	if err != nil || len(owed) != 1 || !reflect.DeepEqual(owed[0], want) {
		t.Fatalf("the first claim: %s, %v; want %s", owedText(owed...), err, owedText(want))
	}
	if again, err := revocations.Claim(ctx, time.Minute, 10); err != nil || len(again) != 0 {
		t.Fatalf("a claim inside the retry window: %+v, %v; want none", again, err)
	}
	if wait, due, err := revocations.NextDue(ctx, time.Minute); err != nil || !due || wait <= 50*time.Second || wait > time.Minute {
		t.Fatalf("NextDue after a claim: %s due %t, %v; want about a minute", wait, due, err)
	}
	ageClaims(t, database, email)
	if owed, err = revocations.Claim(ctx, time.Minute, 10); err != nil || len(owed) != 1 || !reflect.DeepEqual(owed[0], want) {
		t.Fatalf("the claim past the retry window: %s, %v; want %s again", owedText(owed...), err, owedText(want))
	}

	signInAll(t, people, email, "subject-3", "subject-4")
	if settled, err := revocations.Settle(ctx, owed[0]); err != nil || !settled {
		t.Fatalf("settling generation 1 of subject-1's revocation: %t, %v", settled, err)
	}
	ageClaims(t, database, email)
	if owed, err = revocations.Claim(ctx, time.Minute, 10); err != nil || len(owed) != 1 || owed[0].Subject != "subject-2" {
		t.Fatalf("the claim after subject-1's settlement: %+v, %v; want subject-2's, recorded next", owed, err)
	}
	signInAll(t, people, email, "subject-2", "subject-1")
	if settled, err := revocations.Settle(ctx, owed[0]); err != nil || settled {
		t.Fatalf("settling generation 1 of subject-2's revocation after subject-2 was replaced again: %t, %v; want it left owed", settled, err)
	}
	if got := brokerRevocationsOf(t, database, email); !slices.Equal(got, []brokerRevocationRow{
		{Issuer: canonicalIssuer, Subject: "subject-1", HolderSubject: "subject-2", Generation: 1, Done: true},
		owedRow("subject-3", "subject-4", 1), owedRow("subject-4", "subject-2", 1), owedRow("subject-2", "subject-1", 2),
	}) {
		t.Fatalf("after a stale settlement the revocations read %+v, want subject-1's settled, then subject-3's and subject-4's owed, and subject-2's owed at generation 2", got)
	}
	for _, subject := range []string{"subject-3", "subject-4", "subject-2"} {
		ageClaims(t, database, email)
		if owed, err = revocations.Claim(ctx, time.Minute, 10); err != nil || len(owed) != 1 || owed[0].Subject != subject {
			t.Fatalf("the claim in turn: %+v, %v; want %s's", owed, err, subject)
		}
		if settled, err := revocations.Settle(ctx, owed[0]); err != nil || !settled {
			t.Fatalf("settling %s's revocation: %t, %v", subject, settled, err)
		}
	}
	ageClaims(t, database, email)
	if again, err := revocations.Claim(ctx, 0, 10); err != nil || len(again) != 0 {
		t.Fatalf("a claim after every settlement: %+v, %v; want none", again, err)
	}
	if _, due, err := revocations.NextDue(ctx, time.Minute); err != nil || due {
		t.Fatalf("NextDue with every revocation settled: due %t, %v; want none", due, err)
	}
}

// A revocation tells the broker to stop refusing the account that took the address, so an
// address's revocations are paid one at a time, in the order their sign-ins committed, and never
// while a call of another is still in flight. Here member-1's revocation (member-2 took the address)
// is claimed, its call in flight; meanwhile member-1 takes the address back and member-3 takes it
// from member-1. The revocation of member-2 (member-1 took it) is now the earliest owed, but no
// claim takes it until the retry window after member-1's claim, by when that call is over: answered
// after member-2's, it would end the broker's refusal of member-2. Then member-2's is paid, and
// member-1's again, last, the retry window after member-2's claim. NextDue counts that wait.
func TestBrokerRevocationsOfOneAddressArePaidOneAtATimeInCommitOrder(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	const email = "reassigned@example.com"
	signInAll(t, people, email, "member-1", "member-2")
	inFlight, err := revocations.Claim(ctx, time.Minute, 10)
	if err != nil || len(inFlight) != 1 || inFlight[0].Subject != "member-1" || inFlight[0].HolderSubject != "member-2" {
		t.Fatalf("the first claim: %+v, %v; want member-1's, which member-2 took", inFlight, err)
	}
	signInAll(t, people, email, "member-1", "member-3")
	if owed, err := revocations.Claim(ctx, time.Minute, 10); err != nil || len(owed) != 0 {
		t.Fatalf("a claim while member-1's first call is in flight: %+v, %v; want none of %s", owed, err, email)
	}
	if wait, due, err := revocations.NextDue(ctx, time.Minute); err != nil || !due || wait <= 50*time.Second || wait > time.Minute {
		t.Fatalf("NextDue while member-1's first call is in flight: %s due %t, %v; want the retry window after its claim", wait, due, err)
	}
	ageClaims(t, database, email)
	owed, err := revocations.Claim(ctx, time.Minute, 10)
	if err != nil || len(owed) != 1 || owed[0].Subject != "member-2" || owed[0].HolderSubject != "member-1" {
		t.Fatalf("the claim past the window: %+v, %v; want member-2's, which member-1 took, recorded before member-1's again", owed, err)
	}
	if settled, err := revocations.Settle(ctx, inFlight[0]); err != nil || settled {
		t.Fatalf("the late answer to member-1's first call settled %t, %v; want nothing, member-1 owed again under generation 2", settled, err)
	}
	if settled, err := revocations.Settle(ctx, owed[0]); err != nil || !settled {
		t.Fatalf("settling member-2's: %t, %v", settled, err)
	}
	if owed, err := revocations.Claim(ctx, time.Minute, 10); err != nil || len(owed) != 0 {
		t.Fatalf("a claim inside the window after member-2's claim: %+v, %v; want none", owed, err)
	}
	ageClaims(t, database, email)
	want := OwedBrokerRevocation{Email: email, Issuer: canonicalIssuer, Subject: "member-1", HolderSubject: "member-3", Generation: 2,
		SessionGeneration: new(sessionGenerationOf(t, database.Pool, email))}
	if owed, err = revocations.Claim(ctx, time.Minute, 10); err != nil || len(owed) != 1 || !reflect.DeepEqual(owed[0], want) {
		t.Fatalf("the last claim: %s, %v; want %s: member-1's at generation 2, which member-3 took", owedText(owed...), err, owedText(want))
	}
}

// Owed counts the revocations the broker has not confirmed and how long the oldest of them has
// been owed since its replacing commit, on the database's clock: what an operator watches for a
// revocation the broker never confirms. With none owed it answers zero and no age.
func TestBrokerRevocationsOwedCountsTheUnsettledAndAgesTheOldest(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	if count, oldest, err := revocations.Owed(ctx); err != nil || count != 0 || oldest != 0 {
		t.Fatalf("Owed with nothing recorded: %d, %s, %v; want 0, 0", count, oldest, err)
	}
	for _, email := range []string{"first@example.com", "second@example.com"} {
		signInAll(t, people, email, "member-1", "member-2")
	}
	// A recording time moves only when the revocation is recorded again (migration
	// 20261118152422), so ten minutes back is written with the guard off, in one transaction.
	ageRecorded := []string{
		`alter table broker_revocations disable trigger broker_revocations_binding_columns`,
		`update broker_revocations set recorded_at = recorded_at - interval '10 minutes' where email = 'first@example.com'`,
		`alter table broker_revocations enable trigger broker_revocations_binding_columns`,
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range ageRecorded {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if count, oldest, err := revocations.Owed(ctx); err != nil || count != 2 || oldest < 10*time.Minute || oldest > 11*time.Minute {
		t.Fatalf("Owed with two owed, the oldest recorded ten minutes ago: %d, %s, %v; want 2, about ten minutes", count, oldest, err)
	}
	if _, err := database.Pool.Exec(ctx, `update broker_revocations set done_at = now() where email = 'first@example.com'`); err != nil {
		t.Fatal(err)
	}
	if count, oldest, err := revocations.Owed(ctx); err != nil || count != 1 || oldest > time.Minute {
		t.Fatalf("Owed once the oldest settled: %d, %s, %v; want 1, the other's age of seconds", count, oldest, err)
	}
}

// Clear settles, without the broker, exactly the revocations an operator's read of the address saw
// (Owing), each at the generation that read answered, as an operator does for one the broker never
// confirms: a revocation recorded after the read, and one owed again under a later generation since,
// stay owed, and another address's is never touched. The address is read in any case and spacing.
func TestBrokerRevocationsClearSettlesOnlyWhatTheOperatorsReadSaw(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	revocations := NewBrokerRevocations(database.Pool)
	signInAll(t, people, "stuck@example.com", "member-1", "member-2", "member-3")
	signInAll(t, people, "other@example.com", "member-1", "member-2")
	read, err := revocations.Owing(ctx, " Stuck@Example.com ")
	if err != nil || len(read) != 2 || read[0].Subject != "member-1" || read[1].Subject != "member-2" {
		t.Fatalf("Owing: %+v, %v; want member-1's and member-2's, in that order", read, err)
	}
	seen := []BrokerRevocationSeen{{Subject: read[0].Subject, Generation: read[0].Generation}, {Subject: read[1].Subject, Generation: read[1].Generation}}

	// After the operator's read: member-1 takes the address back and member-4 takes it, so
	// member-1's revocation is owed again under generation 2 and member-3's is recorded.
	signInAll(t, people, "stuck@example.com", "member-1", "member-4")
	cleared, err := revocations.Clear(ctx, " Stuck@Example.com ", seen)
	if err != nil || len(cleared) != 1 || cleared[0].Subject != "member-2" || cleared[0].Generation != 1 {
		t.Fatalf("Clear of what the read saw: %+v, %v; want member-2's generation 1 alone", cleared, err)
	}
	want := []brokerRevocationRow{
		{Issuer: canonicalIssuer, Subject: "member-2", HolderSubject: "member-3", Generation: 1, Done: true},
		owedRow("member-3", "member-1", 1),
		owedRow("member-1", "member-4", 2),
	}
	if got := brokerRevocationsOf(t, database, "stuck@example.com"); !slices.Equal(got, want) {
		t.Fatalf("after Clear the revocations of stuck@example.com read %+v, want %+v: member-1's owed again and member-3's, recorded after the read, still owed", got, want)
	}
	if got := brokerRevocationsOf(t, database, "other@example.com"); len(got) != 1 || got[0].Done {
		t.Fatalf("Clear of stuck@example.com settled other@example.com's too: %+v", got)
	}
	if cleared, err := revocations.Clear(ctx, "stuck@example.com", seen); err != nil || len(cleared) != 0 {
		t.Fatalf("Clear of what is settled or owed again since: %+v, %v; want nothing cleared", cleared, err)
	}
	if cleared, err := revocations.Clear(ctx, "nobody@example.com", seen); err != nil || len(cleared) != 0 {
		t.Fatalf("Clear of an address with no revocation: %+v, %v; want nothing cleared", cleared, err)
	}
}
