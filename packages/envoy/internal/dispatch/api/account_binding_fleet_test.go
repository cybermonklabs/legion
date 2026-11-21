package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Every check of the account binding holds only while no build without the binding is in the
// fleet (store.AccountBindingHolds, migration 20261120222130_account_binding_fleet): such a build
// checks no account, so beside it a replaced account's credential acts there whatever this build
// does, and this build does as that build does. Account A's credentials at carol's address, which
// people records account B for, are admitted while a process at owner lock protocol 1 without the
// binding is registered: A's personal token at its use, an approve's re-read of it (naming no
// account), its mint (whose token records none), a broker token recording A at its admission, and
// the membership answer's holder for A. Once that process is gone and its mark has aged out, each
// is refused as the binding refuses it.
//
// The second half is the control, in the test: the same checks on the same rows refuse once the
// fleet holds the binding, so the first half's admissions are the fence's, not checks that never
// refuse.
func TestAReplacedAccountsCredentialsActOnlyWhileABuildWithoutTheBindingIsInTheFleet(t *testing.T) {
	const google = "https://accounts.google.com"
	handler, database, deps := newTestServer(t, testServerOptions{signInIssuer: google})
	ctx := context.Background()
	const carol = "carol@example.com"
	seedPeople(t, database, carol)
	plaintext, hash, err := newAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	var idA string
	if err := database.Pool.QueryRow(ctx, `
		insert into agent_tokens (owner, name, token_hash, prefix, scopes, subject)
		values ($1, 'the token of account-a', $2, $3, $4, 'account-a') returning id::text`,
		carol, hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength], []string{model.ScopePerson, model.ScopeApprove}).Scan(&idA); err != nil {
		t.Fatalf("store account-a's token: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set sub = 'account-b' where email = $1`, carol); err != nil {
		t.Fatalf("record account-b for carol: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into server_processes (instance, commit, signing_keys, owner_lock_protocol)
		values (gen_random_uuid(), 'unbound0build', '{fingerprint}', 1)`); err != nil {
		t.Fatalf("register a process without the binding: %v", err)
	}

	served := directServer(deps)
	owner := carol
	tokenActor := model.Actor{Kind: "session", Owner: &owner, ID: carol, Act: &model.ActorAct{TokenID: idA}}
	brokerCaller := ciphertoken.Caller{Person: carol, AccountIssuer: google, AccountSubject: "account-a", CredentialCreatedAt: time.Now().Add(-time.Hour)}
	check := func(fleet string, held bool) {
		t.Helper()
		if response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, plaintext); held != (response.Code == http.StatusUnauthorized) {
			t.Errorf("%s: account-a's personal token answers %d %s; want 401 %t", fleet, response.Code, response.Body.String(), held)
		}
		subject, _, err := served.approverStanding(httptest.NewRequest(http.MethodPost, "/api/v1/credential-requests/x/approve", nil), tokenActor)
		switch {
		case held && err == nil:
			t.Errorf("%s: an approve's re-read of account-a's token stood; want it refused", fleet)
		case held:
			wantAPIError(t, err, http.StatusUnauthorized, "TOKEN_REVOKED", fleet+": an approve's re-read of account-a's token")
		case err != nil || subject != "":
			t.Errorf("%s: an approve's re-read of account-a's token answered %q, %v; want it to stand naming no account, as a build without the binding relays it", fleet, subject, err)
		}
		minted := agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted " + fleet}, plaintext)
		switch {
		case held && minted.Code != http.StatusUnauthorized:
			t.Errorf("%s: account-a's token minted %d %s; want 401 TOKEN_REVOKED", fleet, minted.Code, minted.Body.String())
		case !held && minted.Code != http.StatusCreated:
			t.Errorf("%s: account-a's token minted %d %s; want 201", fleet, minted.Code, minted.Body.String())
		case !held:
			var recorded *string
			if err := database.Pool.QueryRow(ctx, `select subject from agent_tokens where owner = $1 and name = $2`, carol, "minted "+fleet).Scan(&recorded); err != nil {
				t.Fatal(err)
			}
			if recorded != nil {
				t.Errorf("%s: the token account-a's token minted records %q; want no account", fleet, *recorded)
			}
		}
		if _, err := served.brokerPersonAdmitted(ctx, brokerCaller); held != (err != nil) {
			t.Errorf("%s: a broker token recording account-a was admitted with error %v; want refused %t", fleet, err, held)
		}
		holder, err := served.accountHolder(ctx, database.Pool, accessQuestion{person: carol, issuer: google, subject: "account-a", describes: true})
		if err != nil || holder == nil || *holder == held {
			t.Errorf("%s: the membership answer's holder for account-a is %v, %v; want %t", fleet, holder, err, !held)
		}
	}
	check("beside a process without the binding", false)

	if _, err := database.Pool.Exec(ctx, `delete from server_processes`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `update account_binding_legacy_mark set seen_at = now() - make_interval(secs => $1)`,
		(store.OwnerLockMarkWindow + time.Minute).Seconds()); err != nil {
		t.Fatal(err)
	}
	check("once no build without the binding is in the fleet", true)
}

// A revocation is owed for account-a once account-b's sign-in replaces it. Then account-a takes
// the address back through a build without the account binding, whose sign-in writes `sub` and
// settles nothing (legion-envoy-v32.20.2's store.PgPeopleStore.SignInSubject, its update spelled
// here with the row's sealed refresh token kept, since the test holds no key of that build to seal
// one). A pass of this build then settles the revocation and sends nothing: account-a holds the
// address, and its broker sign-in on a machine stays live and unrefused.
//
// Control, run by hand: with the pass's SettleHeld step removed, the pass revokes account-a at the
// broker (it is refused and its sign-in ended).
func TestARevocationOfAnAccountThatTookItsAddressBackThroughABuildWithoutTheBindingIsSettledNotPaid(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	const email = "takeback-unbound@example.com"
	if err := r.people.SignInSubject(ctx, email, "account-a", "refresh-a", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	machine := r.signInMachine(t, email, "account-a", strings.Repeat("t", 43))
	if err := r.people.SignInSubject(ctx, email, "account-b", "refresh-b", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	if owing, err := r.revocations.Owing(ctx, email); err != nil || len(owing) != 1 || owing[0].Subject != "account-a" {
		t.Fatalf("after account-b's sign-in, %s owes %+v, %v; want account-a's revocation", email, owing, err)
	}
	if _, err := r.dispatch.Pool.Exec(ctx, `
		update people
		set signed_in_at = now(), confirmed_at = now(), sub = $2
		where email = $1`, email, "account-a"); err != nil {
		t.Fatalf("account-a's sign-in through a build without the binding: %v", err)
	}
	r.served.revokeOwedBrokerCredentials(ctx)
	if r.refused(t, email, "account-a") || !r.machineSignInLive(t, machine) {
		t.Errorf("after the pass, the broker refuses account-a at %s %t and its sign-in on a machine is live %t; want account-a, which holds the address, neither refused nor ended",
			email, r.refused(t, email, "account-a"), r.machineSignInLive(t, machine))
	}
	if owing, err := r.revocations.Owing(ctx, email); err != nil || len(owing) != 0 {
		t.Errorf("after the pass, %s owes %+v, %v; want account-a's revocation settled", email, owing, err)
	}
}

// storeTokenOf stores a personal token of owner named name with the person scope, recording
// subject, or no account for "", and answers its plaintext and id.
func storeTokenOf(t *testing.T, database *store.Store, owner, name, subject string) (plaintext, id string) {
	t.Helper()
	plaintext, hash, err := newAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(context.Background(), `
		insert into agent_tokens (owner, name, token_hash, prefix, scopes, subject)
		values ($1, $2, $3, $4, $5, nullif($6, '')) returning id::text`,
		owner, name, hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength], []string{model.ScopePerson, model.ScopeApprove}, subject).Scan(&id); err != nil {
		t.Fatalf("store %s: %v", name, err)
	}
	return plaintext, id
}

// registerUnboundProcess registers a process at owner lock protocol 1 without the account binding,
// whose registration stamps the binding's mark.
func registerUnboundProcess(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into server_processes (instance, commit, signing_keys, owner_lock_protocol)
		values (gen_random_uuid(), 'unbound0build', '{fingerprint}', 1)`); err != nil {
		t.Fatalf("register a process without the binding: %v", err)
	}
}

// clearUnboundFleet ends every process registered and ages the binding's mark past its window,
// so no build without the binding is in the fleet.
func clearUnboundFleet(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `delete from server_processes`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `update account_binding_legacy_mark set seen_at = now() - make_interval(secs => $1)`,
		(store.OwnerLockMarkWindow + time.Minute).Seconds()); err != nil {
		t.Fatal(err)
	}
}

// A first recording made while the binding is off is kept too, and caught up as a first recording
// with the binding on would have been: carol's address records no account and holds a token that
// records none and one that records account-z, an account that never signed in. While a build
// without the binding is in the fleet, account-a is recorded as carol's first account (this build's
// update of sub, as a build without the binding writes it too), which revokes nothing, and the
// token from before it mints one more that records no account. That minted token acts while the
// binding is off and answers 401 TOKEN_REVOKED once it holds, before any catch-up. The catch-up
// then revokes all three tokens, owes the broker account-z's revocation, naming account-a, and
// keeps nothing.
//
// Control, run by hand: with the trigger keeping no first recording (the insert's and the
// update-from-none arm of keep_account_binding_replacement returning at once), the minted token
// acts once the binding holds, and the row fails.
func TestATokenMintedAfterAFirstRecordingTheBindingMissedIsRefusedAndRevokedOnceItHolds(t *testing.T) {
	const google = "https://accounts.google.com"
	handler, database, _ := newTestServer(t, testServerOptions{signInIssuer: google})
	ctx := context.Background()
	const carol = "carol@example.com"
	seedPeople(t, database, carol)
	before, beforeID := storeTokenOf(t, database, carol, "from before any account", "")
	_, otherID := storeTokenOf(t, database, carol, "of an account that never signed in", "account-z")
	registerUnboundProcess(t, database)
	if _, err := database.Pool.Exec(ctx, `update people set sub = 'account-a' where email = $1`, carol); err != nil {
		t.Fatal(err)
	}
	minted := agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted after the recording"}, before)
	if minted.Code != http.StatusCreated {
		t.Fatalf("beside a build without the binding, the token from before any account minted %d %s; want 201", minted.Code, minted.Body.String())
	}
	var made struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(minted.Body.Bytes(), &made); err != nil || made.Token == "" {
		t.Fatalf("read the minted token: %v: %s", err, minted.Body.String())
	}
	whoami := func() *httptest.ResponseRecorder {
		t.Helper()
		return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, made.Token)
	}
	if response := whoami(); response.Code != http.StatusOK {
		t.Fatalf("beside a build without the binding, the minted token answers %d %s; want 200", response.Code, response.Body.String())
	}
	clearUnboundFleet(t, database)
	if response := whoami(); response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "TOKEN_REVOKED") {
		t.Errorf("once the binding holds, the token minted after the kept first recording answers %d %s; want 401 TOKEN_REVOKED", response.Code, response.Body.String())
	}
	held, caught, err := store.CatchUpAccountBindingOf(ctx, database.Pool, carol, google)
	if err != nil || !held {
		t.Fatalf("the catch-up answered held %t, %v", held, err)
	}
	if !caught.FirstRecording || len(caught.Replaced) != 0 || caught.TokensRevoked != 3 || caught.Holder != "account-a" || !slices.Equal(caught.Owed, []string{"account-z"}) {
		t.Errorf("the catch-up did %+v; want the first recording caught up: 3 tokens revoked, account-z owed, account-a holding", caught)
	}
	for name, id := range map[string]string{"from before any account": beforeID, "of account-z": otherID, "minted after the recording": made.ID} {
		var revoked bool
		if err := database.Pool.QueryRow(ctx, `select revoked_at is not null from agent_tokens where id = $1::uuid`, id).Scan(&revoked); err != nil || !revoked {
			t.Errorf("after the catch-up the token %s is revoked %t, %v; want it revoked", name, revoked, err)
		}
	}
	var kept int
	if err := database.Pool.QueryRow(ctx, `select count(*) from account_binding_replacements`).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("after the catch-up %d account changes are kept, %v; want none", kept, err)
	}
}

// A token that records no account and was made after a replacement the binding missed is refused
// from the moment the binding holds, before any catch-up revokes it (tokenAccountReplaced reads
// the kept replacement, account_binding_replacements): account-b replaces account-a while a build
// without the binding is in the fleet, and a token recording no account is made after it, as the
// replaced account could make one then through either build. It acts while the binding is off and
// answers 401 TOKEN_REVOKED once it holds.
//
// Control, in the test: with the kept replacement deleted, the same token acts again, so the
// refusal is the kept replacement's and not the recording time's.
func TestATokenMadeAfterAReplacementTheBindingMissedIsRefusedOnceItHolds(t *testing.T) {
	const google = "https://accounts.google.com"
	handler, database, _ := newTestServer(t, testServerOptions{signInIssuer: google})
	ctx := context.Background()
	const carol = "carol@example.com"
	seedPeople(t, database, carol)
	if _, err := database.Pool.Exec(ctx, `update people set sub = 'account-a' where email = $1`, carol); err != nil {
		t.Fatal(err)
	}
	registerUnboundProcess(t, database)
	if _, err := database.Pool.Exec(ctx, `update people set sub = 'account-b' where email = $1`, carol); err != nil {
		t.Fatal(err)
	}
	plaintext, _ := storeTokenOf(t, database, carol, "made in the overlap", "")
	whoami := func() *httptest.ResponseRecorder {
		t.Helper()
		return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, plaintext)
	}
	if response := whoami(); response.Code != http.StatusOK {
		t.Fatalf("beside a build without the binding, the token made in the overlap answers %d %s; want 200", response.Code, response.Body.String())
	}
	clearUnboundFleet(t, database)
	if response := whoami(); response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "TOKEN_REVOKED") {
		t.Errorf("once the binding holds, the token made after the kept replacement answers %d %s; want 401 TOKEN_REVOKED", response.Code, response.Body.String())
	}
	if _, err := database.Pool.Exec(ctx, `delete from account_binding_replacements`); err != nil {
		t.Fatal(err)
	}
	if response := whoami(); response.Code != http.StatusOK {
		t.Errorf("control: with no replacement kept, the same token answers %d %s; want 200", response.Code, response.Body.String())
	}
}

// The revocation loop's pass catches up a replacement made while the binding was off, once the
// binding holds, against a real broker: account-b replaces account-a beside a build without the
// binding, and account-a's broker sign-in on a machine stays. A pass while the binding is off
// writes nothing and asks for the next within accountBindingCatchUpPoll; once it holds, a pass
// owes the broker account-a's revocation, pays it (the broker refuses account-a and ends its
// sign-in), and keeps nothing, so it asks for no poll.
//
// Control, run by hand: with the pass's catch-up removed, account-a's broker sign-in stays live.
func TestTheRevocationPassCatchesUpAReplacementOnceTheBindingHolds(t *testing.T) {
	ctx := context.Background()
	r := newRecoveryRig(t)
	r.served.deps.SignInIssuer = recoveryIssuer
	const email = "catch-up-pass@example.com"
	if err := r.people.SignInSubject(ctx, email, "account-a", "refresh-a", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	machine := r.signInMachine(t, email, "account-a", strings.Repeat("u", 43))
	registerUnboundProcess(t, r.dispatch)
	if err := r.people.SignInSubject(ctx, email, "account-b", "refresh-b", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	next := r.served.revokeOwedBrokerCredentials(ctx)
	if next.IsZero() || time.Until(next) > accountBindingCatchUpPoll {
		t.Errorf("a pass while the binding is off asked for the next at %v; want one within %s", next, accountBindingCatchUpPoll)
	}
	if r.refused(t, email, "account-a") || !r.machineSignInLive(t, machine) {
		t.Fatalf("a pass while the binding is off revoked account-a at the broker; want nothing revoked")
	}
	clearUnboundFleet(t, r.dispatch)
	if next := r.served.revokeOwedBrokerCredentials(ctx); !next.IsZero() {
		t.Errorf("the pass that caught up and paid asked for the next at %v; want none", next)
	}
	if !r.refused(t, email, "account-a") || r.machineSignInLive(t, machine) {
		t.Errorf("once the binding holds, the pass left the broker refusing account-a %t and its sign-in on a machine live %t; want it refused and ended",
			r.refused(t, email, "account-a"), r.machineSignInLive(t, machine))
	}
	var kept int
	if err := r.dispatch.Pool.QueryRow(ctx, `select count(*) from account_binding_replacements`).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("after the pass %d replacements are kept, %v; want none", kept, err)
	}
}

// heldGateWaits reports whether a transaction holds the binding's gate shared while it waits for
// a lock: a mint or a decision held in its transaction after its fleet read.
func heldGateWaits(t *testing.T, database *store.Store) bool {
	t.Helper()
	var waiting bool
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (select from pg_locks gate join pg_locks waits on waits.pid = gate.pid and not waits.granted
			where gate.locktype = 'advisory' and gate.database = (select oid from pg_database where datname = current_database())
				and gate.classid::bigint = $1 and gate.objid::bigint = $2 and gate.objsubid = 2
				and gate.mode = 'ShareLock' and gate.granted)`,
		store.OwnerLockGateNamespace, store.AccountBindingGateKey).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	return waiting
}

// arrivalWaitsOnTheGate reports whether backend pid waits for the binding's gate exclusively, as
// an arrival of a build without the binding takes it.
func arrivalWaitsOnTheGate(t *testing.T, database *store.Store, pid uint32) bool {
	t.Helper()
	var waiting bool
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (select from pg_locks where locktype = 'advisory'
			and database = (select oid from pg_database where datname = current_database())
			and classid::bigint = $1 and objid::bigint = $2 and objsubid = 2 and pid = $3
			and mode = 'ExclusiveLock' and not granted)`,
		store.OwnerLockGateNamespace, store.AccountBindingGateKey, pid).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	return waiting
}

// A mint and a decision's re-read read the fleet in their own transaction, and keep the binding's
// gate to its commit: each is held after its fleet read (a transaction of the test holds
// user_sessions ACCESS EXCLUSIVE, which the mint's insert and the re-read's select read), and while
// it waits there it holds the gate shared, and an arrival of a build without the binding (its
// dashboard publish) waits for it. Once the test lets it go, it ends (the mint answers 201, the
// re-read stands) and the arrival commits after it. A fleet read moved out of its transaction, to
// a statement of its own on the pool, would hold the gate only for that statement, and this row
// fails at "holds the gate".
//
// Control, run by hand: with accountsHeld reading the fleet on the pool (store.AccountBindingHolds
// in a transaction of its own, committed at once), no transaction holds the gate while the mint
// waits, and the row fails.
func TestTheMintAndTheDecisionHoldTheBindingsGateToTheirCommit(t *testing.T) {
	const google = "https://accounts.google.com"
	handler, database, deps := newTestServer(t, testServerOptions{signInIssuer: google})
	ctx := context.Background()
	const carol = "carol@example.com"
	seedPeople(t, database, carol)
	plaintext, id := storeTokenOf(t, database, carol, "the minting token", "")
	served := directServer(deps)
	owner := carol
	tokenActor := model.Actor{Kind: "session", Owner: &owner, ID: carol, Act: &model.ActorAct{TokenID: id}}

	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"the mint", func() error {
			if response := agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted while held"}, plaintext); response.Code != http.StatusCreated {
				return fmt.Errorf("answered %d %s, want 201", response.Code, response.Body.String())
			}
			return nil
		}},
		{"the decision's re-read", func() error {
			_, _, err := served.approverStanding(httptest.NewRequest(http.MethodPost, "/api/v1/credential-requests/x/approve", nil), tokenActor)
			return err
		}},
	} {
		t.Run(step.name, func(t *testing.T) {
			if _, err := database.Pool.Exec(ctx, `delete from dashboard_builds`); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Pool.Exec(ctx, `delete from account_binding_legacy_mark`); err != nil {
				t.Fatal(err)
			}
			hold, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(ctx)
			if _, err := hold.Exec(ctx, `lock table user_sessions in access exclusive mode`); err != nil {
				t.Fatal(err)
			}
			ran := make(chan error, 1)
			go func() { ran <- step.run() }()
			for deadline := time.Now().Add(15 * time.Second); !heldGateWaits(t, database); time.Sleep(10 * time.Millisecond) {
				select {
				case err := <-ran:
					t.Fatalf("%s ended (%v) while user_sessions was held; want it held after its fleet read", step.name, err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("no transaction holds the gate while %s waits: its fleet read is not in its transaction", step.name)
				}
			}
			conn, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			arrived := make(chan error, 1)
			go func() {
				_, err := conn.Exec(context.Background(), `insert into dashboard_builds (build, entry, commit, owner_lock_protocol) values ('unbound', '', 'unbound0build', 1)`)
				arrived <- err
			}()
			for deadline := time.Now().Add(10 * time.Second); !arrivalWaitsOnTheGate(t, database, conn.PgConn().PID()); time.Sleep(10 * time.Millisecond) {
				select {
				case err := <-arrived:
					t.Fatalf("an arrival without the binding ended (%v) while %s held the gate; want it to wait", err, step.name)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("an arrival without the binding did not wait for the gate %s holds", step.name)
				}
			}
			if err := hold.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-ran:
				if err != nil {
					t.Errorf("%s, once let go: %v", step.name, err)
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("%s still waited 15s after user_sessions was let go", step.name)
			}
			select {
			case err := <-arrived:
				if err != nil {
					t.Errorf("the arrival without the binding, once %s committed: %v", step.name, err)
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("the arrival without the binding still waited 15s after %s ended", step.name)
			}
		})
	}
}
