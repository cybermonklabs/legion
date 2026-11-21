package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The tests in this file are the rows of the account-binding model's counterexamples: each is
// the shortest trace the model found against the rules of the head before them, made a test at the
// surface the trace runs through.

// TestAnEntryTokenActsWhateverItsPersonsAccount (the model's L1, review finding R14-2): a token an
// operator's exchange entry mapped (project_scope named: a pod's account, a workflow, a Google
// machine's account, listed for the person) records no account and no creation time, since no
// person's credential made it. It acts before the person's account is recorded, after the first
// recording and after another account replaces it, while a person's own token recording no
// account, made before the recording, is refused from the first recording on.
//
// Control: the admission holding the entry's token to an account like any other (held without
// the scope's arm): the entry's token is refused BROKER_TOKEN_RECORDS_NO_ACCOUNT from the first
// recording on, for as long as it lives.
func TestAnEntryTokenActsWhateverItsPersonsAccount(t *testing.T) {
	rig := newOutsideBrokerRig(t)
	deps := rig.offDeps
	deps.SignInIssuer = "https://accounts.google.com"
	served := directServer(deps)
	const person = "dana@example.com"
	ctx := context.Background()
	people := store.NewPgPeopleStore(rig.database.Pool, testSigningKey, nil)
	entry := ciphertoken.Caller{Person: person, ProjectScope: ciphertoken.ProjectScopeNamed, Projects: []string{"ACC"}}
	own := ciphertoken.Caller{Person: person, ProjectScope: ciphertoken.ProjectScopePerson}
	acts := func(when string, caller ciphertoken.Caller) {
		t.Helper()
		if _, err := served.brokerPersonAdmitted(ctx, caller); err != nil {
			t.Errorf("%s: %v, want it admitted", when, err)
		}
	}
	acts("the entry's token before any sign-in", entry)
	acts("the person's own token recording no account before any sign-in", own)
	if err := people.SignInSubject(ctx, person, "account-a", "refresh-a", "", model.MemberFull); err != nil {
		t.Fatalf("A's first sign-in: %v", err)
	}
	acts("the entry's token after the first recording", entry)
	_, err := served.brokerPersonAdmitted(ctx, own)
	wantAPIError(t, err, http.StatusUnauthorized, "BROKER_TOKEN_RECORDS_NO_ACCOUNT", "the person's own token recording no account, made before the first recording")
	if err := people.SignInSubject(ctx, person, "account-b", "refresh-b", "", model.MemberFull); err != nil {
		t.Fatalf("B's sign-in: %v", err)
	}
	acts("the entry's token after B replaced A", entry)
}

// TestAnAccountOfAnotherIssuerIsRefusedOnceTheAddressRecordsOne (the model's S1 with an account
// of another issuer, review finding F6): a broker token whose credential records an account of
// another issuer than Dispatch's sign-in issuer names an account Dispatch cannot compare with the
// one it records, so once people records an account for its person it is refused at its admission,
// however long after the recording its credential was made, where a token recording no account
// made that late acts. A broker token mints nothing anyway (brokerPersonScopes).
//
// Control: brokerForeignAccount answering false, the reviewer's alternative of holding such a
// token as one recording none: the token of another issuer made 10 minutes after the recording is
// admitted.
func TestAnAccountOfAnotherIssuerIsRefusedOnceTheAddressRecordsOne(t *testing.T) {
	rig := newOutsideBrokerRig(t)
	deps := rig.offDeps
	deps.SignInIssuer = "https://accounts.google.com"
	served := directServer(deps)
	const person = "dana@example.com"
	ctx := context.Background()
	var recordedAt time.Time
	if err := rig.database.Pool.QueryRow(ctx, `select date_trunc('second', now() - interval '20 minutes')`).Scan(&recordedAt); err != nil {
		t.Fatal(err)
	}
	late := recordedAt.Add(10 * time.Minute)
	foreign := ciphertoken.Caller{Person: person, ProjectScope: ciphertoken.ProjectScopePerson,
		AccountIssuer: "https://issuer.example", AccountSubject: "account-a", CredentialCreatedAt: late}
	none := ciphertoken.Caller{Person: person, ProjectScope: ciphertoken.ProjectScopePerson, CredentialCreatedAt: late}
	if _, err := served.brokerPersonAdmitted(ctx, foreign); err != nil {
		t.Fatalf("a token of another issuer while no account is recorded: %v, want it admitted", err)
	}
	seedPeople(t, rig.database, person)
	if _, err := rig.database.Pool.Exec(ctx, `update people set sub = 'account-b' where email = $1`, person); err != nil {
		t.Fatalf("record B's account: %v", err)
	}
	storetest.SetRecordingTime(t, rig.database, person, recordedAt)
	_, err := served.brokerPersonAdmitted(ctx, foreign)
	wantAPIError(t, err, http.StatusUnauthorized, "BROKER_TOKEN_RECORDS_NO_ACCOUNT", "a token of another issuer made 10 minutes after the recording")
	if _, err := served.brokerPersonAdmitted(ctx, none); err != nil {
		t.Errorf("a token recording no account made 10 minutes after the recording: %v, want it admitted", err)
	}
}

// TestNoCredentialIsMintedForOrApprovedWhileASignInIsInFlight (the model's S1 counterexamples of a
// mint racing a first sign-in, review finding R14-5, and of an approve racing it, found by the
// model in round 15): B's first sign-in to dana's address, which no people row holds, has stamped
// people.sub_recorded_at and has not committed. A's personal token, which records no account and
// was made before, mints a token and approves a machine login meanwhile. Each waits for the
// sign-in on the address's lock, reads what it committed, and is refused 401 TOKEN_REVOKED; no
// token is minted. Before, the mint's lock was the people row for share, which finds no row the
// uncommitted insert made, and the approve's re-read took no lock: each read no recording and
// went on, and the token the mint made, its creation after the stamp, acted as dana's once B's
// sign-in committed; the login the approve relayed, made more than the margin after the stamp
// when the sign-in took that long, would have too.
//
// The sign-in is held after its update by a trigger that waits on an advisory lock the test
// holds, the pattern hazards_test.go uses to hold a commit.
//
// A's token was used a moment ago (last_used_at), so the mint's record of its use writes nothing
// and waits for nothing. The first recording revokes A's token in the sign-in's own transaction
// (store.revokeOtherAccountsTokensSQL), so a use record that wrote the row would wait on that row
// for the sign-in too, and this test could no longer tell the mint's wait on the address's lock
// from it.
//
// Control: createAgentToken and approverStanding without store.AddressLockSharedSQL: each
// answers before the sign-in commits, the mint 201 with a token.
func TestNoCredentialIsMintedForOrApprovedWhileASignInIsInFlight(t *testing.T) {
	rig := newOutsideBrokerRig(t)
	deps := rig.offDeps
	deps.SignInIssuer = "https://accounts.google.com"
	served := directServer(deps)
	const person = "dana@example.com"
	const hold = 3862
	ctx := context.Background()
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := rig.database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var rows int
	if err := rig.database.Pool.QueryRow(ctx, `select count(*) from people where email = $1`, person).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("dana's people rows = %d (%v), want none", rows, err)
	}
	plaintext, hash, err := newAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	var tokenID string
	if err := rig.database.Pool.QueryRow(ctx, `
		insert into agent_tokens (owner, name, token_hash, prefix, scopes, created_at, last_used_at)
		values ($1, 'A''s, made before', $2, $3, $4, now() - interval '1 hour', now()) returning id::text`,
		person, hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength], []string{model.ScopePerson, model.ScopeApprove}).Scan(&tokenID); err != nil {
		t.Fatal(err)
	}
	exec(`create function hold_the_sign_in() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock_shared(3862);
			return null;
		end $$`)
	exec(`create trigger hold_the_sign_in after update on people for each row execute function hold_the_sign_in()`)
	t.Cleanup(func() {
		_, _ = rig.database.Pool.Exec(context.Background(), `drop trigger if exists hold_the_sign_in on people`)
		_, _ = rig.database.Pool.Exec(context.Background(), `drop function if exists hold_the_sign_in()`)
	})
	held, err := rig.database.Pool.Acquire(ctx)
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

	signedIn := make(chan error, 1)
	go func() {
		signedIn <- store.NewPgPeopleStore(rig.database.Pool, testSigningKey, nil).SignInSubject(ctx, person, "account-b", "refresh-b", "", model.MemberFull)
	}()
	// A call must still be waiting while the sign-in is held; one that is not is named, with what
	// it answered.
	type call struct {
		name   string
		done   <-chan struct{}
		answer func() string
	}
	waitFor := func(what, like string, want int, calls ...call) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			for _, c := range calls {
				select {
				case <-c.done:
					t.Fatalf("%s: %s answered before the sign-in committed: %s", what, c.name, c.answer())
				default:
				}
			}
			waiting, err := storetest.CountLockWaits(ctx, rig.database, like)
			if err != nil {
				t.Fatal(err)
			}
			if waiting >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d waiting after 10s, want %d", what, waiting, want)
			}
		}
	}
	waitFor("B's sign-in holds after stamping its update", "%update people%sub = $3%", 1)

	minted := httptest.NewRecorder()
	mintDone := make(chan struct{})
	go func() {
		defer close(mintDone)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/me/agent-tokens", strings.NewReader(`{"name":"during the sign-in"}`))
		request.Header.Set("Authorization", "Bearer "+plaintext)
		request.Header.Set("Content-Type", "application/json")
		served.createAgentToken(minted, request)
	}()
	var approveErr error
	approveDone := make(chan struct{})
	go func() {
		defer close(approveDone)
		owner := person
		_, _, approveErr = served.approverStanding(httptest.NewRequest(http.MethodPost, "/api/v1/credential-requests/x/approve", nil),
			model.Actor{Kind: "session", Owner: &owner, ID: person, Act: &model.ActorAct{TokenID: tokenID}})
	}()
	waitFor("the mint and the approve wait on the address's lock", "%pg_advisory_xact_lock_shared(hashtext('dispatch-person-address')%", 2,
		call{"the mint", mintDone, func() string { return fmt.Sprintf("%d %s", minted.Code, minted.Body.String()) }},
		call{"the approve's re-read", approveDone, func() string { return fmt.Sprint(approveErr) }})

	release()
	if err := <-signedIn; err != nil {
		t.Fatalf("B's sign-in: %v", err)
	}
	<-mintDone
	<-approveDone
	if minted.Code != http.StatusUnauthorized || !strings.Contains(minted.Body.String(), "TOKEN_REVOKED") {
		t.Errorf("the mint by A's token made before the recording = %d %s, want 401 TOKEN_REVOKED", minted.Code, minted.Body.String())
	}
	wantAPIError(t, approveErr, http.StatusUnauthorized, "TOKEN_REVOKED", "the approve by A's token made before the recording")
	var made int
	if err := rig.database.Pool.QueryRow(ctx, `select count(*) from agent_tokens where name = 'during the sign-in'`).Scan(&made); err != nil || made != 0 {
		t.Errorf("tokens minted during the sign-in = %d (%v), want none", made, err)
	}
}
