package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// holdRow is what people holds of a hold for one row, as the table stores it.
type holdRow struct {
	refreshToken                                    *string
	confirmedAt, heldSince, signedOutAt, signedInAt *time.Time
	hostedDomain                                    *string
	confirmTriedAt, outlivingCheckedAt              *time.Time
}

// readHoldRow reads email's row as holdRow.
func readHoldRow(t *testing.T, database *Store, email string) holdRow {
	t.Helper()
	var row holdRow
	if err := database.Pool.QueryRow(context.Background(), `
		select refresh_token, confirmed_at, held_since, signed_out_at, signed_in_at, hosted_domain,
			confirm_tried_at, outliving_checked_at
		from people where email = $1
	`, email).Scan(&row.refreshToken, &row.confirmedAt, &row.heldSince, &row.signedOutAt, &row.signedInAt,
		&row.hostedDomain, &row.confirmTriedAt, &row.outlivingCheckedAt); err != nil {
		t.Fatalf("read the people row of %s: %v", email, err)
	}
	return row
}

// databaseNow is the database's clock_timestamp(), the clock every write under test dates by.
func databaseNow(t *testing.T, database *Store) time.Time {
	t.Helper()
	var now time.Time
	if err := database.Pool.QueryRow(context.Background(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return now
}

// TestHoldTriggerHoldsNobodyElse (c-trigger): only an end in EndHold records a hold. A sign-out (End,
// KeepAfterSignOut, ForgetAfterSignOut), the boot's retirement of a plain-text refresh token, the
// end of a row with no recorded Google account (EndForget, as Membership ends it), an EndKeep, and
// an EndHold of a refresh read before the same account signed in again, which the guard refuses,
// each leave held_since null and the row not held.
//
// Control: write held_since on every end (endSubjectSQL's EndForget and EndKeep set it too); the
// test fails naming the row with no recorded Google account and the kept row.
func TestHoldTriggerHoldsNobodyElse(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	_, flow := revokingPool(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, flow)
	notHeld := func(what, email string) {
		t.Helper()
		if row := readHoldRow(t, database, email); row.heldSince != nil {
			t.Errorf("%s: held_since = %s, want null", what, row.heldSince)
		}
		if membership, found, err := people.Membership(ctx, email); err != nil || !found || membership.Held {
			t.Errorf("%s: Membership = %+v, found %t, %v; want the row, not held", what, membership, found, err)
		}
	}
	signedIn := func(email, subject, token string, kind model.MemberKind) auth.PersonMembership {
		t.Helper()
		if err := people.SignInSubject(ctx, email, subject, token, "example.com", kind); err != nil {
			t.Fatalf("the sign-in of %s: %v", email, err)
		}
		read, _, err := people.Membership(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		return read
	}

	signedIn("end@example.com", "g-end", "refresh-end", model.MemberFull)
	if err := people.End(ctx, "end@example.com"); err != nil {
		t.Fatal(err)
	}
	notHeld("a sign-out that forgets (End)", "end@example.com")

	kept := signedIn("kept-sign-out@example.com", "g-kept", "refresh-kept", model.MemberFull)
	if err := people.KeepAfterSignOut(ctx, "kept-sign-out@example.com", kept.SignedInAt); err != nil {
		t.Fatal(err)
	}
	notHeld("a sign-out that keeps the token", "kept-sign-out@example.com")

	forgot := signedIn("forgot-sign-out@example.com", "g-forgot", "refresh-forgot", model.MemberFull)
	if err := people.ForgetAfterSignOut(ctx, "forgot-sign-out@example.com", forgot.SignedInAt); err != nil {
		t.Fatal(err)
	}
	notHeld("a sign-out that forgets the token", "forgot-sign-out@example.com")

	storePlainRefreshToken(t, database, "plain@example.com", "plain-token")
	if retired, failed, err := people.RetirePlainRefreshTokens(ctx); err != nil || retired != 1 || failed != 0 {
		t.Fatalf("RetirePlainRefreshTokens = %d retired, %d failed, %v; want 1, 0", retired, failed, err)
	}
	notHeld("the boot's retirement of a plain-text refresh token", "plain@example.com")

	if err := people.SignIn(ctx, "no-account@example.com", "refresh-pool"); err != nil {
		t.Fatal(err)
	}
	if ended, err := people.EndSubject(ctx, "no-account@example.com", "", "refresh-pool", auth.EndForget); err != nil || !ended {
		t.Fatalf("the end of a row with no recorded Google account: ended %t, %v; want true", ended, err)
	}
	notHeld("the row with no recorded Google account", "no-account@example.com")

	dana := signedIn("dana@outside.example", "g-dana", "refresh-dana", model.MemberRestricted)
	if ended, err := people.EndSubject(ctx, "dana@outside.example", dana.Subject, dana.RefreshToken, auth.EndKeep); err != nil || !ended {
		t.Fatalf("dana's EndKeep: ended %t, %v; want true", ended, err)
	}
	notHeld("the kept row (dana's list refusal)", "dana@outside.example")

	stale := signedIn("ada@example.com", "g-ada", "refresh-1", model.MemberFull)
	signedIn("ada@example.com", "g-ada", "refresh-2", model.MemberFull)
	if ended, err := people.EndSubject(ctx, "ada@example.com", stale.Subject, stale.RefreshToken, auth.EndHold); err != nil || ended {
		t.Fatalf("a hold of the refresh read before the same account signed in again: ended %t, %v; want false", ended, err)
	}
	notHeld("a refresh read before the same account signed in again, and refused after", "ada@example.com")
	if membership, _, _ := people.Membership(ctx, "ada@example.com"); membership.RefreshToken != "refresh-2" {
		t.Errorf("after the stale hold ada's row holds %q, want the newer sign-in's refresh-2", membership.RefreshToken)
	}

	// The one end that holds, so the reads above can see a hold.
	held := signedIn("leaver@example.com", "g-leaver", "refresh-leaver", model.MemberFull)
	if ended, err := people.EndSubject(ctx, "leaver@example.com", held.Subject, held.RefreshToken, auth.EndHold); err != nil || !ended {
		t.Fatalf("the leaver's EndHold: ended %t, %v; want true", ended, err)
	}
	if row := readHoldRow(t, database, "leaver@example.com"); row.heldSince == nil || row.refreshToken != nil || row.confirmedAt != nil {
		t.Errorf("after EndHold the leaver's row = %+v, want held_since set and no refresh token or confirmation", row)
	}
	if membership, _, err := people.Membership(ctx, "leaver@example.com"); err != nil || !membership.Held {
		t.Errorf("after EndHold the leaver's Membership = %+v, %v; want held", membership, err)
	}
}

// An EndKeep ends the sessions and keeps the refresh token, and sets confirmed_at and signed_out_at
// to the end's time, one clock_timestamp() of the end's transaction; the row reads signed out and
// not held. An EndKeep whose guard misses writes nothing.
func TestPgPeopleStoreEndKeepKeepsTheToken(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := NewPgSessionStore(database.Pool)
	const email = "dana@outside.example"
	if err := people.SignInSubject(ctx, email, "g-dana", "refresh-dana", "", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	generation, err := sessions.EnsureSession(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '2 hours' where email = $1`, email); err != nil {
		t.Fatal(err)
	}
	if ended, err := people.EndSubject(ctx, email, "g-dana", "refresh-other", auth.EndKeep); err != nil || ended {
		t.Fatalf("an EndKeep naming another refresh token: ended %t, %v; want false", ended, err)
	}
	if row := readHoldRow(t, database, email); row.signedOutAt != nil {
		t.Fatalf("an EndKeep whose guard missed set signed_out_at %s", row.signedOutAt)
	}
	before := databaseNow(t, database)
	if ended, err := people.EndSubject(ctx, email, "g-dana", "refresh-dana", auth.EndKeep); err != nil || !ended {
		t.Fatalf("EndKeep: ended %t, %v; want true", ended, err)
	}
	after := databaseNow(t, database)
	row := readHoldRow(t, database, email)
	if row.refreshToken == nil || row.heldSince != nil {
		t.Fatalf("after EndKeep the row = %+v, want its refresh token kept and no hold", row)
	}
	if row.confirmedAt == nil || row.signedOutAt == nil || !row.confirmedAt.Equal(*row.signedOutAt) ||
		row.confirmedAt.Before(before) || row.confirmedAt.After(after) {
		t.Fatalf("after EndKeep confirmed_at = %v and signed_out_at = %v, want both the end's time, between %s and %s", row.confirmedAt, row.signedOutAt, before, after)
	}
	membership, _, err := people.Membership(ctx, email)
	if err != nil || membership.RefreshToken != "refresh-dana" || !membership.SignedOut || membership.Held || !confirmedJustNow(membership) {
		t.Fatalf("after EndKeep Membership = %+v, %v; want refresh-dana kept, signed out, not held, confirmed just now", membership, err)
	}
	if got, _ := sessionGeneration(t, sessions, email); got != generation+1 {
		t.Fatalf("EndKeep left the session generation at %d, want %d", got, generation+1)
	}
}

// A recorded hold is released by any sign-in: this build's SignInSubject sets held_since to null,
// and an older build's sign-in, which sets only signed_in_at = now(), dates the sign-in after the
// hold.
func TestPgPeopleStoreHoldIsReleasedByAnySignIn(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	hold := func(email, token string) {
		t.Helper()
		if err := people.SignInSubject(ctx, email, "g-"+email, token, "example.com", model.MemberFull); err != nil {
			t.Fatal(err)
		}
		if ended, err := people.EndSubject(ctx, email, "g-"+email, token, auth.EndHold); err != nil || !ended {
			t.Fatalf("the hold of %s: ended %t, %v; want true", email, ended, err)
		}
		if membership, _, err := people.Membership(ctx, email); err != nil || !membership.Held {
			t.Fatalf("after the hold %s reads %+v, %v; want held", email, membership, err)
		}
	}
	hold("ada@example.com", "refresh-1")
	if err := people.SignInSubject(ctx, "ada@example.com", "g-ada@example.com", "refresh-2", "example.com", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if membership, _, err := people.Membership(ctx, "ada@example.com"); err != nil || membership.Held || membership.RefreshToken != "refresh-2" {
		t.Fatalf("after this build's sign-in ada reads %+v, %v; want not held, with refresh-2", membership, err)
	}
	if row := readHoldRow(t, database, "ada@example.com"); row.heldSince != nil {
		t.Fatalf("this build's sign-in left held_since %s, want null", row.heldSince)
	}
	hold("bob@example.com", "refresh-3")
	if _, err := database.Pool.Exec(ctx, `update people set signed_in_at = now() where email = $1`, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if membership, _, err := people.Membership(ctx, "bob@example.com"); err != nil || membership.Held {
		t.Fatalf("after an older build's sign-in bob reads %+v, %v; want not held", membership, err)
	}
}

// A sign-out that keeps the token marks the row signed out only while signed_in_at is the one it
// read; ClaimSignedOutHolder takes each signed-out row with a token once in turn, though the first
// 20 it took stay signed out, and takes none again within the hour; ForgetSignedOut forgets only
// while the claim's signed_out_at and signed_in_at stand, so another sign-out since, or an older
// build's sign-in (which sets only signed_in_at), leaves the row as it is, and it answers whether
// it forgot.
func TestPgPeopleStoreSignedOutRows(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	signIn := func(email string) auth.PersonMembership {
		t.Helper()
		if err := people.SignInSubject(ctx, email, "g-"+email, "refresh-"+email, "", model.MemberFull); err != nil {
			t.Fatal(err)
		}
		read, _, err := people.Membership(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		return read
	}

	carol := signIn("carol@example.com")
	if err := people.KeepAfterSignOut(ctx, "carol@example.com", carol.SignedInAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if membership, _, _ := people.Membership(ctx, "carol@example.com"); membership.SignedOut {
		t.Fatal("KeepAfterSignOut with a stale signed_in_at marked the row signed out")
	}
	if err := people.KeepAfterSignOut(ctx, "carol@example.com", carol.SignedInAt); err != nil {
		t.Fatal(err)
	}
	if membership, _, _ := people.Membership(ctx, "carol@example.com"); !membership.SignedOut || membership.RefreshToken == "" || !confirmedJustNow(membership) {
		t.Fatalf("after KeepAfterSignOut carol reads %+v, want signed out with her token and confirmation kept", membership)
	}
	bob := signIn("bob@example.com")
	if err := people.ForgetAfterSignOut(ctx, "bob@example.com", bob.SignedInAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if membership, _, _ := people.Membership(ctx, "bob@example.com"); membership.RefreshToken == "" {
		t.Fatal("ForgetAfterSignOut with a stale signed_in_at forgot the token")
	}
	if err := people.ForgetAfterSignOut(ctx, "bob@example.com", bob.SignedInAt); err != nil {
		t.Fatal(err)
	}
	if membership, _, _ := people.Membership(ctx, "bob@example.com"); membership.RefreshToken != "" || membership.Confirmed || membership.SignedOut {
		t.Fatalf("after ForgetAfterSignOut bob reads %+v, want no token, no confirmation, not signed out", membership)
	}

	// 20 more signed-out rows, so carol and 20 others, 21 in all, are due.
	for i := range 20 {
		email := fmt.Sprintf("kept-%02d@example.com", i)
		read := signIn(email)
		if err := people.KeepAfterSignOut(ctx, email, read.SignedInAt); err != nil {
			t.Fatal(err)
		}
	}
	signIn("signed-in@example.com") // Not signed out: never claimed.
	claimed := map[string]bool{}
	for range 21 {
		email, signedOutAt, signedInAt, ok, err := people.ClaimSignedOutHolder(ctx, time.Hour)
		if err != nil || !ok {
			t.Fatalf("claim %d: %q, %t, %v; want a row", len(claimed)+1, email, ok, err)
		}
		if claimed[email] {
			t.Fatalf("claim %d took %s again, though 21 rows are due", len(claimed)+1, email)
		}
		row := readHoldRow(t, database, email)
		if !signedOutAt.Equal(*row.signedOutAt) || !signedInAt.Equal(*row.signedInAt) || row.outlivingCheckedAt == nil {
			t.Fatalf("the claim of %s answered %s and %s, and the row reads %+v; want the row's own, with outliving_checked_at set", email, signedOutAt, signedInAt, row)
		}
		claimed[email] = true
	}
	if !claimed["carol@example.com"] || claimed["signed-in@example.com"] {
		t.Fatalf("the claims took %v; want carol and the 20 kept rows, not the row signed in", claimed)
	}
	if email, _, _, ok, err := people.ClaimSignedOutHolder(ctx, time.Hour); err != nil || ok {
		t.Fatalf("a 22nd claim within the hour: %q, %t, %v; want none due", email, ok, err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set outliving_checked_at = now() - interval '61 minutes' where email = 'carol@example.com'`); err != nil {
		t.Fatal(err)
	}
	email, signedOutAt, signedInAt, ok, err := people.ClaimSignedOutHolder(ctx, time.Hour)
	if err != nil || !ok || email != "carol@example.com" {
		t.Fatalf("the claim after carol's check is an hour old: %q, %t, %v; want carol again", email, ok, err)
	}

	// Another sign-out since the claim: signed_out_at moved.
	if _, err := database.Pool.Exec(ctx, `update people set signed_out_at = clock_timestamp() where email = 'carol@example.com'`); err != nil {
		t.Fatal(err)
	}
	if forgot, err := people.ForgetSignedOut(ctx, "carol@example.com", signedOutAt, signedInAt); err != nil || forgot {
		t.Fatalf("ForgetSignedOut with a stale signed_out_at answered forgot %t, %v; want false", forgot, err)
	}
	if membership, _, _ := people.Membership(ctx, "carol@example.com"); membership.RefreshToken == "" {
		t.Fatal("ForgetSignedOut with a stale signed_out_at forgot carol's token")
	}
	// An older build's sign-in since the claim: signed_in_at moved, and the row no longer reads
	// signed out.
	row := readHoldRow(t, database, "kept-00@example.com")
	if _, err := database.Pool.Exec(ctx, `update people set signed_in_at = now() where email = 'kept-00@example.com'`); err != nil {
		t.Fatal(err)
	}
	if forgot, err := people.ForgetSignedOut(ctx, "kept-00@example.com", *row.signedOutAt, *row.signedInAt); err != nil || forgot {
		t.Fatalf("ForgetSignedOut with a stale signed_in_at answered forgot %t, %v; want false", forgot, err)
	}
	if membership, _, _ := people.Membership(ctx, "kept-00@example.com"); membership.RefreshToken == "" || membership.SignedOut {
		t.Fatalf("ForgetSignedOut with a stale signed_in_at: kept-00 reads %+v, want its token kept and the row signed in", membership)
	}
	// The claim's own values: the forget writes.
	row = readHoldRow(t, database, "kept-01@example.com")
	if forgot, err := people.ForgetSignedOut(ctx, "kept-01@example.com", *row.signedOutAt, *row.signedInAt); err != nil || !forgot {
		t.Fatalf("ForgetSignedOut with the claim's values answered forgot %t, %v; want true", forgot, err)
	}
	if membership, _, _ := people.Membership(ctx, "kept-01@example.com"); membership.RefreshToken != "" || membership.Confirmed || membership.SignedOut {
		t.Fatalf("ForgetSignedOut with the claim's values: kept-01 reads %+v, want no token, no confirmation, not signed out", membership)
	}
}

// BootPeople counts each row once, as checkable (a refresh token, opening or not, and a recorded
// Google account), held (a recorded hold) or uncheckable (every other row), and lists the distinct
// hosted domains of full members' rows that hold a refresh token, sorted byte for byte.
func TestPgPeopleStoreBootPeople(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	if census, err := people.BootPeople(ctx); err != nil || census.Checkable+census.Held+census.Uncheckable != 0 || len(census.FullMemberDomains) != 0 {
		t.Fatalf("BootPeople of no rows = %+v, %v; want nothing", census, err)
	}
	for _, row := range []struct {
		email, domain string
		kind          model.MemberKind
	}{
		{"ada@example.com", "example.com", model.MemberFull},
		{"bob@example.com", "example.com", model.MemberFull},
		{"carol@example.com", "Example.com", model.MemberFull},
		{"dana@outside.example", "outside.example", model.MemberRestricted},
		{"erin@example.com", "", model.MemberFull},
		{"frank@example.com", "forgotten.example", model.MemberFull},
		{"leaver@example.com", "held.example", model.MemberFull},
	} {
		if err := people.SignInSubject(ctx, row.email, "g-"+row.email, "refresh-"+row.email, row.domain, row.kind); err != nil {
			t.Fatal(err)
		}
	}
	if err := people.End(ctx, "frank@example.com"); err != nil {
		t.Fatal(err)
	}
	if ended, err := people.EndSubject(ctx, "leaver@example.com", "g-leaver@example.com", "refresh-leaver@example.com", auth.EndHold); err != nil || !ended {
		t.Fatalf("the leaver's hold: %t, %v", ended, err)
	}
	storePlainRefreshToken(t, database, "plain@example.com", "plain-token") // A token and no account.
	if err := people.Record(ctx, "recorded@example.com"); err != nil {
		t.Fatal(err)
	}
	census, err := people.BootPeople(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Checkable: ada, bob, carol, dana, erin. Held: the leaver. Uncheckable: frank, plain, recorded.
	if census.Checkable != 5 || census.Held != 1 || census.Uncheckable != 3 {
		t.Errorf("BootPeople counts = %d checkable, %d held, %d uncheckable; want 5, 1, 3", census.Checkable, census.Held, census.Uncheckable)
	}
	if want := []string{"Example.com", "example.com"}; !slices.Equal(census.FullMemberDomains, want) {
		t.Errorf("FullMemberDomains = %q, want %q", census.FullMemberDomains, want)
	}
}

// A sign-in and a confirmation record the ID token's hd in hosted_domain, null for none, and
// ForgetHostedDomains sets every one to null and counts the rows it changed.
func TestPgPeopleStoreRecordsTheHostedDomain(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	domain := func(email string) string {
		t.Helper()
		row := readHoldRow(t, database, email)
		if row.hostedDomain == nil {
			return "<null>"
		}
		return *row.hostedDomain
	}
	if err := people.SignInSubject(ctx, "ada@example.com", "g-ada", "refresh-1", "example.com", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if err := people.SignInSubject(ctx, "dana@outside.example", "g-dana", "refresh-2", "", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	if got, got2 := domain("ada@example.com"), domain("dana@outside.example"); got != "example.com" || got2 != "<null>" {
		t.Fatalf("after the sign-ins hosted_domain = %q and %q, want example.com and null", got, got2)
	}
	if membership, _, _ := people.Membership(ctx, "ada@example.com"); membership.HostedDomain != "example.com" {
		t.Fatalf("Membership.HostedDomain = %q, want example.com", membership.HostedDomain)
	}
	if err := people.ConfirmSubject(ctx, "ada@example.com", "g-ada", "refresh-1", "refresh-3", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if err := people.ConfirmSubject(ctx, "dana@outside.example", "g-dana", "refresh-2", "refresh-4", "outside.example", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	if got, got2 := domain("ada@example.com"), domain("dana@outside.example"); got != "<null>" || got2 != "outside.example" {
		t.Fatalf("after the confirmations hosted_domain = %q and %q, want null and outside.example", got, got2)
	}
	if changed, err := people.ForgetHostedDomains(ctx); err != nil || changed != 1 {
		t.Fatalf("ForgetHostedDomains = %d, %v; want 1 row", changed, err)
	}
	if got := domain("dana@outside.example"); got != "<null>" {
		t.Fatalf("after ForgetHostedDomains hosted_domain = %q, want null", got)
	}
	if _, err := database.Pool.Exec(ctx, `update people set hosted_domain = '' where email = 'ada@example.com'`); err == nil {
		t.Fatal("the table accepted an empty hosted_domain")
	}
}

// TestClaimOverdueConfirmationTriesARowOnceAnHour: rows whose confirmation is over an hour old and
// that were never tried are claimed, the oldest confirmation first, each once; a row claimed now is
// not claimed again until its try is an hour old, though its confirmation stays old; a fresh
// confirmation and a row with no refresh token are never claimed.
func TestClaimOverdueConfirmationTriesARowOnceAnHour(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	for _, row := range []struct {
		email string
		age   string
	}{
		{"three-hours@example.com", "3 hours"},
		{"two-hours@example.com", "2 hours"},
		{"four-hours@example.com", "4 hours"},
		{"fresh@example.com", "10 minutes"},
	} {
		if err := people.SignInSubject(ctx, row.email, "g-"+row.email, "refresh-"+row.email, "", model.MemberFull); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - $2::interval where email = $1`, row.email, row.age); err != nil {
			t.Fatal(err)
		}
	}
	if err := people.Record(ctx, "no-token@example.com"); err != nil {
		t.Fatal(err)
	}
	var order []string
	for {
		email, ok, err := people.ClaimOverdueConfirmation(ctx, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if len(order) > 3 {
			t.Fatalf("claims went on past the three due rows: %v, then %s", order, email)
		}
		if row := readHoldRow(t, database, email); row.confirmTriedAt == nil {
			t.Fatalf("the claim of %s set no confirm_tried_at", email)
		}
		order = append(order, email)
	}
	if want := []string{"four-hours@example.com", "three-hours@example.com", "two-hours@example.com"}; !slices.Equal(order, want) {
		t.Fatalf("claims = %v, want %v: each due row once, the oldest confirmation first", order, want)
	}
	if _, err := database.Pool.Exec(ctx, `update people set confirm_tried_at = now() - interval '59 minutes' where email = 'two-hours@example.com'`); err != nil {
		t.Fatal(err)
	}
	if email, ok, err := people.ClaimOverdueConfirmation(ctx, time.Hour); err != nil || ok {
		t.Fatalf("a claim while the last try is 59 minutes old: %q, %t, %v; want none", email, ok, err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set confirm_tried_at = now() - interval '61 minutes' where email = 'two-hours@example.com'`); err != nil {
		t.Fatal(err)
	}
	if email, ok, err := people.ClaimOverdueConfirmation(ctx, time.Hour); err != nil || !ok || email != "two-hours@example.com" {
		t.Fatalf("a claim once the last try is 61 minutes old: %q, %t, %v; want two-hours again", email, ok, err)
	}
}

// 20261117174706 adds five nullable columns and a check not valid to people: catalog-only alter
// tables, so the census exempts it from the table limit, and it reads no row of people.
func TestCensusExemptsThePeopleHoldMigrationAtAnyTableSize(t *testing.T) {
	entry := censusEntryAtOneByte(t, 20261117160116, 20261117174706, 20261117174706)
	if entry.TableSizeExemption != "alter table" {
		t.Errorf("TableSizeExemption = %q, want %q", entry.TableSizeExemption, "alter table")
	}
	if len(entry.Tables) != 1 || entry.Tables[0].Name != "people" {
		t.Errorf("tables = %+v, want people alone", entry.Tables)
	}
}
