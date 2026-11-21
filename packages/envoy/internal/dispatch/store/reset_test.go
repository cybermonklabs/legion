package store

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// resetLead is the line of the runbook's reset after a Dispatch database restored or made again
// alone whose heredoc ends every browser session and revokes every personal token on Dispatch's
// database ("After a Dispatch database restored or made again alone"); the heredoc before it reads
// the broker's greatest recorded generation, which runReset is handed.
const resetLead = `psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v broker_greatest=`

// runReset runs that heredoc on database, as psql sends it, with brokerGreatest as the broker's
// greatest recorded generation, and answers the number it raised every session to.
func runReset(t *testing.T, database *Store, brokerGreatest int64) int64 {
	t.Helper()
	ctx := context.Background()
	statements := sqlBlockAfter(t, restoreRunbook, resetLead)
	if len(statements) < 3 || statements[0] != "begin" || statements[len(statements)-1] != "commit" {
		t.Fatalf("the runbook's reset is %q; want one transaction", statements)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	args := pgx.NamedArgs{"broker_greatest": brokerGreatest}
	var raised int64
	for _, statement := range statements[1 : len(statements)-1] {
		statement = strings.ReplaceAll(statement, ":'broker_greatest'", "@broker_greatest")
		if strings.HasPrefix(statement, "select generation from reset_generation") {
			if err := tx.QueryRow(ctx, statement, args).Scan(&raised); err != nil {
				t.Fatalf("the reset's %q: %v", statement, err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, statement, args); err != nil {
			t.Fatalf("the reset's %q: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the reset's commit: %v", err)
	}
	return raised
}

// cookieMatches reports whether the signed cookie Dispatch issued email at generation still
// signs its holder in: it verifies, and email's session row holds that generation. An address with
// no session row signs no cookie in.
func cookieMatches(t *testing.T, database *Store, email string, generation int64) bool {
	t.Helper()
	cookie := auth.IssueSessionCookie(email, generation, testSigningKey, true)
	request := httptest.NewRequest("GET", "https://dispatch.example/", nil)
	request.Header.Set("Cookie", strings.SplitN(cookie, ";", 2)[0])
	session, ok := auth.SessionFromRequest(request, testSigningKey)
	if !ok {
		t.Fatalf("the cookie issued %s at generation %d does not verify", email, generation)
	}
	current, found, err := NewPgSessionStore(database.Pool).CurrentSessionGeneration(context.Background(), email)
	if err != nil {
		t.Fatalf("read %s's generation: %v", email, err)
	}
	return found && current == session.Generation
}

// TestTheResetEndsEveryCookieAndToken runs, from the runbook, the reset's block on Dispatch's
// database after a Dispatch database restored or made again alone. Afterwards no cookie the lost
// history can have issued signs anyone in: one of an address the database holds, one its holder
// signed out of, and one of an address the restored database has never seen, whose first sign-in
// after the reset starts its session row at the reset's number (the deployment's floor,
// 20261119183355) rather than at 1. Every personal token is revoked. The number is above the
// broker's greatest generation, so the broker refuses no approve for it.
//
// A second restore, to before the first reset, takes away both the floor and the raised rows, so
// the cookies the first reset's number signed in would match a raise to that same number again:
// the second reset, run later, raises past it, and the cookie a holder signed in with between the
// two resets signs nobody in.
//
// Controls, in the test: before the reset the kept address's cookie and the token act (the state
// the reset ends). Controls in the runbook, each a break restored by checksum: the raise without
// its time term, under which the second reset raises to the first one's number; and the token
// statement narrowed, under which a token stays live.
func TestTheResetEndsEveryCookieAndToken(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	sessions := NewPgSessionStore(database.Pool)
	const kept, signedOut, unseen = "kept@example.com", "signedout@example.com", "unseen@example.com"
	signInAll(t, people, kept, "account-k")
	signInAll(t, people, signedOut, "account-s")
	keptAt := sessionGenerationOf(t, database.Pool, kept)
	signedOutAt := sessionGenerationOf(t, database.Pool, signedOut)
	if err := sessions.RevokeSessions(ctx, signedOut); err != nil {
		t.Fatal(err)
	}
	tokens := []string{
		mintToken(t, database.Pool, kept, "kept's agent", "account-k"),
		mintToken(t, database.Pool, signedOut, "signed-out's agent", "account-s"),
		mintToken(t, database.Pool, kept, "kept's agent recording no account", ""),
	}
	if !cookieMatches(t, database, kept, keptAt) || tokenRevokedAt(t, database, tokens[0]) != nil {
		t.Fatalf("control: before the reset %s's cookie signs in %t and its token is live %t; want both", kept,
			cookieMatches(t, database, kept, keptAt), tokenRevokedAt(t, database, tokens[0]) == nil)
	}

	const brokerGreatest = 7
	raised := runReset(t, database, brokerGreatest)
	if raised <= brokerGreatest || raised < time.Now().Add(-time.Hour).UnixMicro() {
		t.Errorf("the reset raised to %d; want above the broker's %d and at least the microseconds of an hour ago", raised, brokerGreatest)
	}
	for _, email := range []string{kept, signedOut} {
		if got := sessionGenerationOf(t, database.Pool, email); got != raised {
			t.Errorf("after the reset %s's session generation is %d, want %d", email, got, raised)
		}
	}
	for _, cookie := range []struct {
		email      string
		generation int64
	}{{kept, keptAt}, {signedOut, signedOutAt}, {signedOut, signedOutAt + 1}, {unseen, 1}} {
		if cookieMatches(t, database, cookie.email, cookie.generation) {
			t.Errorf("after the reset, the cookie %s held at generation %d signs in; want every cookie of the lost history refused", cookie.email, cookie.generation)
		}
	}
	for _, id := range tokens {
		if tokenRevokedAt(t, database, id) == nil {
			t.Errorf("after the reset, the personal token %s is live; want every personal token revoked", id)
		}
	}
	// The address the restored database has never seen: its first sign-in after the reset starts
	// its session row at the deployment's floor, above the cookie the lost history issued it at 1.
	signInAll(t, people, unseen, "account-u")
	if got := sessionGenerationOf(t, database.Pool, unseen); got < raised {
		t.Errorf("the first sign-in at %s after the reset made its session row at %d, want at least the reset's %d", unseen, got, raised)
	}
	if cookieMatches(t, database, unseen, 1) {
		t.Errorf("after %s's first sign-in, the lost history's cookie at generation 1 signs in; want it refused", unseen)
	}

	// The holder of kept signs in again between the two resets, under the first reset's number.
	if !cookieMatches(t, database, kept, raised) {
		t.Fatalf("control: after the first reset %s's new cookie at %d does not sign in", kept, raised)
	}
	// A second restore, to before the first reset: every session row is back where the backup
	// holds it, the row of the address first signed in after the reset is gone, and the floor is
	// gone. A restore writes what the guards refuse, so they are off for it.
	for _, statement := range []string{
		`alter table user_sessions disable trigger user_sessions_binding_columns`,
		`alter table session_generation_floors disable trigger session_generation_floors_guarded`,
		`update user_sessions set generation = ` + itoa(keptAt) + ` where login = '` + kept + `'`,
		`update user_sessions set generation = ` + itoa(signedOutAt+1) + ` where login = '` + signedOut + `'`,
		`delete from user_sessions where login = '` + unseen + `'`,
		`delete from session_generation_floors`,
		`alter table session_generation_floors enable trigger session_generation_floors_guarded`,
		`alter table user_sessions enable trigger user_sessions_binding_columns`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("the second restore, %q: %v", statement, err)
		}
	}
	time.Sleep(2 * time.Millisecond)
	again := runReset(t, database, brokerGreatest)
	if again <= raised {
		t.Errorf("the second reset raised to %d, want above the first's %d", again, raised)
	}
	if cookieMatches(t, database, kept, raised) {
		t.Errorf("after a second restore and reset, the cookie %s signed in with under the first reset's %d signs in; want it refused", kept, raised)
	}
}

// TestTheSecondResetRaisesItsKeptDeploymentFloor covers the reset's floor statement when its `*`
// row survived a restore. late@ first signs in after the first reset at that floor, then the
// restored database keeps the floor but has no late@ row. The next reset must raise that existing
// row, so the lost cookie at the first floor stays refused when late@ signs in again.
//
// Control in the runbook: the floor statement's conflict arm `do nothing` keeps the first floor,
// and the lost cookie signs in after the second reset.
func TestTheSecondResetRaisesItsKeptDeploymentFloor(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	first := runReset(t, database, 7)
	const late = "late@example.com"
	signInAll(t, people, late, "account-z1")
	lostAt := sessionGenerationOf(t, database.Pool, late)
	if !cookieMatches(t, database, late, lostAt) {
		t.Fatalf("control: %s's cookie at %d does not match before the restore", late, lostAt)
	}
	for _, statement := range []string{
		`alter table user_sessions disable trigger user_sessions_binding_columns`,
		`alter table people disable trigger people_recorded_account_kept`,
		`delete from user_sessions where login = '` + late + `'`,
		`delete from people where email = '` + late + `'`,
		`alter table people enable trigger people_recorded_account_kept`,
		`alter table user_sessions enable trigger user_sessions_binding_columns`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("the restore that keeps the floor, %q: %v", statement, err)
		}
	}
	time.Sleep(2 * time.Millisecond)
	second := runReset(t, database, 7)
	var floor int64
	if err := database.Pool.QueryRow(ctx, `select generation from session_generation_floors where login = '*'`).Scan(&floor); err != nil {
		t.Fatal(err)
	}
	signInAll(t, people, late, "account-z2")
	if got := sessionGenerationOf(t, database.Pool, late); got != floor || got < second || floor <= first {
		t.Errorf("after the second reset, the floor is %d and %s starts at %d; want its raised floor at least %d and above the first %d", floor, late, got, second, first)
	}
	if cookieMatches(t, database, late, lostAt) {
		t.Errorf("after the second reset, %s's cookie of the lost history at %d signs in; want it refused", late, lostAt)
	}
}

// itoa is n in decimal, for a statement the test writes out.
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
