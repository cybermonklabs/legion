package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// browserSessions is the rig's header identity with each caller a browser's session cookie of the
// address's current session generation, as a person signed in at their browser is to the reads
// that bind a credential to its account (mintCookie, approverStanding): a passkey's sign-in is a
// cookie, and only a cookie's caller is bound to the account people records.
type browserSessions struct {
	identity.HeaderIdentity
	sessions *store.PgSessionStore
}

func (b browserSessions) SessionCookie(r *http.Request) (string, int64, bool) {
	login, err := b.Login(r)
	if err != nil {
		return "", 0, false
	}
	generation, _, err := b.sessions.CurrentSessionGeneration(r.Context(), login)
	return login, generation, err == nil
}

// passkeyOutsideOrigin is the outside origin of these rows: the issuer Dispatch records a passkey's
// subject under (people.sub_issuer), and the passkey store's.
const passkeyOutsideOrigin = "https://outside.example.com"

// withPasskeys turns passkey sign-in on at passkeyOutsideOrigin, beside the restricted rig's
// Google sign-in, whose issuer the deployment's sign-in records accounts of.
func withPasskeys(options *testServerOptions) {
	options.passkeyIssuer = passkeyOutsideOrigin
	options.signInIssuer = "https://accounts.google.com"
}

// recordPasskeyAccount writes erin's people row as her first passkey enrollment leaves it: the
// subject the enrollment made, under the outside origin, and her sign-in standing.
func recordPasskeyAccount(t *testing.T, database *store.Store, email, subject string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update people set sub = $2, sub_issuer = $3, confirmed_at = now(), signed_in_at = now()
		where email = $1`, email, subject, passkeyOutsideOrigin); err != nil {
		t.Fatalf("record %s's passkey account: %v", email, err)
	}
}

// TestPasskeyApprovalRelaysTheOutsideOriginAsTheApproversIssuer (H7, Dispatch's half): a restricted
// member whose account is a passkey's, its subject under the outside origin, approves their own
// machine login at their browser, and Dispatch relays the approving account to the secrets broker
// as that pair: the broker's minted login records the outside origin and the passkey's subject,
// never Google's issuer. The broker's half, that it compares the pair it recorded, is the broker's
// TestPasskeyApproverIssuerIsRecordedAndComparedAtTheBroker.
//
// Control: relay every account under the sign-in issuer (approve ignoring
// credentialAccount.issuer), and the login records Google's issuer and the row fails naming it.
func TestPasskeyApprovalRelaysTheOutsideOriginAsTheApproversIssuer(t *testing.T) {
	r := newRestrictedTokenRig(t, withPasskeys)
	r.server.deps.Identity = browserSessions{HeaderIdentity: r.server.deps.Identity.(identity.HeaderIdentity), sessions: store.NewPgSessionStore(r.database.Pool)}
	const subject = "cGFzc2tleS1zdWJqZWN0IQ"
	recordPasskeyAccount(t, r.database, accessErin, subject)
	_, _, credential := r.approvedLogin(t, accessErin, "erin-laptop")
	var issuer, approver string
	if err := r.broker.Store.Pool.QueryRow(t.Context(), `select coalesce(approver_issuer, ''), coalesce(approver_subject, '')
		from launcher_credentials where id = $1::uuid`, credential).Scan(&issuer, &approver); err != nil {
		t.Fatalf("read the approving account of erin's login: %v", err)
	}
	if issuer != passkeyOutsideOrigin || approver != subject {
		t.Fatalf("erin's machine login records the approving account (%q, %q), want her passkey's (%q, %q)", issuer, approver, passkeyOutsideOrigin, subject)
	}
}

// TestAResetRevokesTheMembersDeviceLogins (R5, R16): the owner's Reset of a restricted member ends
// their sign-in, deletes their passkeys, and records in the same transaction that Dispatch owes the
// secrets broker the revocation of every machine login they approved, as a removal from the list
// does; the broker revocation loop's next pass pays it, so the device login a hijacker approved
// with the person's passkey ends too: its agent's token introspects not active, and its refresh is
// refused. The person stays on ACC's list.
//
// Control: owe nothing on a Reset (resetProjectAccessPerson not calling
// recordMachineLoginRevocation), and the row fails naming the revocation not owed.
func TestAResetRevokesTheMembersDeviceLogins(t *testing.T) {
	r := newRestrictedTokenRig(t, withPasskeys)
	ctx := t.Context()
	recordPasskeyAccount(t, r.database, accessErin, "cmVzZXQtc3ViamVjdC0xIQ")
	erins, erinsDevice, _ := r.approvedLogin(t, accessErin, "erin-laptop")
	erinsToken := r.sessionToken(t, erins, erinsDevice, "erin-agent", "ACC")

	reset := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/projects/ACC/access/people/"+accessErin+"/reset", nil, "alice")
	if reset.Code != http.StatusOK {
		t.Fatalf("alice's Reset of erin: %d %s, want 200", reset.Code, reset.Body.String())
	}
	if people := decodeBody[struct {
		People []string `json:"people"`
	}](t, reset).People; len(people) != 1 || people[0] != accessErin {
		t.Fatalf("ACC's list after the Reset is %v, want erin still on it", people)
	}
	if owed, _, err := store.NewMachineLoginRevocations(r.database.Pool).Owing(ctx, accessErin); err != nil || !owed {
		t.Fatalf("the revocation of erin's machine logins is owed %t (%v) after the Reset, want owed", owed, err)
	}
	var confirmed bool
	if err := r.database.Pool.QueryRow(ctx, `select confirmed_at is not null from people where email = $1`, accessErin).Scan(&confirmed); err != nil || confirmed {
		t.Fatalf("erin's sign-in stands %t (%v) after the Reset, want it ended", confirmed, err)
	}
	r.server.revokeBrokerDebts(ctx)
	if r.introspect(t, erinsToken).Active {
		t.Errorf("erin's agent's token introspects active after the Reset, want it ended")
	}
	if refreshed, err := r.client.Refresh(t, erins.key, erinsDevice); err == nil {
		t.Errorf("erin's device login refreshed after the Reset, answering a token expiring %s; want it refused", refreshed.Expiry)
	}
}

// TestAResetsRevocationIsLoggedAsTheResets: the broker revocation loop's lines for the owner's
// Reset name the Reset: the account it replaced as a passkey account the owner's Reset replaced,
// never one "another Google account took the address of", and the machine logins it revoked as a
// member's reset by a project's owner, not only one taken off an access list. Either other wording
// sends an operator looking for a sign-in or a removal that never happened.
//
// Control: log every revocation with the Google takeover's words (revokedLog ignoring passkey), and
// the line names another Google account and the row fails.
func TestAResetsRevocationIsLoggedAsTheResets(t *testing.T) {
	r := newRestrictedTokenRig(t, withPasskeys)
	ctx := t.Context()
	recordPasskeyAccount(t, r.database, accessErin, "cmVzZXQtbG9nLXN1YmplY3Qh")
	if reset := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/projects/ACC/access/people/"+accessErin+"/reset", nil, "alice"); reset.Code != http.StatusOK {
		t.Fatalf("alice's Reset of erin: %d %s, want 200", reset.Code, reset.Body.String())
	}
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	r.server.revokeBrokerDebts(ctx)
	written := logged.String()
	if !strings.Contains(written, "the credentials of a passkey account the project owner's Reset replaced") || strings.Contains(written, "another Google account") ||
		!strings.Contains(written, "machine logins of a restricted member taken off an access list or reset by a project's owner") {
		t.Fatalf("the revocation pass after the Reset logged:\n%s\nwant the passkey account the owner's Reset replaced, no Google account, and the machine logins of a member reset by a project's owner", written)
	}
}

// TestAnApprovalInFlightAcrossAResetMintsNoLogin (R5, R16): an approve whose standing Dispatch
// read before the owner's Reset of the approver (approveCredentialRecord: the account and the
// session generation it relays), and which reaches the secrets broker only after the Reset
// committed and the broker revocation loop's pass ran, mints nothing. The Reset gave the address
// a new passkey account and owes the broker the revocation of the old one at the generation the
// Reset left, so the broker refuses an approve that names the old account, or an older
// generation, and erin's machine never gets a token for that login.
//
// Control: the Reset keeps the address's account (store.PgPasskeys.EndAccountTx recording no new
// subject), and the late approve mints the login and the row fails naming its credential.
func TestAnApprovalInFlightAcrossAResetMintsNoLogin(t *testing.T) {
	r := newRestrictedTokenRig(t, withPasskeys)
	ctx := t.Context()
	const subject = "aW4tZmxpZ2h0LXN1YmplY3Qh"
	recordPasskeyAccount(t, r.database, accessErin, subject)
	machine := r.startLogin(t, accessErin, "erin-laptop")
	recordID, _ := r.recordOf(t, accessErin, machine)
	var generation int64
	if err := r.database.Pool.QueryRow(ctx, `select coalesce((select generation from user_sessions where login = $1), 0)`, accessErin).Scan(&generation); err != nil {
		t.Fatalf("read erin's session generation: %v", err)
	}
	inFlight := cipher.Decision{
		Approver: accessErin, Code: &machine.device.UserCode,
		ApproverIssuer: passkeyOutsideOrigin, ApproverSubject: subject, ApproverSessionGeneration: &generation,
	}

	if reset := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/projects/ACC/access/people/"+accessErin+"/reset", nil, "alice"); reset.Code != http.StatusOK {
		t.Fatalf("alice's Reset of erin: %d %s, want 200", reset.Code, reset.Body.String())
	}
	r.server.revokeBrokerDebts(ctx)
	if minted, err := r.server.deps.AgentSecrets.Approve(ctx, recordID, inFlight); err == nil {
		t.Fatalf("the approve Dispatch read before the Reset, relayed after the revocation pass, minted erin's login: %s", minted)
	}
	if token, err := r.client.DeviceToken(t, machine.key, machine.device); err == nil {
		t.Fatalf("erin's machine got a token for the login approved across the Reset, expiring %s", token.Expiry)
	}
}

// TestTwoRemovalsFromTheLastTwoListsEndThePasskeySignIn (R5): two owners take a restricted member
// off the last two lists that name them at the same moment, on a deployment with no broker, and
// the removal that commits second ends the sign-in and deletes the passkeys: each removal takes the
// address's lock before it removes the person (lockPasskeyRemoval) and reads whether a list still
// names them under it (endUnlistedPasskeySignIn), so the second reads the first's committed
// removal.
//
// Control: take no address lock before the removal (lockPasskeyRemoval without
// store.LockAddressTx), and each removal reads the other's row as still there, both skip the end,
// and the row fails naming the passkey still stored.
func TestTwoRemovalsFromTheLastTwoListsEndThePasskeySignIn(t *testing.T) {
	served, database, s := newRegisteredAccessServer(t, testServerOptions{passkeyIssuer: passkeyOutsideOrigin, signInIssuer: "https://accounts.google.com"})
	ctx := t.Context()
	if created := dispatchRequest(t, served, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTHER", "name": "Other project"}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create OTHER: %d %s", created.Code, created.Body.String())
	}
	for _, key := range []string{"ACC", "OTHER"} {
		if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/"+key+"/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
			t.Fatalf("alice names erin on %s: %d %s", key, listed.Code, listed.Body.String())
		}
	}
	const subject = "dHdvLXJlbW92YWxzLWVyaW4h"
	recordPasskeyAccount(t, database, accessErin, subject)
	if _, err := database.Pool.Exec(ctx, `insert into passkeys (credential_id, email, user_handle, public_key, aaguid, flags, project_key, made_by)
		values ('\x01'::bytea, $1, convert_to($2, 'UTF8'), '\x02'::bytea, ''::bytea, 0, 'ACC', 'alice')`, accessErin, subject); err != nil {
		t.Fatalf("store erin's passkey: %v", err)
	}

	begin := func() pgx.Tx {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}
	// Each removal as deleteProjectAccessPerson makes it: the links and the address, the list's
	// row, then the end.
	remove := func(tx pgx.Tx, key string) error {
		if err := s.lockPasskeyRemoval(ctx, tx, key, accessErin); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `delete from project_access where project_key = $1 and principal_kind = 'person' and principal = $2`, key, accessErin); err != nil {
			return err
		}
		return s.endUnlistedPasskeySignIn(ctx, tx, accessErin)
	}
	first, second := begin(), begin()
	if err := remove(first, "ACC"); err != nil {
		t.Fatalf("the removal from ACC: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- remove(second, "OTHER") }()
	// The removal from OTHER either reads the lists before ACC's commits (no lock) or waits for
	// ACC's address lock; ACC's commits once one of the two is seen.
	var secondErr error
	finished := false
	for deadline := time.Now().Add(time.Minute); !finished; {
		select {
		case secondErr = <-done:
			finished = true
			continue
		case <-time.After(20 * time.Millisecond):
		}
		var waiting int
		if err := database.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the removal from OTHER neither finished nor waited on the address's lock within a minute")
		}
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the removal from ACC: %v", err)
	}
	if !finished {
		secondErr = <-done
	}
	if secondErr != nil {
		t.Fatalf("the removal from OTHER: %v", secondErr)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("commit the removal from OTHER: %v", err)
	}
	if n := countRows(t, database, `select count(*) from passkeys where email = '`+accessErin+`'`); n != 0 {
		t.Fatalf("after both removals erin holds %d passkey(s), want every one deleted", n)
	}
}

// TestARemovalTakesTheLinksThenTheAddressBeforeItEndsTheSignIn (R5): a removal from the last list
// that names a person ends their passkey sign-in only under the address's lock, which it takes
// after their unused links of the project, the order Enroll takes them in (the link, then the
// address). While an enrollment holds erin's address, DELETE of erin from ACC waits for it holding
// her link of ACC and ends nothing; once the enrollment commits, the removal deletes her passkey.
// The end itself takes no lock (store's endPasskeySignInTx): it runs under the one its caller took.
//
// Controls: take the address's lock before the links (lockPasskeyRemoval calling
// store.LockAddressTx before store.DeleteInvitationsTx), and the removal waits with the link free,
// and the row fails naming it. Taking out store.LockAddressTx and EndAccountTx's own lock is not
// enough to break it, since the new account's binding (signInAccountTx) takes the address's lock
// too. Take out all three (no store.LockAddressTx, no lock in EndAccountTx, and EndAccountTx
// ending the sign-in alone with endPasskeySignInTx), and the removal finishes while the enrollment
// holds the address, and the row fails.
func TestARemovalTakesTheLinksThenTheAddressBeforeItEndsTheSignIn(t *testing.T) {
	served, database, _ := newRegisteredAccessServer(t, testServerOptions{passkeyIssuer: passkeyOutsideOrigin, signInIssuer: "https://accounts.google.com"})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC: %d %s", listed.Code, listed.Body.String())
	}
	const subject = "bG9jay1vcmRlci1lcmluIQ"
	recordPasskeyAccount(t, database, accessErin, subject)
	if _, err := database.Pool.Exec(ctx, `insert into passkeys (credential_id, email, user_handle, public_key, aaguid, flags, project_key, made_by)
		values ('\x01'::bytea, $1, convert_to($2, 'UTF8'), '\x02'::bytea, ''::bytea, 0, 'ACC', 'alice')`, accessErin, subject); err != nil {
		t.Fatalf("store erin's passkey: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into outside_invitations (token_sha256, email, project_key, made_by, expires_at)
		values (sha256('erin''s link of ACC'::bytea), $1, 'ACC', 'alice', now() + interval '1 hour')`, accessErin); err != nil {
		t.Fatalf("store erin's unused link of ACC: %v", err)
	}

	begin := func() pgx.Tx {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}
	enrollment := begin()
	if err := store.LockAddressTx(ctx, enrollment, accessErin); err != nil {
		t.Fatalf("the enrollment takes erin's address: %v", err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice")
	}()
	for deadline := time.Now().Add(time.Minute); ; {
		select {
		case removed := <-done:
			t.Fatalf("the removal of erin from ACC answered %d %s while the enrollment held her address, want it to wait for the address's lock", removed.Code, removed.Body.String())
		case <-time.After(20 * time.Millisecond):
		}
		waiting := countRows(t, database, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`)
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the removal of erin from ACC neither finished nor waited on the address's lock within a minute")
		}
	}
	var held *pgconn.PgError
	if _, err := begin().Exec(ctx, `select 1 from outside_invitations where email = $1 for update nowait`, accessErin); !errors.As(err, &held) || held.Code != "55P03" {
		t.Fatalf("erin's link of ACC, read while the removal waits for her address, answered %v, want it held by the removal (55P03): the links are taken before the address", err)
	}
	if err := enrollment.Commit(ctx); err != nil {
		t.Fatalf("commit the enrollment: %v", err)
	}
	if removed := <-done; removed.Code != http.StatusOK {
		t.Fatalf("the removal of erin from ACC answered %d %s once the enrollment committed, want 200", removed.Code, removed.Body.String())
	}
	if n := countRows(t, database, `select count(*) from passkeys where email = $1`, accessErin); n != 0 {
		t.Fatalf("after the removal from her last list erin holds %d passkey(s), want every one deleted", n)
	}
}

// TestARemovalWaitsForTheAddressBeforeItLocksThePersonsSessions (R5, the tester's red test): an
// enrollment's finish takes the person's address lock and then, recording the projects they no
// longer see, their sessions' owner lock (store's signInAccountTx, then
// store.RecordReachedProjects, the last advisory lock its callers take). A removal from the last
// list must take them in that order too. While an enrollment holds erin's address, DELETE of erin
// from ACC waits for the address holding no lock of erin's sessions, so the enrollment's next lock
// is granted, it commits, and the removal answers 200.
//
// Control: remove the person before taking the links and the address (deleteProjectAccessPerson
// calling removePerson before lockPasskeyRemoval, as at 516b5bfa981): the removal takes the
// sessions' owner lock first (access.RemovePrincipal) and then waits for the address, a cycle
// PostgreSQL breaks with 40P01, and the row fails.
func TestARemovalWaitsForTheAddressBeforeItLocksThePersonsSessions(t *testing.T) {
	served, database, _ := newRegisteredAccessServer(t, testServerOptions{passkeyIssuer: passkeyOutsideOrigin, signInIssuer: "https://accounts.google.com"})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC: %d %s", listed.Code, listed.Body.String())
	}
	enrollment, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enrollment.Rollback(context.Background()) })
	if err := store.LockAddressTx(ctx, enrollment, accessErin); err != nil {
		t.Fatalf("the enrollment takes erin's address: %v", err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice")
	}()
	for deadline := time.Now().Add(time.Minute); ; {
		select {
		case removed := <-done:
			t.Fatalf("the removal of erin from ACC answered %d %s while the enrollment held her address, want it to wait for the address's lock", removed.Code, removed.Body.String())
		case <-time.After(20 * time.Millisecond):
		}
		if countRows(t, database, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the removal of erin from ACC neither finished nor waited on an advisory lock within a minute")
		}
	}
	// The enrollment's next lock, taken as RecordReachedProjects takes it, after the address.
	if _, err := enrollment.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1), hashtext($2))`, store.SessionOwnerLock, accessErin); err != nil {
		t.Fatalf("the enrollment's lock of erin's sessions, after her address as signInAccountTx takes it: %v; want it granted, the removal holding no lock of hers while it waits for her address", err)
	}
	if err := enrollment.Commit(ctx); err != nil {
		t.Fatalf("commit the enrollment: %v", err)
	}
	if removed := <-done; removed.Code != http.StatusOK {
		t.Fatalf("the removal of erin from ACC answered %d %s once the enrollment committed, want 200", removed.Code, removed.Body.String())
	}
}

// TestARemovalWaitsForTheAddressBeforeAGoogleSignInLocksThePersonsSessions (R5): the lock order's
// second partner. A Google sign-in that makes the person restricted takes their address lock,
// writes their row, and then, recording the projects the old kind saw, their sessions' owner lock
// (store's signInAccountTx, projectsSeenAs, then store.RecordReachedProjects), as an enrollment's
// finish does. (A sign-in that makes a person full takes no sessions lock, since a full member
// loses no project.) So the removal that takes the address before the person
// (lockPasskeyRemoval) is in order with it too: while erin's sign-in, a full member's row under
// her Google account signed in again as restricted (which takesAMembersAddress allows for the same
// account), is held after writing her row, DELETE of erin from ACC waits for her address; once the
// sign-in goes on, it takes her sessions' lock and commits, and the removal answers 200.
//
// Control: remove the person before taking the links and the address (deleteProjectAccessPerson
// calling removePerson before lockPasskeyRemoval, as at 516b5bfa981), and the removal holds her
// sessions' lock while it waits for the address the sign-in holds; PostgreSQL fails one of them
// (40P01), and the row fails. Before this row signed erin in as restricted it signed a restricted
// row in as full, which takes no sessions lock, and the row passed under that control too.
func TestARemovalWaitsForTheAddressBeforeAGoogleSignInLocksThePersonsSessions(t *testing.T) {
	served, database, _ := newRegisteredAccessServer(t, testServerOptions{passkeyIssuer: passkeyOutsideOrigin, signInIssuer: "https://accounts.google.com"})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC: %d %s", listed.Code, listed.Body.String())
	}
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	if err := people.SignInSubject(ctx, accessErin, "google-erin", "refresh-erin", "", model.MemberFull); err != nil {
		t.Fatalf("erin's first sign-in as a full member: %v", err)
	}
	// The sign-in waits, after writing erin's row and before recording her projects, for a lock the
	// test holds.
	const hold = 386207
	exec(`create function hold_the_sign_in() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock_shared(386207);
			return null;
		end $$`)
	exec(`create trigger hold_the_sign_in after update on people for each row execute function hold_the_sign_in()`)
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `drop trigger if exists hold_the_sign_in on people`)
		_, _ = database.Pool.Exec(context.Background(), `drop function if exists hold_the_sign_in()`)
	})
	held, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if _, err := held.Exec(ctx, `select pg_advisory_lock($1)`, hold); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			if _, err := held.Exec(ctx, `select pg_advisory_unlock($1)`, hold); err != nil {
				t.Errorf("release the sign-in: %v", err)
			}
		}
	}
	defer release()
	waiting := func(want int64, what string) {
		t.Helper()
		for deadline := time.Now().Add(time.Minute); countRows(t, database, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`) < want; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: fewer than %d waits on an advisory lock within a minute", what, want)
			}
		}
	}

	signedIn := make(chan error, 1)
	go func() {
		signedIn <- people.SignInSubject(ctx, accessErin, "google-erin", "refresh-erin", "", model.MemberRestricted)
	}()
	waiting(1, "erin's sign-in held after writing her row")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice")
	}()
	waiting(2, "the removal of erin from ACC waiting for her address")
	select {
	case removed := <-done:
		t.Fatalf("the removal of erin from ACC answered %d %s while her sign-in held her address, want it to wait", removed.Code, removed.Body.String())
	default:
	}
	release()
	if err := <-signedIn; err != nil {
		t.Fatalf("erin's sign-in as a restricted member, which records her sessions' projects after her address: %v; want it to commit, the removal holding no lock of hers while it waits for her address", err)
	}
	if removed := <-done; removed.Code != http.StatusOK {
		t.Fatalf("the removal of erin from ACC answered %d %s once her sign-in committed, want 200", removed.Code, removed.Body.String())
	}
}

// TestTheOwnersAccessReadAnswersOneSnapshot (R5): the owner's access answer reads the passkeys of
// the people the list names and the project's open links in one snapshot, so an enrollment that
// commits between the two reads (server.betweenOwnersPasskeyReads) is either in both or in
// neither: the answer lists erin's link open and no passkey of hers, as the list stood when the
// read began, and never no passkey and no open link, which would make the Access tab drop a link
// it shows with no passkey to show for it. The next read lists her passkey and no open link, which
// proves the enrollment committed.
//
// Control: read the owner's fields on the pool, each query its own snapshot (getProjectAccess
// passing the pool rather than its read-only transaction), and the first answer holds neither and
// the row fails.
func TestTheOwnersAccessReadAnswersOneSnapshot(t *testing.T) {
	served, database, s := newRegisteredAccessServer(t, testServerOptions{passkeyIssuer: passkeyOutsideOrigin, signInIssuer: "https://accounts.google.com"})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC: %d %s", listed.Code, listed.Body.String())
	}
	const subject = "c25hcHNob3QtZXJpbiE"
	recordPasskeyAccount(t, database, accessErin, subject)
	if _, err := database.Pool.Exec(ctx, `insert into outside_invitations (token_sha256, email, project_key, made_by, expires_at)
		values (sha256('erin''s link of ACC'::bytea), $1, 'ACC', 'alice', now() + interval '1 hour')`, accessErin); err != nil {
		t.Fatalf("store erin's unused link of ACC: %v", err)
	}
	enrolled := 0
	s.betweenOwnersPasskeyReads = func() {
		enrolled++
		// The enrollment as store.PgPasskeys.Enroll commits it: the link used and the passkey
		// stored in one transaction.
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `update outside_invitations set used_at = now() where email = $1 and used_at is null`, accessErin); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `insert into passkeys (credential_id, email, user_handle, public_key, aaguid, flags, project_key, made_by)
				values ('\x03'::bytea, $1, convert_to($2, 'UTF8'), '\x04'::bytea, ''::bytea, 0, 'ACC', 'alice')`, accessErin, subject)
			return err
		}); err != nil {
			t.Errorf("commit erin's enrollment between the owner's two reads: %v", err)
		}
	}
	first := readAccess(t, served, "alice")
	s.betweenOwnersPasskeyReads = nil
	if enrolled != 1 {
		t.Fatalf("the owner's read ran the seam between its two reads %d times, want once", enrolled)
	}
	if first.Passkeys == nil || first.OpenInvitations == nil {
		t.Fatalf("alice's access answer carries no owner's passkey fields: %+v", first)
	}
	if len(*first.Passkeys) != 0 || len(*first.OpenInvitations) != 1 || (*first.OpenInvitations)[0].Email != accessErin {
		t.Fatalf("with erin's enrollment committed between the owner's two reads, the answer lists passkeys %+v and open links %+v, want no passkey and her link open, as the list stood when the read began",
			*first.Passkeys, *first.OpenInvitations)
	}
	after := readAccess(t, served, "alice")
	if after.Passkeys == nil || len(*after.Passkeys) != 1 || after.OpenInvitations == nil || len(*after.OpenInvitations) != 0 {
		t.Fatalf("after erin's enrollment committed, alice's access answer lists passkeys %+v and open links %+v, want her one passkey and no open link: the enrollment did not commit, so the read before it proved nothing",
			after.Passkeys, after.OpenInvitations)
	}
}

// TestOnlyAPersonalTokenThatListsTheProjectIsLetInToMakeItsLink (R5): the link's own test of a
// token (personalTokenListsProject), which runs beside the route's scope gate. A personal token
// whose list names the project is let in; one that lists another project alone, or none, is not,
// and nor is a broker-issued token, which names no token of its own, whatever it lists and
// whatever scopes it held. No broker token holds approve (brokerTokenActor), so through the routes
// the gate refuses one before this test reads it (cmd/dispatch's
// TestTheOwnersPersonalTokenThatListsTheProjectMakesItsLink): this row holds the link to a
// personal token should a broker token ever hold approve. A browser is let in by atBrowserAs,
// never here.
//
// Control: let in any credential that lists the project (personalTokenListsProject not reading
// TokenID), and the broker-issued token's row is let in and fails.
func TestOnlyAPersonalTokenThatListsTheProjectIsLetInToMakeItsLink(t *testing.T) {
	both := []string{model.ScopePerson, model.ScopeApprove}
	for _, row := range []struct {
		name       string
		credential *model.Credential
		letIn      bool
	}{
		{"a personal token listing PRIV among others", &model.Credential{Scopes: both, TokenID: "token-1", Projects: []string{"OTHER", "PRIV"}}, true},
		{"a personal token listing OTHER alone", &model.Credential{Scopes: both, TokenID: "token-1", Projects: []string{"OTHER"}}, false},
		{"a personal token listing no project", &model.Credential{Scopes: both, TokenID: "token-1"}, false},
		{"a broker-issued token listing PRIV, holding approve", &model.Credential{Scopes: both, Session: "session-1", Projects: []string{"PRIV"}}, false},
		{"a browser's cookie", model.BrowserActor("alice@example.com").Credential, false},
		{"no credential", nil, false},
	} {
		if got := personalTokenListsProject(row.credential, "PRIV"); got != row.letIn {
			t.Errorf("%s: let in %t, want %t", row.name, got, row.letIn)
		}
	}
}
