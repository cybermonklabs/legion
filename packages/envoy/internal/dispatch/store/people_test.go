package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// A sign-in and a confirmation are each dated by the database's now(), and Membership answers the
// confirmation's age by the database's clock: a row aged ninety minutes in SQL reads ninety minutes
// old, and the confirmation that follows reads new again.
func TestPgPeopleStoreKeepsEachPersonsMembership(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	email := "person-" + randomDatabaseSuffix(t) + "@d.example"

	if _, found, err := people.Membership(ctx, email); err != nil || found {
		t.Fatalf("membership before any sign-in: found=%t err=%v, want not found", found, err)
	}
	if err := people.SignIn(ctx, email, "refresh-1"); err != nil {
		t.Fatalf("sign in: %v", err)
	}
	// Recording a person who already signed in through the pool keeps their membership.
	if err := people.Record(ctx, email); err != nil {
		t.Fatalf("record: %v", err)
	}
	membership, found, err := people.Membership(ctx, email)
	if err != nil || !found || membership.RefreshToken != "refresh-1" || !confirmedJustNow(membership) {
		t.Fatalf("membership after sign-in = %#v found=%t err=%v, want refresh-1 confirmed just now", membership, found, err)
	}

	if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '90 minutes' where email = $1`, email); err != nil {
		t.Fatalf("date the confirmation ninety minutes back: %v", err)
	}
	membership, _, err = people.Membership(ctx, email)
	if err != nil || !membership.Confirmed || membership.ConfirmedAge < 90*time.Minute || membership.ConfirmedAge >= 91*time.Minute {
		t.Fatalf("membership dated ninety minutes back = %#v err=%v, want confirmed ninety minutes ago", membership, err)
	}
	if err := people.ConfirmSubject(ctx, email, "", "refresh-1", "refresh-2", "", model.MemberFull); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	membership, _, err = people.Membership(ctx, email)
	if err != nil || membership.RefreshToken != "refresh-2" || !confirmedJustNow(membership) {
		t.Fatalf("membership after confirm = %#v err=%v, want refresh-2 confirmed just now", membership, err)
	}

	if err := people.End(ctx, email); err != nil {
		t.Fatalf("end: %v", err)
	}
	membership, found, err = people.Membership(ctx, email)
	if err != nil || !found || membership.RefreshToken != "" || membership.Confirmed {
		t.Fatalf("membership after end = %#v found=%t err=%v, want the person kept with neither", membership, found, err)
	}
}

// TestARecordFailureNamesNoPerson: a sign-in's record that fails names no person in its error,
// since the error reaches the sign-in callback's log line (the rule of
// access.TestNoAccessErrorNamesAPerson).
//
// Control: the email back in the error's text; the test fails naming it.
func TestARecordFailureNamesNoPerson(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	database.Pool.Close()
	const email = "dana@d.example"
	err := people.Record(ctx, email)
	if err == nil {
		t.Fatal("a record on a closed pool answered no error")
	}
	if strings.Contains(err.Error(), email) || strings.Contains(err.Error(), "dana") {
		t.Errorf("the record's error = %q, which names the person", err)
	}
}

// confirmedJustNow reports whether membership holds a confirmation the database dates within the
// last minute: one a write in this test just made.
func confirmedJustNow(membership auth.PersonMembership) bool {
	return membership.Confirmed && membership.ConfirmedAge >= 0 && membership.ConfirmedAge < time.Minute
}

// People are named by lowercase email; the table refuses any other spelling, so no writer can
// make one person two rows.
func TestPeopleTableRefusesAnEmailThatIsNotLowercase(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	for _, email := range []string{"Sami@d.example", ""} {
		if err := people.Record(ctx, email); err == nil {
			t.Errorf("Record(%q) was accepted", email)
		}
	}
}

// storedRefreshToken is people.refresh_token for email as the table holds it, as a backup copies
// it; nil when the row holds none.
func storedRefreshToken(t *testing.T, database *Store, email string) *string {
	t.Helper()
	var stored *string
	if err := database.Pool.QueryRow(context.Background(), `select refresh_token from people where email = $1`, email).Scan(&stored); err != nil {
		t.Fatalf("read the stored refresh token of %s: %v", email, err)
	}
	return stored
}

// captureLogs sends slog's default logger to a buffer until t ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))
	return &logs
}

// revokingPool is a sign-in pool on this machine with Dispatch's app client and the revocation
// endpoint its discovery names, and the code flow a people store revokes through, as main hands it
// the one it builds.
func revokingPool(t *testing.T) (*oidctest.Issuer, *oidc.CodeFlow) {
	t.Helper()
	pool := oidctest.New(t)
	pool.EnableCodeFlow(pool.PublishKey(t, "pool-key"), "dispatch-client", "client-secret")
	return pool, oidc.NewCodeFlow(pool.URL(), "dispatch-client", "client-secret", "")
}

// storePlainRefreshToken stores token for email in plain text with the statement the release
// before sealing signs a person in with, as a task of that release still does during a roll or
// after a rollback.
func storePlainRefreshToken(t *testing.T, database *Store, email, token string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into people (email, refresh_token, confirmed_at) values ($1, $2, $3)
		on conflict (email) do update
		set signed_in_at = now(), refresh_token = excluded.refresh_token, confirmed_at = excluded.confirmed_at
	`, email, token, time.Now()); err != nil {
		t.Fatalf("store the refresh token of %s in plain text: %v", email, err)
	}
}

// migratedEmptyStore is a database of the test's own at the current schema, so a sweep of people
// meets only the rows the test writes.
func migratedEmptyStore(t *testing.T) *Store {
	t.Helper()
	database := openEmptyTestStore(t)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return database
}

// retireLines counts the lines logging a refresh token the pool did not revoke.
func retireLines(logs *bytes.Buffer) int {
	return strings.Count(logs.String(), "the sign-in pool did not revoke a refresh token stored in plain text")
}

// The row a backup copies never holds the refresh token: a sign-in and a confirmation each store it
// sealed under the signing key, in the versioned format, and only a store under that key opens it.
func TestPgPeopleStoreSealsTheRefreshTokenAtRest(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	email := "sealed-" + randomDatabaseSuffix(t) + "@d.example"
	for _, write := range []struct {
		name  string
		write func(token string) error
	}{
		{"sign-in", func(token string) error { return people.SignIn(ctx, email, token) }},
		{"confirmation", func(token string) error {
			read, _, err := people.Membership(ctx, email)
			if err != nil {
				return err
			}
			return people.ConfirmSubject(ctx, email, read.Subject, read.RefreshToken, token, "", model.MemberFull)
		}},
	} {
		token := "refresh-" + write.name + "-" + randomDatabaseSuffix(t)
		if err := write.write(token); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		stored := storedRefreshToken(t, database, email)
		if stored == nil {
			t.Fatalf("after the %s the row holds no refresh token", write.name)
		}
		if strings.Contains(*stored, token) || !strings.HasPrefix(*stored, "v1:") {
			t.Fatalf("after the %s the row holds %q, want the token sealed in the v1: format", write.name, *stored)
		}
		membership, found, err := people.Membership(ctx, email)
		if err != nil || !found || membership.RefreshToken != token || !confirmedJustNow(membership) {
			t.Fatalf("membership after the %s = %#v found=%t err=%v, want %s confirmed just now", write.name, membership, found, err, token)
		}
	}
}

// A refresh token that does not open is no refresh token: one sealed under another signing key (the
// key rotated), one sealed for another person (a value moved between rows), and one stored in plain
// text. Each read answers none, so the person signs in again, and logs that, naming the person by
// their login's hash (model.LoginHash), never by their address, and never the value; the read
// writes nothing and asks the pool nothing. The person's next sign-in replaces the value, revoking
// one in plain text first; a sealed value is never sent to the pool. Control: log "email", email on
// that line again, and each row fails on the address.
func TestPgPeopleStoreReadsARefreshTokenThatDoesNotOpenAsNone(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for name, unopened := range map[string]struct {
		store   func(t *testing.T, people *PgPeopleStore, email, token string)
		revoked bool
	}{
		"sealed under another signing key": {store: func(t *testing.T, _ *PgPeopleStore, email, token string) {
			if err := NewPgPeopleStore(database.Pool, auth.SingleSigningKey("rotated-signing-key"), nil).SignIn(ctx, email, token); err != nil {
				t.Fatal(err)
			}
		}},
		"sealed for another person": {store: func(t *testing.T, people *PgPeopleStore, email, token string) {
			other := "other-" + email
			if err := people.SignIn(ctx, other, token); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Pool.Exec(ctx, `insert into people (email, refresh_token, confirmed_at) select $1, refresh_token, confirmed_at from people where email = $2`, email, other); err != nil {
				t.Fatal(err)
			}
		}},
		"stored in plain text": {store: func(t *testing.T, _ *PgPeopleStore, email, token string) {
			storePlainRefreshToken(t, database, email, token)
		}, revoked: true},
	} {
		t.Run(name, func(t *testing.T) {
			pool, flow := revokingPool(t)
			people := NewPgPeopleStore(database.Pool, testSigningKey, flow)
			email := "unopened-" + randomDatabaseSuffix(t) + "@d.example"
			token := "refresh-" + randomDatabaseSuffix(t)
			unopened.store(t, people, email, token)
			stored := storedRefreshToken(t, database, email)
			if stored == nil {
				t.Fatal("the row holds no refresh token to read")
			}
			logs := captureLogs(t)
			for read := range 2 {
				membership, found, err := people.Membership(ctx, email)
				if err != nil || !found || membership.RefreshToken != "" || membership.Confirmed {
					t.Fatalf("read %d: membership = %#v found=%t err=%v, want the person with no refresh token or confirmation", read, membership, found, err)
				}
			}
			if kept := storedRefreshToken(t, database, email); kept == nil || *kept != *stored {
				t.Errorf("the row holds %v after the reads, want the value the reads found, unchanged", kept)
			}
			if got := pool.RevocationRequests(); len(got) != 0 {
				t.Errorf("the reads sent %q to the sign-in pool, want nothing", got)
			}
			out := logs.String()
			if count := strings.Count(out, "a stored refresh token did not open"); count != 2 || strings.Count(out, "login_hash="+model.LoginHash(email)) != 2 {
				t.Errorf("logged %d lines naming the unopened token, want one per read naming login_hash=%s:\n%s", count, model.LoginHash(email), out)
			}
			if strings.Contains(strings.ToLower(out), email) {
				t.Errorf("the log holds the address %s:\n%s", email, out)
			}
			if strings.Contains(out, token) || strings.Contains(out, *stored) {
				t.Errorf("the log carries the stored value:\n%s", out)
			}
			if err := people.SignIn(ctx, email, "refresh-again"); err != nil {
				t.Fatal(err)
			}
			if membership, _, err := people.Membership(ctx, email); err != nil || membership.RefreshToken != "refresh-again" {
				t.Errorf("membership after signing in again = %#v err=%v, want refresh-again", membership, err)
			}
			var want []string
			if unopened.revoked {
				want = []string{token}
			}
			if got := pool.RevocationRequests(); !reflect.DeepEqual(got, want) {
				t.Errorf("signing in again sent %q to the sign-in pool, want %q", got, want)
			}
		})
	}
}

// A boot retires every refresh token people holds in plain text: it revokes each at the sign-in
// pool, then forgets it with its confirmation, keeping the person. A sealed token is never sent to
// the pool and stays, as does a person holding none, and a second boot finds nothing to do.
func TestRetirePlainRefreshTokensRevokesEachThenForgetsIt(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	pool, flow := revokingPool(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, flow)
	storePlainRefreshToken(t, database, "plain-1@d.example", "plain-token-1")
	storePlainRefreshToken(t, database, "plain-2@d.example", "plain-token-2")
	if err := people.SignIn(ctx, "sealed@d.example", "sealed-token"); err != nil {
		t.Fatal(err)
	}
	sealed := storedRefreshToken(t, database, "sealed@d.example")
	if err := people.Record(ctx, "recorded@d.example"); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	retired, failed, err := people.RetirePlainRefreshTokens(ctx)
	if err != nil || retired != 2 || failed != 0 {
		t.Fatalf("RetirePlainRefreshTokens = %d retired, %d failed, %v; want 2, 0", retired, failed, err)
	}
	if got, want := pool.RevocationRequests(), []string{"plain-token-1", "plain-token-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the sign-in pool was sent %q to revoke, want %q: each plain token once, nothing sealed", got, want)
	}
	for _, email := range []string{"plain-1@d.example", "plain-2@d.example", "recorded@d.example"} {
		membership, found, err := people.Membership(ctx, email)
		if err != nil || !found || membership.RefreshToken != "" || membership.Confirmed {
			t.Errorf("%s after the boot = %#v found=%t err=%v, want the person kept with no refresh token or confirmation", email, membership, found, err)
		}
	}
	if kept := storedRefreshToken(t, database, "sealed@d.example"); kept == nil || *kept != *sealed {
		t.Errorf("the sealed row holds %v after the boot, want its sealed value unchanged", kept)
	}
	if membership, _, err := people.Membership(ctx, "sealed@d.example"); err != nil || membership.RefreshToken != "sealed-token" {
		t.Errorf("the sealed sign-in after the boot = %#v err=%v, want sealed-token", membership, err)
	}
	if strings.Contains(logs.String(), "plain-token") {
		t.Errorf("the log carries a token:\n%s", logs.String())
	}

	if retired, failed, err := people.RetirePlainRefreshTokens(ctx); err != nil || retired != 0 || failed != 0 {
		t.Errorf("a second boot = %d retired, %d failed, %v; want nothing to do", retired, failed, err)
	}
	if got := pool.RevocationRequests(); len(got) != 2 {
		t.Errorf("after a second boot the sign-in pool was sent %q, want nothing more", got)
	}
}

// A token the sign-in pool does not revoke stays in its row, for the next boot to try again: the
// boot logs one ERROR naming the person by their login's hash and the pool's HTTP status, never the
// value or the address, and counts it. The next boot the pool answers retires it.
func TestRetirePlainRefreshTokensLeavesATokenThePoolDidNotRevoke(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	pool, flow := revokingPool(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, flow)
	email := "plain@d.example"
	storePlainRefreshToken(t, database, email, "plain-token")
	pool.FailRevocation(http.StatusServiceUnavailable, "")

	logs := captureLogs(t)
	if retired, failed, err := people.RetirePlainRefreshTokens(ctx); err != nil || retired != 0 || failed != 1 {
		t.Fatalf("a boot the pool refuses = %d retired, %d failed, %v; want 0, 1", retired, failed, err)
	}
	if kept := storedRefreshToken(t, database, email); kept == nil || *kept != "plain-token" {
		t.Fatalf("the row holds %v after a revocation the pool refused, want the token kept for the next attempt", kept)
	}
	out := logs.String()
	if retireLines(logs) != 1 || !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "login_hash="+model.LoginHash(email)) || !strings.Contains(out, "HTTP 503") {
		t.Errorf("logged, want one ERROR naming login_hash=%s and HTTP 503:\n%s", model.LoginHash(email), out)
	}
	if strings.Contains(out, email) {
		t.Errorf("the log carries the address %s:\n%s", email, out)
	}
	if strings.Contains(out, "plain-token") {
		t.Errorf("the log carries the token:\n%s", out)
	}

	pool.FailRevocation(0, "")
	if retired, failed, err := people.RetirePlainRefreshTokens(ctx); err != nil || retired != 1 || failed != 0 {
		t.Fatalf("the next boot = %d retired, %d failed, %v; want 1, 0", retired, failed, err)
	}
	if kept := storedRefreshToken(t, database, email); kept != nil {
		t.Errorf("the row holds %q after the pool revoked it, want it forgotten", *kept)
	}
}

// The retirement runs at every boot, not once: a token a task of the earlier release stores in
// plain text after this release booted (during the roll, or after a rollback) is retired at the
// next boot.
func TestRetirePlainRefreshTokensRetiresATokenStoredAfterTheLastBoot(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	pool, flow := revokingPool(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, flow)
	boot := func() (retired, failed int) {
		t.Helper()
		if err := database.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		retired, failed, err := people.RetirePlainRefreshTokens(ctx)
		if err != nil {
			t.Fatalf("RetirePlainRefreshTokens: %v", err)
		}
		return retired, failed
	}
	if retired, failed := boot(); retired != 0 || failed != 0 {
		t.Fatalf("the first boot = %d retired, %d failed, want nothing to do", retired, failed)
	}
	email := "rolled-back@d.example"
	storePlainRefreshToken(t, database, email, "stored-after-the-boot")
	if retired, failed := boot(); retired != 1 || failed != 0 {
		t.Errorf("the next boot = %d retired, %d failed, want 1, 0", retired, failed)
	}
	if kept := storedRefreshToken(t, database, email); kept != nil {
		t.Errorf("after the next boot the row holds %q, want it forgotten", *kept)
	}
	if got := pool.RevocationRequests(); !reflect.DeepEqual(got, []string{"stored-after-the-boot"}) {
		t.Errorf("the sign-in pool was sent %q, want the token stored after the first boot", got)
	}
}

// A sign-in replaces the refresh token a row holds; one in plain text is retired first, so the
// replacement never forgets a token the pool still accepts without revoking it. When the pool does
// not revoke it, the sign-in proceeds (the person just signed in at the pool) and logs a line of its
// own, apart from the boot's, naming the person to sign out at the pool by their login's hash and
// the status, never the value or the address: no later boot will see that token.
func TestPgPeopleStoreSignInRetiresThePlainTokenItReplaces(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, status := range []int{0, http.StatusBadRequest} {
		pool, flow := revokingPool(t)
		people := NewPgPeopleStore(database.Pool, testSigningKey, flow)
		email := "replaced-" + randomDatabaseSuffix(t) + "@d.example"
		plain := "plain-" + randomDatabaseSuffix(t)
		storePlainRefreshToken(t, database, email, plain)
		pool.FailRevocation(status, "invalid_request")
		logs := captureLogs(t)
		if err := people.SignIn(ctx, email, "refresh-fresh"); err != nil {
			t.Fatalf("pool answering %d: sign in: %v", status, err)
		}
		if got := pool.RevocationRequests(); !reflect.DeepEqual(got, []string{plain}) {
			t.Errorf("pool answering %d: the sign-in sent %q to revoke, want the plain token it replaces", status, got)
		}
		if membership, _, err := people.Membership(ctx, email); err != nil || membership.RefreshToken != "refresh-fresh" {
			t.Errorf("pool answering %d: membership = %#v err=%v, want the new sign-in's refresh-fresh", status, membership, err)
		}
		out := logs.String()
		replaced := strings.Count(out, "a sign-in replaced a refresh token the pool did not revoke; sign this person out at the pool")
		if status == 0 && (replaced != 0 || retireLines(logs) != 0) {
			t.Errorf("pool answering 0: logged, want nothing:\n%s", out)
		}
		if status != 0 && (replaced != 1 || retireLines(logs) != 0 || !strings.Contains(out, "login_hash="+model.LoginHash(email)) || !strings.Contains(out, "HTTP 400")) {
			t.Errorf("pool answering %d: logged, want one sign-in line naming login_hash=%s and HTTP 400, and no boot line:\n%s", status, model.LoginHash(email), out)
		}
		if strings.Contains(out, email) {
			t.Errorf("pool answering %d: the log carries the address %s:\n%s", status, email, out)
		}
		if strings.Contains(out, plain) {
			t.Errorf("pool answering %d: the log carries the token:\n%s", status, out)
		}
	}
}

// retire forgets the token only while the row still holds the value the sweep read: a sign-in
// recorded since keeps its own refresh token.
func TestRetireKeepsASignInRecordedSinceTheRead(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	_, flow := revokingPool(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, flow)
	email := "raced@d.example"
	if err := people.SignIn(ctx, email, "refresh-new"); err != nil {
		t.Fatal(err)
	}
	if err := people.retire(ctx, email, "plain-the-sweep-read"); err != nil {
		t.Fatal(err)
	}
	if membership, _, err := people.Membership(ctx, email); err != nil || membership.RefreshToken != "refresh-new" {
		t.Errorf("membership = %#v err=%v, want the later sign-in's refresh-new", membership, err)
	}
}

// sessionGeneration is the session generation of login, and whether user_sessions holds a row for
// it.
func sessionGeneration(t *testing.T, sessions *PgSessionStore, login string) (int64, bool) {
	t.Helper()
	generation, found, err := sessions.CurrentSessionGeneration(context.Background(), login)
	if err != nil {
		t.Fatalf("read the session generation of %s: %v", login, err)
	}
	return generation, found
}

// In domain mode the row holds the Google account's subject. A sign-in under the subject the row
// holds keeps the address's sessions; one under another subject (a deleted account's address
// given to a new account) ends every session of the address and records the new subject, logging
// the address. The database ends them: recording an account, a fresh row's first included, moves
// the session generation (migration 20261118041916). A sign-in under a group neither reads nor
// writes the subject, and ending a sign-in keeps it, so the next account's sign-in is still
// compared with it.
func TestPgPeopleStoreEndsTheSessionsOfAnAddressAnotherGoogleAccountSignsInTo(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := NewPgSessionStore(database.Pool)
	const email = "a.person@example.com"
	membership := func() auth.PersonMembership {
		t.Helper()
		membership, found, err := people.Membership(ctx, email)
		if err != nil || !found {
			t.Fatalf("membership of %s: found=%t err=%v", email, found, err)
		}
		return membership
	}
	logs := captureLogs(t)

	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberFull); err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if got := membership(); got.Subject != "subject-1" || got.RefreshToken != "refresh-1" {
		t.Fatalf("membership after the first sign-in = %#v, want subject-1 with refresh-1", got)
	}
	if generation, found := sessionGeneration(t, sessions, email); !found || generation != 1 {
		t.Fatalf("session generation %d (row %t) after a first sign-in under a subject, want 1: recording the address's first account starts its sessions there", generation, found)
	}
	generation, err := sessions.EnsureSession(ctx, email)
	if err != nil {
		t.Fatal(err)
	}

	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-2", "", model.MemberFull); err != nil {
		t.Fatalf("sign-in under the same subject: %v", err)
	}
	if got, _ := sessionGeneration(t, sessions, email); got != generation {
		t.Fatalf("a sign-in under the subject the row holds moved the generation from %d to %d", generation, got)
	}
	if strings.Contains(logs.String(), "another Google account") {
		t.Fatalf("a sign-in under the same subject logged a revoke:\n%s", logs.String())
	}

	if err := people.SignInSubject(ctx, email, "subject-2", "refresh-3", "", model.MemberFull); err != nil {
		t.Fatalf("sign-in under another subject: %v", err)
	}
	if got, _ := sessionGeneration(t, sessions, email); got != generation+1 {
		t.Fatalf("a sign-in under another subject left the generation at %d, want %d: every earlier cookie ended", got, generation+1)
	}
	if got := membership(); got.Subject != "subject-2" || got.RefreshToken != "refresh-3" {
		t.Fatalf("membership after another subject's sign-in = %#v, want subject-2 with refresh-3", got)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "another Google account") || !strings.Contains(out, "login_hash="+model.LoginHash(email)) || strings.Contains(out, email) {
		t.Errorf("the revoke was not logged at WARN with the address's login hash and no address:\n%s", out)
	}

	if err := people.SignIn(ctx, email, "refresh-4"); err != nil {
		t.Fatalf("sign-in under a group: %v", err)
	}
	if got := membership(); got.Subject != "subject-2" || got.RefreshToken != "refresh-4" {
		t.Fatalf("membership after a group sign-in = %#v, want subject-2 kept with refresh-4", got)
	}
	if got, _ := sessionGeneration(t, sessions, email); got != generation+1 {
		t.Fatalf("a group sign-in moved the generation to %d", got)
	}
	if err := people.End(ctx, email); err != nil {
		t.Fatal(err)
	}
	if got := membership(); got.Subject != "subject-2" || got.RefreshToken != "" {
		t.Fatalf("membership after End = %#v, want subject-2 kept with no refresh token", got)
	}

	// A person recorded with no subject (header identity, a group sign-in, before the column) may
	// hold a cookie from before a switch to domain mode: their first domain sign-in treats the
	// unrecorded subject as another account and ends any session the row already had.
	if err := people.Record(ctx, "recorded@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := people.SignInSubject(ctx, "recorded@example.com", "subject-3", "refresh-5", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if generation, found := sessionGeneration(t, sessions, "recorded@example.com"); !found || generation != 1 {
		t.Errorf("session generation %d (row %t) after a recorded person's first domain sign-in, want 1: an unrecorded subject ends any session the row already had", generation, found)
	}
}

// The revoke and the new subject commit together: a revoke that fails leaves the row's subject and
// refresh token as they were, so the next sign-in under the new subject revokes again.
func TestPgPeopleStoreRecordsNoSubjectWhoseRevokeFailed(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := NewPgSessionStore(database.Pool)
	const email = "a.person@example.com"
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	generation, err := sessions.EnsureSession(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		create function refuse_session_revoke() returns trigger language plpgsql as $$ begin raise exception 'the test refuses every revoke'; end $$;
		create trigger refuse_session_revoke before insert or update on user_sessions for each row execute function refuse_session_revoke();
	`); err != nil {
		t.Fatal(err)
	}
	if err := people.SignInSubject(ctx, email, "subject-2", "refresh-2", "", model.MemberFull); err == nil || !strings.Contains(err.Error(), "the test refuses every revoke") {
		t.Fatalf("a sign-in whose revoke fails: err = %v, want the revoke's error", err)
	}
	if membership, _, err := people.Membership(ctx, email); err != nil || membership.Subject != "subject-1" || membership.RefreshToken != "refresh-1" {
		t.Fatalf("membership after the failed revoke = %#v err=%v, want subject-1 and refresh-1 as they were", membership, err)
	}
	if _, err := database.Pool.Exec(ctx, `drop trigger refuse_session_revoke on user_sessions`); err != nil {
		t.Fatal(err)
	}
	if err := people.SignInSubject(ctx, email, "subject-2", "refresh-2", "", model.MemberFull); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got, _ := sessionGeneration(t, sessions, email); got != generation+1 {
		t.Fatalf("the retry left the generation at %d, want %d", got, generation+1)
	}
}

// Two first sign-ins to one address at once, each under its own Google account: the second waits
// for the first to commit and then ends the first's sessions, as it would had the first committed
// before it began. The first is held mid-transaction, its row written and not committed.
func TestPgPeopleStoreEndsTheSessionsOfAConcurrentFirstSignIn(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := NewPgSessionStore(database.Pool)
	const email = "a.person@example.com"
	first, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	if _, err := first.Exec(ctx, `insert into people (email, refresh_token, confirmed_at, sub) values ($1, 'v1:first', now(), 'subject-1')`, email); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() { second <- people.SignInSubject(ctx, email, "subject-2", "refresh-2", "", model.MemberFull) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := database.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity where datname = current_database() and cardinality(pg_blocking_pids(pid)) > 0`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-second:
			t.Fatalf("the second sign-in finished while the first was uncommitted: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the second sign-in never waited on the first")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatalf("the second sign-in: %v", err)
	}
	if generation, found := sessionGeneration(t, sessions, email); !found || generation != 2 {
		t.Fatalf("session generation %d (row %t) after a second account's concurrent first sign-in, want 2: the first account's recording started the address at 1, and the second's ended the first's sessions", generation, found)
	}
	if membership, _, err := people.Membership(ctx, email); err != nil || membership.Subject != "subject-2" || membership.RefreshToken != "refresh-2" {
		t.Fatalf("membership = %#v err=%v, want subject-2 with refresh-2", membership, err)
	}
}

// A confirmation under a Google account writes only while the row still records that account: a
// refresh that began before another account signed in to the address cannot overwrite the new
// holder's refresh token. A sign-in pool's confirmation, under the empty subject a pool's row
// records, writes nothing to a row that records an account.
func TestPgPeopleStoreConfirmKeepsTheRowOfAnotherGoogleAccount(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	const email = "a.person@example.com"
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if err := people.SignInSubject(ctx, email, "subject-2", "refresh-2", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '90 minutes' where email = $1`, email); err != nil {
		t.Fatal(err)
	}
	membership := func() auth.PersonMembership {
		t.Helper()
		membership, _, err := people.Membership(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		return membership
	}
	if err := people.ConfirmSubject(ctx, email, "subject-1", "refresh-1", "refresh-stale", "", model.MemberFull); err != nil {
		t.Fatalf("a stale confirmation: %v", err)
	}
	if got := membership(); got.RefreshToken != "refresh-2" || got.Subject != "subject-2" || got.ConfirmedAge < 90*time.Minute {
		t.Fatalf("after subject-1's stale confirmation: %#v, want subject-2's refresh-2 confirmed ninety minutes ago", got)
	}
	if err := people.ConfirmSubject(ctx, email, "subject-2", "refresh-2", "refresh-3", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if got := membership(); got.RefreshToken != "refresh-3" || !confirmedJustNow(got) {
		t.Fatalf("after subject-2's confirmation: %#v, want refresh-3 confirmed just now", got)
	}
	if err := people.ConfirmSubject(ctx, email, "", "refresh-3", "refresh-4", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if got := membership(); got.RefreshToken != "refresh-3" {
		t.Fatalf("after a confirmation under no subject, of a row recording subject-2: %#v, want refresh-3 kept", got)
	}
}

// A confirmation writes only while the row still holds the refresh token the refresh read, not
// only the same account: a refresh that read refresh-1, and answered after the person signed in
// again under the same account, which stored refresh-2 and the member kind it named, leaves that
// newer sign-in's token, kind and confirmation as they are. A confirmation naming the token the row
// holds writes.
func TestConfirmSubjectWritesOnlyOverTheSignInItRead(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	const email = "a.person@example.com"
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	read, _, err := people.Membership(ctx, email)
	if err != nil || read.RefreshToken != "refresh-1" {
		t.Fatalf("the membership a refresh reads: %#v, %v", read, err)
	}
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-2", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '90 minutes' where email = $1`, email); err != nil {
		t.Fatal(err)
	}
	if err := people.ConfirmSubject(ctx, email, read.Subject, read.RefreshToken, "refresh-stale", "", model.MemberRestricted); err != nil {
		t.Fatalf("a stale confirmation: %v", err)
	}
	got, _, err := people.Membership(ctx, email)
	if err != nil || got.RefreshToken != "refresh-2" || got.Kind != model.MemberFull || got.ConfirmedAge < 90*time.Minute {
		t.Fatalf("after a confirmation that read refresh-1, the same account having signed in again with refresh-2: %#v, %v; want refresh-2, full, confirmed ninety minutes ago", got, err)
	}
	if err := people.ConfirmSubject(ctx, email, "subject-1", "refresh-2", "refresh-3", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := people.Membership(ctx, email); got.RefreshToken != "refresh-3" || !confirmedJustNow(got) {
		t.Fatalf("after a confirmation naming the token the row holds: %#v, want refresh-3 confirmed just now", got)
	}
}

// EndSubject ends a sign-in only while the row records the account it names: under another
// account's subject it writes nothing, and under the row's own it advances the generation and
// forgets the token, keeping the subject. A row with no subject ends only for an empty one.
func TestPgPeopleStoreEndSubjectEndsOnlyTheAccountItNames(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := NewPgSessionStore(database.Pool)
	const email = "a.person@example.com"
	if err := people.SignInSubject(ctx, email, "subject-2", "refresh-2", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	generation, err := sessions.EnsureSession(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if ended, err := people.EndSubject(ctx, email, "subject-1", "refresh-2", auth.EndForget); err != nil || ended {
		t.Fatalf("EndSubject under another account: ended %t, %v; want false", ended, err)
	}
	if membership, _, _ := people.Membership(ctx, email); membership.RefreshToken != "refresh-2" {
		t.Fatalf("after EndSubject under another account: %#v, want refresh-2 kept", membership)
	}
	if got, _ := sessionGeneration(t, sessions, email); got != generation {
		t.Fatalf("EndSubject under another account moved the generation to %d", got)
	}
	if ended, err := people.EndSubject(ctx, email, "subject-2", "refresh-2", auth.EndForget); err != nil || !ended {
		t.Fatalf("EndSubject under the row's account: ended %t, %v; want true", ended, err)
	}
	if membership, _, _ := people.Membership(ctx, email); membership.RefreshToken != "" || membership.Subject != "subject-2" {
		t.Fatalf("after EndSubject: %#v, want no refresh token and subject-2 kept", membership)
	}
	if got, _ := sessionGeneration(t, sessions, email); got != generation+1 {
		t.Fatalf("EndSubject left the generation at %d, want %d", got, generation+1)
	}
	if err := people.Record(ctx, "recorded@example.com"); err != nil {
		t.Fatal(err)
	}
	if ended, err := people.EndSubject(ctx, "recorded@example.com", "subject-1", "", auth.EndForget); err != nil || ended {
		t.Fatalf("EndSubject of a row with no subject under an account: ended %t, %v; want false", ended, err)
	}
	if ended, err := people.EndSubject(ctx, "recorded@example.com", "", "", auth.EndForget); err != nil || !ended {
		t.Fatalf("EndSubject of a row with no subject under none: ended %t, %v; want true", ended, err)
	}
	if ended, err := people.EndSubject(ctx, "nobody@example.com", "", "", auth.EndForget); err != nil || ended {
		t.Fatalf("EndSubject of no row: ended %t, %v; want false", ended, err)
	}
}

// EndSubject ends a sign-in only while the row still holds the refresh token the failed refresh
// read, not only the same account: a refresh that read refresh-1 and was then answered invalid_grant
// after the person signed in again under the same account, which stored refresh-2, ends nothing, and
// the newer sign-in's sessions and token stand. An end that names the token the row holds ends it.
func TestPgPeopleStoreEndSubjectEndsOnlyTheSignInItRead(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := NewPgSessionStore(database.Pool)
	const email = "a.person@example.com"
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	read, _, err := people.Membership(ctx, email)
	if err != nil || read.RefreshToken != "refresh-1" {
		t.Fatalf("the membership a refresh reads: %#v, %v", read, err)
	}
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-2", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	generation, err := sessions.EnsureSession(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if ended, err := people.EndSubject(ctx, email, read.Subject, read.RefreshToken, auth.EndForget); err != nil || ended {
		t.Fatalf("EndSubject naming refresh-1, after a sign-in of the same account stored refresh-2: ended %t, %v; want false, the newer sign-in kept", ended, err)
	}
	if membership, _, _ := people.Membership(ctx, email); membership.RefreshToken != "refresh-2" {
		t.Fatalf("after the stale end: %#v, want refresh-2 kept", membership)
	}
	if got, _ := sessionGeneration(t, sessions, email); got != generation {
		t.Fatalf("the stale end moved the generation to %d, want %d: the newer sign-in's cookies stand", got, generation)
	}
	if ended, err := people.EndSubject(ctx, email, "subject-1", "refresh-2", auth.EndForget); err != nil || !ended {
		t.Fatalf("EndSubject naming the token the row holds: ended %t, %v; want true", ended, err)
	}
}

// storedMemberKind is the member_kind column of email's row.
func storedMemberKind(t *testing.T, database *Store, email string) string {
	t.Helper()
	var kind string
	if err := database.Pool.QueryRow(context.Background(), `select member_kind from people where email = $1`, email).Scan(&kind); err != nil {
		t.Fatalf("read the member kind of %s: %v", email, err)
	}
	return kind
}

// A Google sign-in records the member kind the sign-in rule named: an outside account's sign-in
// writes restricted, and a later sign-in of the same address and account as a full member writes
// full, so the column follows the latest sign-in. A kind that is neither is refused before any
// write.
func TestSignInSubjectRecordsTheMemberKind(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	const email = "outsider@other.example"
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	if got := storedMemberKind(t, database, email); got != "restricted" {
		t.Fatalf("after a restricted sign-in: member_kind = %q, want restricted", got)
	}
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-2", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if got := storedMemberKind(t, database, email); got != "full" {
		t.Fatalf("after a full sign-in: member_kind = %q, want full", got)
	}
	for _, kind := range []model.MemberKind{model.MemberNone, "admin"} {
		if err := people.SignInSubject(ctx, "refused@other.example", "subject-2", "refresh-3", "", kind); err == nil || !strings.Contains(err.Error(), "member kind") {
			t.Errorf("SignInSubject with member kind %q: err = %v, want a refusal naming the member kind", kind, err)
		}
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from people where email = 'refused@other.example'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a refused member kind left %d rows (%v), want none", rows, err)
	}
}

// A confirmation writes the member kind only while the row still records the account the refresh
// read: once another account signed in to the address, the stale confirmation leaves the row's kind
// as that account's sign-in wrote it. That account signs in as a full member, since an account
// outside the Workspace never takes the row of a full member under another account
// (TestSignInSubjectRefusesAnOutsideSignInToAFullMembersAddress).
func TestConfirmSubjectRecordsTheMemberKindOnlyForItsSubject(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	const email = "outsider@other.example"
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	if err := people.ConfirmSubject(ctx, email, "subject-1", "refresh-1", "refresh-2", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if got := storedMemberKind(t, database, email); got != "full" {
		t.Fatalf("after its own subject's confirmation as full: member_kind = %q, want full", got)
	}
	if err := people.SignInSubject(ctx, email, "subject-2", "refresh-3", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if err := people.ConfirmSubject(ctx, email, "subject-1", "refresh-2", "refresh-stale", "", model.MemberRestricted); err != nil {
		t.Fatalf("a stale confirmation: %v", err)
	}
	if got := storedMemberKind(t, database, email); got != "full" {
		t.Fatalf("after subject-1's stale confirmation as restricted: member_kind = %q, want subject-2's full", got)
	}
	if err := people.ConfirmSubject(ctx, email, "subject-2", "refresh-3", "refresh-4", "", "admin"); err == nil || !strings.Contains(err.Error(), "member kind") {
		t.Fatalf("ConfirmSubject with member kind admin: err = %v, want a refusal naming the member kind", err)
	}
}

// A sign-in or a confirmation through the Cognito sign-in pool records a full member: the pool's
// group admits only the organization's people.
func TestPoolSignInRecordsAFullMember(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	const email = "a.person@example.com"
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	if err := people.SignIn(ctx, email, "refresh-2"); err != nil {
		t.Fatal(err)
	}
	if got := storedMemberKind(t, database, email); got != "full" {
		t.Fatalf("after a pool sign-in: member_kind = %q, want full", got)
	}
	if _, err := database.Pool.Exec(ctx, `update people set member_kind = 'restricted' where email = $1`, email); err != nil {
		t.Fatal(err)
	}
	if err := people.ConfirmSubject(ctx, email, "subject-1", "refresh-2", "refresh-3", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if got := storedMemberKind(t, database, email); got != "full" {
		t.Fatalf("after a pool confirmation: member_kind = %q, want full", got)
	}
}

// Each write of people.member_kind - a Google sign-in (SignInSubject), the hourly confirmation
// (ConfirmSubject) and a pool sign-in (SignIn) - announces on AccessChangedChannel, as its
// transaction commits, when the kind it writes is not the one the row held, and announces nothing
// when the kind stays as it was or the write writes nothing (a stale confirmation): every process
// then reads again what the person sees, and closes their document sockets and live views of a
// project a restricted member no longer reads.
func TestSignInSubjectConfirmSubjectAndSignInAnnounceAMemberKindChange(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	listener, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `listen `+AccessChangedChannel); err != nil {
		t.Fatal(err)
	}
	announced := func() bool {
		wait, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		_, err := listener.Conn().WaitForNotification(wait)
		return err == nil
	}
	const email = "a.person@second.example"
	for _, step := range []struct {
		what      string
		write     func() error
		announces bool
	}{
		{"a first sign-in as full", func() error {
			return people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberFull)
		}, false},
		{"the confirmation as full", func() error {
			return people.ConfirmSubject(ctx, email, "subject-1", "refresh-1", "refresh-2", "", model.MemberFull)
		}, false},
		{"the confirmation as restricted", func() error {
			return people.ConfirmSubject(ctx, email, "subject-1", "refresh-2", "refresh-3", "", model.MemberRestricted)
		}, true},
		{"the confirmation as restricted again", func() error {
			return people.ConfirmSubject(ctx, email, "subject-1", "refresh-3", "refresh-4", "", model.MemberRestricted)
		}, false},
		{"a stale confirmation as full, which writes nothing", func() error {
			return people.ConfirmSubject(ctx, email, "subject-1", "refresh-stale", "refresh-5", "", model.MemberFull)
		}, false},
		{"a sign-in of the same account as full", func() error {
			return people.SignInSubject(ctx, email, "subject-1", "refresh-6", "", model.MemberFull)
		}, true},
		{"a sign-in of the same account as restricted", func() error {
			return people.SignInSubject(ctx, email, "subject-1", "refresh-7", "", model.MemberRestricted)
		}, true},
		{"a pool sign-in, which records a full member", func() error {
			return people.SignIn(ctx, email, "refresh-8")
		}, true},
		{"a pool sign-in again", func() error {
			return people.SignIn(ctx, email, "refresh-9")
		}, false},
	} {
		if err := step.write(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if got := announced(); got != step.announces {
			t.Errorf("%s: announced on %s %t, want %t", step.what, AccessChangedChannel, got, step.announces)
		}
	}
	if got := storedMemberKind(t, database, email); got != string(model.MemberFull) {
		t.Fatalf("after the steps: member_kind = %q, want full", got)
	}
}

// TestAMemberMadeFullTakesNoSessionOwnerLock: a write that makes a restricted member full takes no
// project from them, since a full member sees every project a restricted one does, so it reads no
// project (projectsSeenAs) and takes no session-owner lock (RecordReachedProjects, handed none,
// returns at once). The member is listed on CORE, which a restricted member sees, so a write that
// read the projects the stored kind saw would hand CORE to RecordReachedProjects and take the lock.
// With that lock held by another transaction, the hourly confirmation and the sign-in that make
// the member full each commit while it is still held.
//
// Control: projectsSeenAs answering for every change of kind, as it did before this row. Each write
// then queues behind the held lock, and the row names it.
func TestAMemberMadeFullTakesNoSessionOwnerLock(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	const email = "made.full@outside.example"
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice@d.example')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into project_access (project_key, principal_kind, principal, added_by)
		values ('CORE', 'person', $1, '{"kind":"user","id":"alice"}')`, email); err != nil {
		t.Fatalf("list the member on the project: %v", err)
	}
	if err := people.SignInSubject(ctx, email, "subject-1", "refresh-1", "", model.MemberRestricted); err != nil {
		t.Fatalf("the restricted sign-in: %v", err)
	}
	for _, write := range []struct {
		what  string
		write func() error
	}{
		{"the confirmation as full", func() error {
			return people.ConfirmSubject(ctx, email, "subject-1", "refresh-1", "refresh-2", "", model.MemberFull)
		}},
		{"the sign-in as full", func() error {
			return people.SignInSubject(ctx, email, "subject-1", "refresh-3", "", model.MemberFull)
		}},
	} {
		if _, err := database.Pool.Exec(ctx, `update people set member_kind = 'restricted' where email = $1`, email); err != nil {
			t.Fatalf("%s: make the member restricted again: %v", write.what, err)
		}
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1), hashtext($2))`, SessionOwnerLock, email); err != nil {
			t.Fatalf("%s: hold the member's session-owner lock: %v", write.what, err)
		}
		done := make(chan error, 1)
		go func() { done <- write.write() }()
		committed := awaitCommitOrQueue(t, holder, done)
		if err := holder.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if !committed {
			t.Errorf("%s queued behind the member's session-owner lock; want it to take none, since it takes no project from them", write.what)
			if err := <-done; err != nil {
				t.Fatalf("%s, once the lock was let go: %v", write.what, err)
			}
		}
		if got := storedMemberKind(t, database, email); got != string(model.MemberFull) {
			t.Fatalf("%s: member_kind = %q, want full", write.what, got)
		}
	}
}

// awaitCommitOrQueue answers true once the write answers on done with no error, and false as soon
// as any backend queues behind a lock holder's transaction owns, whichever comes first, reading
// pg_locks through holder's own connection. Only a hang guard of ten minutes fails a write that
// does neither.
func awaitCommitOrQueue(t *testing.T, holder pgx.Tx, done <-chan error) bool {
	t.Helper()
	guard := time.Now().Add(10 * time.Minute)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the write: %v", err)
			}
			return true
		default:
		}
		var queued bool
		if err := holder.QueryRow(context.Background(),
			`select exists (select 1 from pg_locks where not granted and pg_backend_pid() = any(pg_blocking_pids(pid)))`).Scan(&queued); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if queued {
			return false
		}
		if time.Now().After(guard) {
			t.Fatal("the write neither answered nor queued behind the held lock within ten minutes")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the write: %v", err)
			}
			return true
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// rowOf is email's people row as JSON, every column, so a sign-in that writes anything changes it.
func rowOf(t *testing.T, database *Store, email string) string {
	t.Helper()
	var row string
	if err := database.Pool.QueryRow(context.Background(), `select row_to_json(p)::text from people p where email = $1`, email).Scan(&row); err != nil {
		t.Fatalf("read the row of %s: %v", email, err)
	}
	return row
}

// An outside account's sign-in (restricted) never takes an address whose row records a full member
// under another Google account, or under none (a Cognito-era, header- or dev-recorded row): it is
// auth.ErrAddressHeldByMember, naming the address, and writes nothing, so the row (a refresh token
// it holds in plain text included, which no pool is asked to revoke) and the session generation
// stay as they were and the member's sessions stand.
func TestSignInSubjectRefusesAnOutsideSignInToAFullMembersAddress(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	pool, flow := revokingPool(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, flow)
	sessions := NewPgSessionStore(database.Pool)
	logs := captureLogs(t)
	for _, held := range []struct {
		name, email string
		seed        func(email string) error
	}{
		{"a full member under another account", "alice@second.example", func(email string) error {
			return people.SignInSubject(ctx, email, "member", "refresh-member", "", model.MemberFull)
		}},
		{"a full member recorded under no account", "recorded@second.example", func(email string) error {
			return people.Record(ctx, email)
		}},
		{"a full member holding a plain refresh token", "plain@second.example", func(email string) error {
			if err := people.SignInSubject(ctx, email, "member", "refresh-member", "", model.MemberFull); err != nil {
				return err
			}
			_, err := database.Pool.Exec(ctx, `update people set refresh_token = $2 where email = $1`, email, "plain-member")
			return err
		}},
	} {
		t.Run(held.name, func(t *testing.T) {
			if err := held.seed(held.email); err != nil {
				t.Fatalf("seed %s: %v", held.email, err)
			}
			generation, err := sessions.EnsureSession(ctx, held.email)
			if err != nil {
				t.Fatal(err)
			}
			before := rowOf(t, database, held.email)
			err = people.SignInSubject(ctx, held.email, "outsider", "refresh-outsider", "", model.MemberRestricted)
			if !errors.Is(err, auth.ErrAddressHeldByMember) || !strings.Contains(err.Error(), held.email) {
				t.Fatalf("an outside sign-in to the address of %s: err = %v, want auth.ErrAddressHeldByMember naming %s", held.name, err, held.email)
			}
			if after := rowOf(t, database, held.email); after != before {
				t.Errorf("the refused sign-in wrote the row of %s:\nbefore %s\nafter  %s", held.name, before, after)
			}
			if got, _ := sessionGeneration(t, sessions, held.email); got != generation {
				t.Errorf("the refused sign-in moved the session generation of %s from %d to %d", held.name, generation, got)
			}
		})
	}
	if got := pool.RevocationRequests(); len(got) != 0 {
		t.Errorf("the refused sign-ins sent %q to the pool to revoke, want nothing", got)
	}
	if strings.Contains(logs.String(), "another Google account") {
		t.Errorf("a refused sign-in logged that it ended the address's sessions:\n%s", logs.String())
	}
}

// The refusal holds under the row's lock, not only at the read before it: an outside sign-in that
// begins while a Workspace account's first sign-in to the address is uncommitted finds no row
// there, waits on that sign-in's insert, and once it commits is refused and writes nothing, so the
// member's row is as the member's sign-in wrote it.
func TestSignInSubjectRefusesAnOutsideSignInRacingAMembersFirstSignIn(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	const email = "alice@second.example"
	member, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer member.Rollback(ctx)
	if _, err := member.Exec(ctx, `insert into people (email, refresh_token, confirmed_at, sub, member_kind) values ($1, 'v1:member', now(), 'member', 'full')`, email); err != nil {
		t.Fatal(err)
	}
	outsider := make(chan error, 1)
	go func() {
		outsider <- people.SignInSubject(ctx, email, "outsider", "refresh-outsider", "", model.MemberRestricted)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := database.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity where datname = current_database() and cardinality(pg_blocking_pids(pid)) > 0`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-outsider:
			t.Fatalf("the outside sign-in finished while the member's was uncommitted: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the outside sign-in never waited on the member's")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := member.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-outsider; !errors.Is(err, auth.ErrAddressHeldByMember) {
		t.Fatalf("the outside sign-in that waited on the member's first sign-in: err = %v, want auth.ErrAddressHeldByMember", err)
	}
	var sub, kind, token string
	if err := database.Pool.QueryRow(ctx, `select coalesce(sub, ''), member_kind, coalesce(refresh_token, '') from people where email = $1`, email).Scan(&sub, &kind, &token); err != nil {
		t.Fatal(err)
	}
	if sub != "member" || kind != "full" || token != "v1:member" {
		t.Errorf("the member's row after the refused sign-in: sub %q, member_kind %q, refresh_token %q; want member, full and v1:member as the member's sign-in wrote them", sub, kind, token)
	}
}

// Every other Google sign-in is what it was: an outside account's sign-in to the address its own
// account holds as a full member, to an address another outside account holds, and to an address
// nobody holds; and a Workspace account's sign-in to an address another Workspace account holds,
// which ends the address's sessions and takes it (R9).
func TestSignInSubjectTakesEveryAddressNoOtherFullMemberHolds(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := NewPgSessionStore(database.Pool)
	for _, test := range []struct {
		name, email     string
		held            model.MemberKind
		holder, subject string
		kind            model.MemberKind
		endsTheSessions bool
	}{
		{"restricted over the same account's full row", "same@second.example", model.MemberFull, "member", "member", model.MemberRestricted, false},
		{"restricted over another account's restricted row", "taken@other.example", model.MemberRestricted, "outsider-1", "outsider-2", model.MemberRestricted, true},
		// The fresh row's first account moves the generation from none to 1: the database marks
		// every recording of an account so, with no earlier session to end here.
		{"restricted over no row", "new@other.example", "", "", "outsider", model.MemberRestricted, true},
		{"full over another account's full row", "reassigned@example.com", model.MemberFull, "member-1", "member-2", model.MemberFull, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			generation := int64(0)
			if test.held != "" {
				if err := people.SignInSubject(ctx, test.email, test.holder, "refresh-holder", "", test.held); err != nil {
					t.Fatalf("seed %s: %v", test.email, err)
				}
				var err error
				if generation, err = sessions.EnsureSession(ctx, test.email); err != nil {
					t.Fatal(err)
				}
			}
			if err := people.SignInSubject(ctx, test.email, test.subject, "refresh-new", "", test.kind); err != nil {
				t.Fatalf("%s: %v, want the sign-in recorded", test.name, err)
			}
			membership, _, err := people.Membership(ctx, test.email)
			if err != nil || membership.Subject != test.subject || membership.RefreshToken != "refresh-new" || membership.Kind != test.kind {
				t.Fatalf("%s: membership = %#v err=%v, want %s as %s with refresh-new", test.name, membership, err, test.subject, test.kind)
			}
			want := generation
			if test.endsTheSessions {
				want++
			}
			if got, _ := sessionGeneration(t, sessions, test.email); got != want {
				t.Errorf("%s: session generation %d, want %d", test.name, got, want)
			}
		})
	}
}

// storedToken writes a personal token of owner named name, as the mint stores one, revoked at
// revokedAt unless that is nil, and returns its id. Its hash is of owner and name, so no two of a
// test's tokens share one, and no plaintext is ever written.
func storedToken(t *testing.T, database *Store, owner, name string, revokedAt *time.Time) string {
	t.Helper()
	hash := sha256.Sum256([]byte(owner + "\x00" + name))
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into agent_tokens (owner, name, token_hash, prefix, revoked_at) values ($1, $2, $3, $4, $5)
		returning id::text
	`, owner, name, hash[:], "test", revokedAt).Scan(&id); err != nil {
		t.Fatalf("store the token %q of %s: %v", name, owner, err)
	}
	return id
}

// tokenRevokedAt is the revoked_at of token id, nil while the token is live.
func tokenRevokedAt(t *testing.T, database *Store, id string) *time.Time {
	t.Helper()
	var revokedAt *time.Time
	if err := database.Pool.QueryRow(context.Background(), `select revoked_at from agent_tokens where id = $1`, id).Scan(&revokedAt); err != nil {
		t.Fatalf("read the token %s: %v", id, err)
	}
	return revokedAt
}

// A sign-in under another Google account than the one the address's row records revokes every
// personal token of the address, in the transaction that ends its sessions, whatever member kind
// the row held and the sign-in records: the replaced account minted them, and the address is the
// new account's now. So the member who reclaims an address an outside account took before the
// member's first sign-in leaves none of that account's tokens acting as the member, and an address
// Google gave to another account leaves none of the old one's. A token's owner is compared with its
// letters folded, as people.email stores the address; a token of another address, and one revoked
// before, keep the revoked_at they had. The revoke is logged once, at WARN, with the address's
// login hash and how many tokens it revoked, never the address.
//
// A row that recorded no Google account (Record: header identity, a dev sign-in; or a Cognito
// pool's sign-in) replaces no account, but its tokens record none either: they were minted before
// the address's first Google account was recorded, by a credential bound to no account. Its first
// Google sign-in revokes them (revokeOtherAccountsTokensSQL), logged once at WARN as the first
// recording's revoke. They were already refused as the person
// (api.tokenAccountReplaced: created_at before people.sub_recorded_at), but only by a comparison
// of two readings of the database's clock, which a backward step defeats; revoking them needs no
// clock. (This row expected the tokens kept before the same-family review's F3 of
// cybermonklabs/legion#4199.)
func TestSignInSubjectRevokesThePersonalTokensOfTheAccountItReplaces(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := NewPgSessionStore(database.Pool)
	logs := captureLogs(t)
	earlier := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	const (
		replacedLog = `msg="dispatch: a sign-in under another Google account ended every session and revoked every personal token of the address"`
		firstLog    = `msg="dispatch: the first Google account recorded for the address revoked every personal token of it that records another account or none"`
	)
	for _, test := range []struct {
		name, email, other string
		// held is the member kind the row records before the sign-in, or "" for a row recorded
		// with no Google account (Record).
		held            model.MemberKind
		holder, subject string
		kind            model.MemberKind
		// logged is the one WARN line the sign-in's revoke writes.
		logged string
	}{
		{"restricted to full: the member reclaims an address an outside account took", "alice@second.example", "alice@example.com", model.MemberRestricted, "outsider", "member", model.MemberFull, replacedLog},
		{"full to full: Google gave the address to another Workspace account", "reassigned@example.com", "reassigned@second.example", model.MemberFull, "member-1", "member-2", model.MemberFull, replacedLog},
		{"restricted to restricted: another outside account takes the address", "taken@other.example", "taken@example.com", model.MemberRestricted, "outsider-1", "outsider-2", model.MemberRestricted, replacedLog},
		{"no account to full: the first recording revokes the tokens a credential bound to no account minted", "recorded@example.com", "recorded@second.example", "", "", "member", model.MemberFull, firstLog},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.held == "" {
				if err := people.Record(ctx, test.email); err != nil {
					t.Fatal(err)
				}
			} else if err := people.SignInSubject(ctx, test.email, test.holder, "refresh-holder", "", test.held); err != nil {
				t.Fatalf("seed %s: %v", test.email, err)
			}
			generation, err := sessions.EnsureSession(ctx, test.email)
			if err != nil {
				t.Fatal(err)
			}
			addressTokens := map[string]string{
				"an agent": storedToken(t, database, test.email, "an agent", nil),
				"an agent whose owner is written in capitals":                   storedToken(t, database, strings.ToUpper(test.email), "an agent whose owner is written in capitals", nil),
				"an agent whose owner is wrapped in a tab and a no-break space": storedToken(t, database, "\t"+test.email+"\u00a0", "an agent whose owner is wrapped in a tab and a no-break space", nil),
			}
			revokedBefore := storedToken(t, database, test.email, "revoked before", &earlier)
			others := storedToken(t, database, test.other, "another address's agent", nil)
			logs.Reset()

			if err := people.SignInSubject(ctx, test.email, test.subject, "refresh-new", "", test.kind); err != nil {
				t.Fatalf("the sign-in of %s under %s: %v", test.email, test.subject, err)
			}
			if got, _ := sessionGeneration(t, sessions, test.email); got != generation+1 {
				t.Errorf("%s: session generation %d after %s signed in, want %d: every earlier cookie ended", test.email, got, test.subject, generation+1)
			}
			for name, id := range addressTokens {
				if tokenRevokedAt(t, database, id) == nil {
					t.Errorf("%s: the token %q, which an account other than %s or a credential bound to no account minted, is live after %s signed in, want it revoked", test.email, name, test.subject, test.subject)
				}
			}
			if got := tokenRevokedAt(t, database, revokedBefore); got == nil || !got.Equal(earlier) {
				t.Errorf("%s: the token revoked at %s reads revoked at %v after the sign-in, want %s kept", test.email, earlier, got, earlier)
			}
			if got := tokenRevokedAt(t, database, others); got != nil {
				t.Errorf("%s: the token of %s was revoked at %s, want it live: only the address's own tokens end", test.email, test.other, got)
			}
			out := logs.String()
			if strings.Count(out, test.logged) != 1 || !strings.Contains(out, "level=WARN") || !strings.Contains(out, "login_hash="+model.LoginHash(test.email)) || !strings.Contains(out, "tokens_revoked=3") || strings.Contains(out, test.email) {
				t.Errorf("%s: the revoke was not logged once at WARN as %s, with the address's login hash, tokens_revoked=3 and no address:\n%s", test.email, test.logged, out)
			}
		})
	}
}

// A sign-in under the Google account the address's row already records revokes no token, whatever
// member kind it records, and logs no revoke: a member signing in again, an outside account doing
// so, and the member's own account signing in from outside the Workspace.
func TestSignInSubjectUnderTheSameAccountRevokesNoToken(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	logs := captureLogs(t)
	for _, test := range []struct {
		name, email string
		held, kind  model.MemberKind
	}{
		{"a full member again", "member@example.com", model.MemberFull, model.MemberFull},
		{"an outside account again", "outsider@other.example", model.MemberRestricted, model.MemberRestricted},
		{"a full member's own account from outside the Workspace", "same@second.example", model.MemberFull, model.MemberRestricted},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := people.SignInSubject(ctx, test.email, "subject-1", "refresh-1", "", test.held); err != nil {
				t.Fatalf("seed %s: %v", test.email, err)
			}
			id := storedToken(t, database, test.email, "an agent", nil)
			logs.Reset()
			if err := people.SignInSubject(ctx, test.email, "subject-1", "refresh-2", "", test.kind); err != nil {
				t.Fatalf("the sign-in of %s under subject-1 again: %v", test.email, err)
			}
			if got := tokenRevokedAt(t, database, id); got != nil {
				t.Errorf("%s: a sign-in under the account the row records revoked its token at %s, want it live", test.email, got)
			}
			if out := logs.String(); strings.Contains(out, "another Google account") {
				t.Errorf("%s: a sign-in under the account the row records logged a revoke:\n%s", test.email, out)
			}
		})
	}
}
