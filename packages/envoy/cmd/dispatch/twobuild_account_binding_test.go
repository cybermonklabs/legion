//go:build twobuild

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// ───── the account binding across the two builds ────────────────────────────

// This tree holds every credential of an address to the Google account Dispatch records for it
// (people.sub, and sub_recorded_at for a credential recording none), records the account a mint's
// credential is bound to, and revokes the address's tokens and owes the secrets broker a
// revocation when another account signs in. A build without that binding (the previous build, a
// release before it) does none of it, so while one serves beside this tree a replaced account's
// token acts on it, and this tree does as it does: it checks, records and replaces no account
// while a build without the binding is in the fleet, by the fence the owner lock reads, which sees
// such a build from its dashboard publish on, before anything else its boot writes, and until its
// registration is gone and its mark has aged out (store.AccountBindingHolds, migration
// 20261120222130_account_binding_fleet). The row below reads whether the previous build carries
// the binding from the account_binding it registers (previousBuildAccountBinding) and runs beside
// a build that does not; beside one that does, which dispatch-two-builds-same pairs this tree
// with, there is no overlap to test, and it skips (accountBindingRowSkip).

// accountBindingRowSkip answers whether the account binding row skips beside a previous build that
// registers the account binding value binding, and the reason it skips with: 0 is a build without
// the binding, and the row runs; store.AccountBindingProtocol is a build with it, and the row skips.
// No row states a case for any other value, so for one it answers an error, which fails the row
// rather than skipping it.
func accountBindingRowSkip(binding int) (reason string, skip bool, err error) {
	switch binding {
	case 0:
		return "", false, nil
	case store.AccountBindingProtocol:
		return fmt.Sprintf("The previous build registers account binding %d, this tree's (store.AccountBindingProtocol). It holds every credential to its account as this build does, so no build without the binding is in the fleet beside it and there is no overlap to test; TestAPersonalTokenOfAnAccountPeopleNoLongerRecordsActsOnNothing (internal/dispatch/api) covers the binding of one build.",
			store.AccountBindingProtocol), true, nil
	}
	return "", false, fmt.Errorf("the previous build registers account binding %d, and the account binding row states a case only for 0, a build without the binding, and %d, this tree's (store.AccountBindingProtocol)",
		binding, store.AccountBindingProtocol)
}

// The account binding row decides its case from the account binding the previous build registers,
// and from nothing else: 0 runs it, this tree's value skips it naming the row that covers one
// build, and any other value fails it. A decision that skipped at 0 would leave
// dispatch-two-builds, the job that pairs this tree with a release without the binding, passing
// with the overlap untested. The row runs the decision alone, with no build and no database.
func TestTheAccountBindingRowDecidesItsCaseByThePreviousBuildsBinding(t *testing.T) {
	if reason, skip, err := accountBindingRowSkip(0); err != nil || skip {
		t.Errorf("beside a previous build at account binding 0 the row skips %t (%q), error %v; want it run", skip, reason, err)
	}
	switch reason, skip, err := accountBindingRowSkip(store.AccountBindingProtocol); {
	case err != nil:
		t.Errorf("beside a previous build at this tree's account binding the row fails: %v; want it skipped", err)
	case !skip:
		t.Errorf("beside a previous build at this tree's account binding the row runs; want it skipped")
	case !strings.Contains(reason, "TestAPersonalTokenOfAnAccountPeopleNoLongerRecordsActsOnNothing"):
		t.Errorf("beside a previous build at this tree's account binding the row skips saying %q, which names no row that covers the binding", reason)
	}
	if _, _, err := accountBindingRowSkip(store.AccountBindingProtocol + 1); err == nil {
		t.Errorf("a previous build at account binding %d, for which no row states a case, gets no error; want one that fails the row", store.AccountBindingProtocol+1)
	}
}

// The sign-in this tree's process runs in the row below: Google's domain sign-in, under a fake
// issuer standing in for Google (DISPATCH_TEST_SIGNIN_DOMAIN_ISSUER) with the fake client the
// multiprocess sign-in rows use (cookieClientID), at a public origin of its own that each browser
// write names as its Origin.
const (
	bindingDomain    = "example.com"
	bindingServerURL = "http://dispatch.binding.test"
	bindingPerson    = "carol@example.com"
)

// bindingSignIn is the environment this tree's process signs people in under: cookie identity,
// the domain sign-in under issuer, and a secrets broker on a loopback port nothing answers on
// (127.0.0.1:9), so a sign-in that replaces an account records the broker revocation it owes
// (store.WithBrokerRevocations), which no pass can pay and so stays for the row to read.
func bindingSignIn(t *testing.T, issuer *oidctest.Issuer) map[string]string {
	t.Helper()
	accountToken := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountToken, []byte("dispatch-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"DISPATCH_IDENTITY":                         "cookie",
		"DISPATCH_SIGNIN_ISSUER":                    issuer.URL(),
		"DISPATCH_SIGNIN_CLIENT_ID":                 cookieClientID,
		"DISPATCH_SIGNIN_CLIENT_SECRET":             cookieClientSecret,
		"DISPATCH_SIGNIN_DOMAIN":                    bindingDomain,
		"DISPATCH_TEST_SIGNIN_DOMAIN_ISSUER":        "1",
		"DISPATCH_INSECURE_COOKIE":                  "1",
		"DISPATCH_SERVER_URL":                       bindingServerURL,
		"DISPATCH_AGENT_SECRETS_URL":                "http://127.0.0.1:9",
		"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": accountToken,
	}
}

// signInAccount signs bindingPerson in on p as the Google account subject, the whole code flow a
// browser walks (signInThrough's), and returns the session cookie.
func signInAccount(t *testing.T, p *dispatchProcess, issuer *oidctest.Issuer, subject string) *http.Cookie {
	t.Helper()
	issuer.SignInAs(map[string]any{"sub": subject, "email": bindingPerson, "email_verified": true, "hd": bindingDomain})
	started := browserGet(t, p.url("/auth/start"), nil)
	defer started.Body.Close()
	var state *http.Cookie
	for _, cookie := range started.Cookies() {
		if cookie.Name == "dispatch_oauth_state" {
			state = cookie
		}
	}
	if started.StatusCode != http.StatusFound || state == nil {
		t.Fatalf("/auth/start answered %d with cookies %v; want a redirect to the issuer and the sign-in state", started.StatusCode, started.Cookies())
	}
	code, returned := issuer.Authorize(t, started.Header.Get("Location"))
	callback := browserGet(t, p.url("/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(returned)), state)
	defer callback.Body.Close()
	for _, cookie := range callback.Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" {
			return cookie
		}
	}
	body, _ := io.ReadAll(callback.Body)
	t.Fatalf("the sign-in of %s as %s: /auth/callback answered %d and set no session cookie: %s", bindingPerson, subject, callback.StatusCode, body)
	return nil
}

// mintToken mints a personal token named name on p with session, as the dashboard does, and
// returns its plaintext.
func mintToken(t *testing.T, p *dispatchProcess, session *http.Cookie, name string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, p.url("/api/v1/me/agent-tokens"), strings.NewReader(`{"name":"`+name+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", bindingServerURL)
	request.AddCookie(session)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("mint %s: %v", name, err)
	}
	defer response.Body.Close()
	var minted struct {
		Token string `json:"token"`
	}
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("mint %s: %d %s", name, response.StatusCode, body)
	}
	if err := json.Unmarshal(body, &minted); err != nil || !strings.HasPrefix(minted.Token, "dsp_") {
		t.Fatalf("mint %s: %v: %s", name, err, body)
	}
	return minted.Token
}

// tokenUse sends a request that needs a person, as a token's agent does: GET
// /api/v1/me/agent-tokens with token as its bearer.
func tokenUse(t *testing.T, p *dispatchProcess, token string) httpAnswer {
	t.Helper()
	return p.send(t, http.MethodGet, "/api/v1/me/agent-tokens", "", http.Header{"Authorization": {"Bearer " + token}})
}

// bindingState is what the binding reads and writes of bindingPerson's address, for a failure to
// name: the account people records and when, each token with the account it records and whether
// it is revoked, and each broker revocation recorded.
func bindingState(t *testing.T, database *store.Store) string {
	t.Helper()
	ctx := context.Background()
	var sub, recorded string
	if err := database.Pool.QueryRow(ctx, `select coalesce(sub, ''), coalesce(sub_recorded_at::text, '') from people where email = $1`, bindingPerson).
		Scan(&sub, &recorded); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read %s's people row: %v", bindingPerson, err)
	}
	rows, err := database.Pool.Query(ctx, `
		select name || ': records ' || coalesce(subject, 'no account') || ', made ' || created_at::text ||
			case when revoked_at is null then ', live' else ', revoked ' || revoked_at::text end
		from agent_tokens where owner = $1 order by created_at`, bindingPerson)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	rows, err = database.Pool.Query(ctx, `
		select subject || ' taken by ' || holder_subject || case when done_at is null then ', owed' else ', settled' end
		from broker_revocations where email = $1 order by recorded_at`, bindingPerson)
	if err != nil {
		t.Fatal(err)
	}
	revocations, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s records account %q since %s; tokens [%s]; broker revocations [%s]",
		bindingPerson, sub, recorded, strings.Join(tokens, "; "), strings.Join(revocations, "; "))
}

// tokenRecord is one token's binding columns, by its name.
func tokenRecord(t *testing.T, database *store.Store, name string) (subject string, revoked bool) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(subject, ''), revoked_at is not null from agent_tokens where owner = $1 and name = $2`, bindingPerson, name).
		Scan(&subject, &revoked); err != nil {
		t.Fatalf("read token %s: %v", name, err)
	}
	return subject, revoked
}

// owedRevocations is how many broker revocations bindingPerson's address has recorded.
func owedRevocations(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from broker_revocations where email = $1`, bindingPerson).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// bindingMarked reports whether a build without the binding arrived within the mark's window
// (account_binding_legacy_mark, store.OwnerLockMarkWindow).
func bindingMarked(t *testing.T, database *store.Store) bool {
	t.Helper()
	var marked bool
	if err := database.Pool.QueryRow(context.Background(),
		`select exists (select from account_binding_legacy_mark where seen_at > now() - make_interval(secs => $1))`,
		store.OwnerLockMarkWindow.Seconds()).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	return marked
}

// unboundRegistrations is how many server_processes rows a build without the binding holds.
func unboundRegistrations(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from server_processes where account_binding = 0`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// previousRegistration matches the previous build's registration insert, which the window below
// holds.
var previousRegistration = regexp.MustCompile(`insert into server_processes`)

// mintByToken mints a personal token named name on p with token as its bearer, as an agent does,
// and returns its plaintext.
func mintByToken(t *testing.T, p *dispatchProcess, token, name string) string {
	t.Helper()
	answer := p.send(t, http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"`+name+`"}`,
		http.Header{"Authorization": {"Bearer " + token}, "Content-Type": {"application/json"}})
	var minted struct {
		Token string `json:"token"`
	}
	if answer.status != http.StatusCreated || json.Unmarshal(answer.body, &minted) != nil || !strings.HasPrefix(minted.Token, "dsp_") {
		t.Fatalf("mint %s by a token: %s", name, answer)
	}
	return minted.Token
}

// keptReplacements is how many replacements the database keeps as made while the binding was off
// (account_binding_replacements).
func keptReplacements(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from account_binding_replacements`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// owedSubjects is the accounts bindingPerson's owed broker revocations name, sorted.
func owedSubjects(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select subject from broker_revocations where email = $1 and done_at is null order by subject`, bindingPerson)
	if err != nil {
		t.Fatal(err)
	}
	subjects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return subjects
}

// A replaced account's credentials act on nothing once the account binding holds, and the binding
// holds only while no build without it is in the fleet. One row walks a rolling update through
// both builds on one database, carol@example.com signing in on this tree under four Google
// accounts in turn, a, b, c and d, each replacing the one before:
//
//  1. This tree alone, no build without the binding in the fleet: a's first sign-in, and a token
//     a's cookie mints, which records a (the row's own control that it can see the binding write).
//  2. The previous build boots and publishes its dashboard, and its registration is held (the test
//     holds server_processes in SHARE mode): the window between a boot's first write and its
//     registration, which server_processes alone does not see. b signs in on this tree, which
//     revokes nothing and owes nothing; a's token acts on it, and mints a token through this tree
//     that records no account; a token b's cookie mints records no account.
//  3. Both builds serve. c signs in on this tree, again revoking and owing nothing; a's token and
//     b's act on both builds; a's token mints a token through the previous build, which records no
//     account; a token c's cookie mints records none. The database keeps the two replacements.
//  4. The previous build stops, as a rollout's last old pod does: its registration is gone, and
//     its mark holds the binding off until it ages out (store.OwnerLockMarkWindow, which the test
//     moves past rather than waiting an hour). Then this tree refuses, with 401 TOKEN_REVOKED, a's
//     token (it records a, and people records c), the two tokens a's token minted in the overlap
//     through either build, b's and c's own (each records none and was made after a kept
//     replacement). Its revocation loop catches the kept replacements up: those four tokens are
//     revoked, and so is a's token, which the binding refuses and which a later build without the
//     binding would admit again; the broker is owed a's and b's revocations, and nothing stays
//     kept. c's cookie then mints a token that records c, which acts, and d's sign-in revokes the
//     address's tokens and owes c's revocation, the row's control that it can see the binding
//     replace.
//
// Controls, run by hand: AccountBindingHoldsSQL answering true (the gate removed) fails step 2,
// naming the state the previous build leaves unchecked; account_binding_fleet_read reading
// server_processes alone fails step 2 too, the window before the previous build registers; the
// trigger people_account_binding_replacement dropped (the catch-up removed) fails step 4, where
// the tokens a's token minted in the overlap act; and CatchUpAccountBinding's refused-token sweep
// removed fails step 4 at a's token, left live.
func TestTheAccountBindingStartsOnlyOnceNoBuildWithoutItIsInTheFleet(t *testing.T) {
	build := openPreviousBuild(t)
	if reason, skip, err := accountBindingRowSkip(previousBuildAccountBinding(t, build)); err != nil {
		t.Fatal(err)
	} else if skip {
		t.Skip(reason)
	}
	builds := newTwoBuildsOf(t, build)
	database := builds.database
	ctx := context.Background()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret, bindingServerURL+"/auth/callback")

	// 1. This tree alone.
	options := builds.options(bindingSignIn(t, issuer), nil)
	options.Binary = builds.binary
	builds.b = startDispatchProcessWith(t, builds.databaseURL(t)+"&application_name="+currentApplication, options)
	builds.b.waitHealthy(t)
	current := builds.b
	if bindingMarked(t, database) || unboundRegistrations(t, database) != 0 {
		t.Fatalf("before the %s boots, the fleet holds a build without the binding (a mark %t, %d registrations); want none",
			builds.previousName(), bindingMarked(t, database), unboundRegistrations(t, database))
	}
	sessionA := signInAccount(t, current, issuer, "account-a")
	tokenA := mintToken(t, current, sessionA, "token-a")
	if subject, _ := tokenRecord(t, database, "token-a"); subject != "account-a" {
		t.Fatalf("control: with no build without the binding in the fleet, the current build's mint by account-a's cookie records %q; want account-a. %s",
			subject, bindingState(t, database))
	}

	// 2. The previous build's boot, its registration held.
	hold, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `lock table server_processes in share mode`); err != nil {
		t.Fatal(err)
	}
	previousOptions := builds.options(nil, nil)
	previousOptions.Binary, previousOptions.WebDist = builds.build.path("envoy-dispatch"), builds.build.path("web")
	builds.a = startDispatchProcessWith(t, builds.databaseURL(t)+"&application_name="+previousApplication, previousOptions)
	previous := builds.a
	_, waitingIn := queuedBackend(t, database, hold, previousApplication, previousRegistration, 120*time.Second)
	t.Logf("the %s's registration waits in %q", builds.previousName(), waitingIn)
	if !bindingMarked(t, database) || unboundRegistrations(t, database) != 0 {
		t.Fatalf("with the %s's registration held, its dashboard publish stamped the binding's mark %t and %d of its registrations are in; want the mark and none",
			builds.previousName(), bindingMarked(t, database), unboundRegistrations(t, database))
	}
	window := "before the " + builds.previousName() + " registers (its dashboard publish is in, its registration waits)"
	sessionB := signInAccount(t, current, issuer, "account-b")
	if answer := tokenUse(t, current, tokenA); answer.status != http.StatusOK {
		t.Errorf("%s: the current build answered account-a's token %s, after account-b's sign-in replaced account-a; want it to act as a build without the binding lets it (200), since the %s, which checks no account, is in the fleet. %s",
			window, answer, builds.previousName(), bindingState(t, database))
	}
	if _, revoked := tokenRecord(t, database, "token-a"); revoked || owedRevocations(t, database) != 0 {
		t.Errorf("%s: account-b's sign-in on the current build revoked account-a's token (%t) or owed the broker a revocation (%d); want neither, as the %s's sign-in does neither. %s",
			window, revoked, owedRevocations(t, database), builds.previousName(), bindingState(t, database))
	}
	tokenB := mintToken(t, current, sessionB, "token-b")
	if subject, _ := tokenRecord(t, database, "token-b"); subject != "" {
		t.Errorf("%s: the current build's mint by account-b's cookie records %q; want no account, as the %s's mint records. %s",
			window, subject, builds.previousName(), bindingState(t, database))
	}
	aThroughCurrent := mintByToken(t, current, tokenA, "a-through-current")
	if subject, _ := tokenRecord(t, database, "a-through-current"); subject != "" {
		t.Errorf("%s: the current build's mint by account-a's token records %q; want no account. %s", window, subject, bindingState(t, database))
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	previous.waitHealthy(t)

	// 3. Both builds serve.
	both := "while the " + builds.previousName() + " and the current build both serve"
	sessionC := signInAccount(t, current, issuer, "account-c")
	for _, process := range []*dispatchProcess{previous, current} {
		for name, token := range map[string]string{"account-a's": tokenA, "account-b's": tokenB} {
			if answer := tokenUse(t, process, token); answer.status != http.StatusOK {
				t.Errorf("%s: the %s answered %s token %s, after account-c's sign-in; want it to act as it did before the binding (200). %s",
					both, builds.buildName(process), name, answer, bindingState(t, database))
			}
		}
	}
	tokenC := mintToken(t, current, sessionC, "token-c")
	if subject, _ := tokenRecord(t, database, "token-c"); subject != "" {
		t.Errorf("%s: the current build's mint by account-c's cookie records %q; want no account. %s", both, subject, bindingState(t, database))
	}
	aThroughPrevious := mintByToken(t, previous, tokenA, "a-through-previous")
	if subject, _ := tokenRecord(t, database, "a-through-previous"); subject != "" {
		t.Errorf("%s: the %s's mint by account-a's token records %q; want no account. %s", both, builds.previousName(), subject, bindingState(t, database))
	}
	if kept := keptReplacements(t, database); kept != 2 {
		t.Errorf("%s: the database keeps %d replacements; want account-a's and account-b's. %s", both, kept, bindingState(t, database))
	}
	for _, name := range []string{"token-a", "token-b"} {
		if _, revoked := tokenRecord(t, database, name); revoked {
			t.Errorf("%s: %s is revoked; want the current build's sign-ins to revoke nothing. %s", both, name, bindingState(t, database))
		}
	}
	if owed := owedRevocations(t, database); owed != 0 {
		t.Errorf("%s: the current build's sign-ins owe the broker %d revocations; want none. %s", both, owed, bindingState(t, database))
	}

	// 4. The previous build leaves, and its mark ages out.
	stop(t, previous)
	if unboundRegistrations(t, database) != 0 {
		t.Fatalf("the %s's registration outlived its stop; want it deregistered", builds.previousName())
	}
	if answer := tokenUse(t, current, tokenA); answer.status != http.StatusOK {
		t.Errorf("after the %s stopped and before its mark aged out, the current build answered account-a's token %s; want 200: the fence keeps the binding off for store.OwnerLockMarkWindow after a build without it arrived. %s",
			builds.previousName(), answer, bindingState(t, database))
	}
	if _, err := database.Pool.Exec(ctx, `update account_binding_legacy_mark set seen_at = now() - make_interval(secs => $1)`,
		(store.OwnerLockMarkWindow + time.Minute).Seconds()); err != nil {
		t.Fatal(err)
	}
	after := "once no build without the binding is in the fleet"
	for name, token := range map[string]string{
		"account-a's token, which records account-a,":                             tokenA,
		"the token account-a's token minted through the current build":            aThroughCurrent,
		"the token account-a's token minted through the " + builds.previousName(): aThroughPrevious,
		"account-b's token, which records none,":                                  tokenB,
		"account-c's token, which records none and was made in the overlap,":      tokenC,
	} {
		if answer := tokenUse(t, current, token); answer.status != http.StatusUnauthorized || answer.code() != "TOKEN_REVOKED" {
			t.Errorf("%s: the current build answered %s %s; want 401 TOKEN_REVOKED. %s", after, name, answer, bindingState(t, database))
		}
	}
	caughtUp := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if keptReplacements(t, database) == 0 {
			caughtUp = true
			break
		}
	}
	if !caughtUp {
		t.Fatalf("%s: the current build's revocation loop kept the replacements 60s; want them caught up. %s", after, bindingState(t, database))
	}
	if owed := owedSubjects(t, database); !slices.Equal(owed, []string{"account-a", "account-b"}) {
		t.Errorf("%s: the catch-up owes the broker the revocations of %q; want account-a's and account-b's. %s", after, owed, bindingState(t, database))
	}
	for _, name := range []string{"a-through-current", "a-through-previous", "token-b", "token-c"} {
		if _, revoked := tokenRecord(t, database, name); !revoked {
			t.Errorf("%s: the catch-up left %s live; want every token that records no account made since the first kept replacement revoked. %s", after, name, bindingState(t, database))
		}
	}
	if _, revoked := tokenRecord(t, database, "token-a"); !revoked {
		t.Errorf("%s: the catch-up left token-a live, which records account-a and which a later build without the binding would admit again; want it revoked. %s", after, bindingState(t, database))
	}
	tokenC2 := mintToken(t, current, sessionC, "token-c2")
	if subject, _ := tokenRecord(t, database, "token-c2"); subject != "account-c" {
		t.Errorf("%s: the current build's mint by account-c's cookie records %q; want account-c. %s", after, subject, bindingState(t, database))
	}
	if answer := tokenUse(t, current, tokenC2); answer.status != http.StatusOK {
		t.Errorf("%s: the current build answered account-c's new token %s; want 200. %s", after, answer, bindingState(t, database))
	}
	signInAccount(t, current, issuer, "account-d")
	if _, revoked := tokenRecord(t, database, "token-c2"); !revoked || !slices.Equal(owedSubjects(t, database), []string{"account-a", "account-b", "account-c"}) {
		t.Errorf("control: %s, account-d's sign-in revoked account-c's token %t and the broker is owed %q; want it revoked and account-c's revocation owed beside the two caught up. %s",
			after, revoked, owedSubjects(t, database), bindingState(t, database))
	}
}
