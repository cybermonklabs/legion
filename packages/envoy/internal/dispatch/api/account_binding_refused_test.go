package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// A personal token the account binding refuses is revoked once the binding holds, with everything
// it could have minted, and the broker is told, against a real broker. At carol's address account-a
// holds two tokens, one recording account-a and one recording no account, and account-b's
// recording replaces account-a with nothing kept (storetest.TestOnlyRecordAccountUnkept: a change
// the database stamped and nothing kept, as one made while this release's migrations run), so
// nothing is revoked. Once the binding holds, both are refused (tokenAccountReplaced, by the
// account the first records and by the time the second was made) but neither is revoked.
//
// Then a build without the binding is in the fleet. Both tokens act, as that build lets them, and
// each mints a token that records no account and was made after account-b's recording; the one
// recording account-a passes an approve's re-read, which names no account then, and the broker
// makes a machine login recording no approving account, as it makes one from that approve; and at
// dave's address a token from before its recording mints a token too, then revokes itself, so no
// refused token of dave is live. A pass of the revocation loop then writes nothing and asks for the
// next within accountBindingCatchUpPoll, since the binding is off and an address has something to
// catch up.
//
// Once that build is gone and its mark has aged out, one pass revokes the two refused tokens and
// the three tokens minted while one of them was live, dave's included, leaves account-b's own token
// recording account-b, owes the broker account-a's revocation, naming account-b, and pays it: the
// broker refuses account-a and ends the machine login, which records no approving account. The pass
// asks for no poll, since nothing is left to catch up. A later build without the binding in the
// fleet then admits none of those tokens, and the token recording account-a mints nothing.
//
// Controls, run by hand: with CatchUpAccountBinding's refused-token sweep removed (the RefusedLive
// branch never taken, and AccountBindingCatchUpAddresses' second arm removed), the pass while off
// asks for no poll and the pass that holds revokes nothing and owes nothing, and every assertion
// after it fails; with MintedWhileRefusedLiveSQL reading only live refused tokens (its arm for one
// revoked after the token's creation removed), dave's minted token stays live.
func TestARefusedTokenThatOutlivedItsRecordingIsRevokedWithWhatItMintedOnceTheBindingHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRecoveryRig(t)
	r.served.deps.SignInIssuer = recoveryIssuer
	mux := http.NewServeMux()
	served := register(mux, t.Context(), r.served.deps)
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	const carol, dave = "carol-outlived@example.com", "dave-outlived@example.com"
	for _, email := range []string{carol, dave} {
		if err := r.people.SignInSubject(ctx, email, "account-a", "refresh-a", "", model.MemberFull); err != nil {
			t.Fatal(err)
		}
	}
	recordsA, idRecordsA := storeTokenOf(t, r.dispatch, carol, "carol's token recording account-a", "account-a")
	recordsNone, idRecordsNone := storeTokenOf(t, r.dispatch, carol, "carol's token recording no account", "")
	daveNone, idDaveNone := storeTokenOf(t, r.dispatch, dave, "dave's token recording no account", "")
	for _, email := range []string{carol, dave} {
		storetest.TestOnlyRecordAccountUnkept(t, r.dispatch, email, "account-b")
	}
	var kept, owed int
	if err := r.dispatch.Pool.QueryRow(ctx, `select (select count(*) from account_binding_replacements), (select count(*) from broker_revocations)`).Scan(&kept, &owed); err != nil || kept != 0 || owed != 0 {
		t.Fatalf("setup: after account-b replaced account-a, %d changes are kept and %d revocations owed, %v; want none of either", kept, owed, err)
	}
	holderB, idHolderB := storeTokenOf(t, r.dispatch, carol, "carol's token recording account-b", "account-b")

	use := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		return agentRequest(t, mux, http.MethodGet, "/api/v1/whoami", nil, token)
	}
	refused := func(response *httptest.ResponseRecorder) bool {
		return response.Code == http.StatusUnauthorized && strings.Contains(response.Body.String(), "TOKEN_REVOKED")
	}
	mint := func(token, name string) (plaintext, id string, response *httptest.ResponseRecorder) {
		t.Helper()
		response = agentRequest(t, mux, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": name}, token)
		if response.Code != http.StatusCreated {
			return "", "", response
		}
		var made struct {
			Token string `json:"token"`
			ID    string `json:"id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &made); err != nil || made.Token == "" || made.ID == "" {
			t.Fatalf("read the token %s: %v: %s", name, err, response.Body.String())
		}
		return made.Token, made.ID, response
	}
	revoked := func(id string) bool {
		t.Helper()
		var gone bool
		if err := r.dispatch.Pool.QueryRow(ctx, `select revoked_at is not null from agent_tokens where id = $1::uuid`, id).Scan(&gone); err != nil {
			t.Fatal(err)
		}
		return gone
	}
	for name, token := range map[string]string{"carol's token recording account-a": recordsA, "carol's token recording no account": recordsNone, "dave's token recording no account": daveNone} {
		if response := use(token); !refused(response) {
			t.Fatalf("setup: with the binding holding, %s answers %d %s; want 401 TOKEN_REVOKED", name, response.Code, response.Body.String())
		}
	}

	// A build without the binding in the fleet.
	registerUnboundProcess(t, r.dispatch)
	childOfA, idChildOfA, minted := mint(recordsA, "minted by the token recording account-a")
	if childOfA == "" {
		t.Fatalf("beside a build without the binding, the token recording account-a minted %d %s; want 201", minted.Code, minted.Body.String())
	}
	childOfNone, idChildOfNone, minted := mint(recordsNone, "minted by the token recording no account")
	if childOfNone == "" {
		t.Fatalf("beside a build without the binding, the token recording no account minted %d %s; want 201", minted.Code, minted.Body.String())
	}
	childOfDave, idChildOfDave, minted := mint(daveNone, "minted by dave's token")
	if childOfDave == "" {
		t.Fatalf("beside a build without the binding, dave's token minted %d %s; want 201", minted.Code, minted.Body.String())
	}
	if response := agentRequest(t, mux, http.MethodDelete, "/api/v1/me/agent-tokens/"+idDaveNone, nil, daveNone); response.Code >= 300 {
		t.Fatalf("dave's token revoking itself answered %d %s; want it revoked", response.Code, response.Body.String())
	}
	owner := carol
	standing := model.Actor{Kind: "session", Owner: &owner, ID: carol, Act: &model.ActorAct{TokenID: idRecordsA}}
	if account, _, err := served.approverStanding(httptest.NewRequest(http.MethodPost, "/api/v1/credential-requests/x/approve", nil), standing); err != nil || account != (credentialAccount{}) {
		t.Fatalf("beside a build without the binding, an approve's re-read of the token recording account-a answered %+v, %v; want it to stand naming no account and no issuer", account, err)
	}
	login := testerR26PoolLogin(t, r, carol)
	if next := served.revokeOwedBrokerCredentials(ctx); next.IsZero() || time.Until(next) > accountBindingCatchUpPoll {
		t.Errorf("a pass while the binding is off asked for the next at %v; want one within %s, since an address has something to catch up", next, accountBindingCatchUpPoll)
	}
	if revoked(idRecordsA) || revoked(idChildOfA) || !testerR26LoginLive(t, r, login) {
		t.Fatalf("a pass while the binding is off revoked a token or ended the machine login; want nothing written")
	}

	// The build leaves and its mark ages out; one pass catches up.
	clearUnboundFleet(t, r.dispatch)
	if next := served.revokeOwedBrokerCredentials(ctx); !next.IsZero() {
		t.Errorf("the pass that caught up and paid asked for the next at %v; want none, since nothing is left to catch up", next)
	}
	tokens := map[string]struct{ plaintext, id string }{
		"carol's token recording account-a":                                {recordsA, idRecordsA},
		"carol's token recording no account":                               {recordsNone, idRecordsNone},
		"the token the token recording account-a minted while it was off":  {childOfA, idChildOfA},
		"the token the token recording no account minted while it was off": {childOfNone, idChildOfNone},
		"the token dave's token minted before it revoked itself":           {childOfDave, idChildOfDave},
	}
	for name, token := range tokens {
		if !revoked(token.id) {
			t.Errorf("once the binding holds, the pass left %s live; want it revoked", name)
		}
		if response := use(token.plaintext); !refused(response) {
			t.Errorf("once the binding holds, %s answers %d %s; want 401 TOKEN_REVOKED", name, response.Code, response.Body.String())
		}
	}
	if response := use(holderB); revoked(idHolderB) || response.Code != http.StatusOK {
		t.Errorf("control: once the binding holds, carol's token recording account-b is revoked %t and answers %d %s; want it live and 200", revoked(idHolderB), response.Code, response.Body.String())
	}
	if !r.refused(t, carol, "account-a") || testerR26LoginLive(t, r, login) {
		t.Errorf("once the binding holds, the pass left the broker refusing account-a at %s %t and the machine login recording no approving account live %t; want account-a refused and the login ended",
			carol, r.refused(t, carol, "account-a"), testerR26LoginLive(t, r, login))
	}
	if owing, err := r.revocations.Owing(ctx, carol); err != nil || len(owing) != 0 {
		t.Errorf("after the pass, %s owes %+v, %v; want account-a's revocation paid", carol, owing, err)
	}

	// A later build without the binding in the fleet admits none of them.
	registerUnboundProcess(t, r.dispatch)
	for name, token := range tokens {
		if response := use(token.plaintext); !refused(response) {
			t.Errorf("beside a later build without the binding, %s answers %d %s; want 401 TOKEN_REVOKED: a revoked token acts on no build", name, response.Code, response.Body.String())
		}
	}
	if plaintext, _, response := mint(recordsA, "minted by the token recording account-a in a later overlap"); plaintext != "" || !refused(response) {
		t.Errorf("beside a later build without the binding, the token recording account-a minted %d %s; want 401 TOKEN_REVOKED", response.Code, response.Body.String())
	}
}

// refusedParentAndOverlapChild sets up, at owner's address, a token the binding refuses and a token
// it minted while a build without the binding was in the fleet: account-a is recorded, a token
// recording no account is made, and account-b's recording replaces account-a with nothing kept
// (storetest.TestOnlyRecordAccountUnkept, a change the database stamped and nothing kept), so the
// token is refused but live. Then a build without the binding registers, the token mints a child
// through handler (201), and that build leaves and its mark ages out, so the binding holds. It
// answers the child's plaintext and id.
func refusedParentAndOverlapChild(t *testing.T, handler http.Handler, database *store.Store, owner string) (child, childID string) {
	t.Helper()
	seedPeople(t, database, owner)
	storetest.TestOnlyRecordAccountUnkept(t, database, owner, "account-a")
	parent, _ := storeTokenOf(t, database, owner, "account-a's token recording no account", "")
	storetest.TestOnlyRecordAccountUnkept(t, database, owner, "account-b")
	registerUnboundProcess(t, database)
	minted := agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "the overlap child"}, parent)
	if minted.Code != http.StatusCreated {
		t.Fatalf("setup: beside a build without the binding, the refused token minted %d %s; want 201", minted.Code, minted.Body.String())
	}
	var made struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(minted.Body.Bytes(), &made); err != nil || made.Token == "" {
		t.Fatalf("setup: read the overlap child: %v: %s", err, minted.Body.String())
	}
	clearUnboundFleet(t, database)
	return made.Token, made.ID
}

// A token that a refused token minted while a build without the binding was in the fleet mints and
// approves nothing as the holder once the binding holds, before any pass revokes it
// (tokenAccountReplaced's store.MintedWhileRefusedLiveSQL arm): such a child records no account and
// was made after the recording, so a mint by it would record the holder's account
// (credentialSubjectSQL) and an approve by it would name the holder, and neither would be revoked
// by the catch-up. Once the binding holds, the child answers 401 TOKEN_REVOKED, its mint is refused
// 401 TOKEN_REVOKED and no grandchild exists, and an approve's re-read of it is refused
// TOKEN_REVOKED. A token recording account-b, the holder's, acts throughout: the row's control
// that the binding refuses the child, not every token.
//
// Control, run by hand: with that arm removed from tokenAccountReplaced, the child answers 200, its
// mint answers 201 with a grandchild recording account-b, and the approve's re-read stands naming
// account-b.
func TestATokenMintedWhileARefusedTokenWasLiveMintsAndApprovesNothingOnceTheBindingHolds(t *testing.T) {
	t.Parallel()
	const google = "https://accounts.google.com"
	handler, database, deps := newTestServer(t, testServerOptions{signInIssuer: google})
	ctx := context.Background()
	const carol = "carol@example.com"
	child, childID := refusedParentAndOverlapChild(t, handler, database, carol)
	holder, _ := storeTokenOf(t, database, carol, "account-b's token", "account-b")
	refused := func(response *httptest.ResponseRecorder) bool {
		return response.Code == http.StatusUnauthorized && strings.Contains(response.Body.String(), "TOKEN_REVOKED")
	}
	if response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, child); !refused(response) {
		t.Errorf("once the binding holds, the overlap child answers %d %s; want 401 TOKEN_REVOKED", response.Code, response.Body.String())
	}
	if response := agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "the grandchild"}, child); !refused(response) {
		t.Errorf("once the binding holds, the overlap child minted %d %s; want 401 TOKEN_REVOKED", response.Code, response.Body.String())
	}
	var grandchildren int
	if err := database.Pool.QueryRow(ctx, `select count(*) from agent_tokens where name = 'the grandchild'`).Scan(&grandchildren); err != nil || grandchildren != 0 {
		t.Errorf("%d grandchildren exist, %v; want none", grandchildren, err)
	}
	owner := carol
	standing := model.Actor{Kind: "session", Owner: &owner, ID: carol, Act: &model.ActorAct{TokenID: childID}}
	subject, _, err := directServer(deps).approverStanding(httptest.NewRequest(http.MethodPost, "/api/v1/credential-requests/x/approve", nil), standing)
	if err == nil {
		t.Errorf("once the binding holds, an approve's re-read of the overlap child stood naming %q; want it refused", subject)
	} else {
		wantAPIError(t, err, http.StatusUnauthorized, "TOKEN_REVOKED", "an approve's re-read of the overlap child")
	}
	if response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, holder); response.Code != http.StatusOK {
		t.Errorf("control: account-b's token answers %d %s; want 200", response.Code, response.Body.String())
	}
}

// The arm for a token minted while a refused token was live (store.MintedWhileRefusedLiveSQL, in
// tokenAccountReplaced) reads only the address's own tokens: personalTokenActor runs it on every
// request of a token recording no account made after its address's recording, so a scan of
// agent_tokens there would grow each such request with the whole table. Over 5000 tokens of 500
// other addresses, analyzed, the statement the request runs (personalTokenActorSQL), explained
// with sequential scans off, scans agent_tokens sequentially nowhere, searches
// agent_tokens_canonical_owner (migration 20261119020200) by its key, canonical_login(owner), and
// reads every index it searches by that index's first key.
//
// Control, run by hand: with that index dropped before the plan, the row fails twice: "the arm's
// read of the address's tokens searches no agent_tokens_canonical_owner", and the arm's subplan
// reads agent_tokens_owner_created_at by `created_at`, "which names nothing of that index's first
// key column `owner`", a read of the whole index. Without the index that arm costs a request of
// such a token about 30 ms over 10,000 tokens and 290 ms over 100,000; with it, about 0.02 ms.
func TestTheArmForATokenMintedWhileARefusedTokenWasLiveReadsOnlyTheAddresssTokens(t *testing.T) {
	t.Parallel()
	_, database, _ := newTestServer(t, testServerOptions{signInIssuer: "https://accounts.google.com"})
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into agent_tokens (owner, name, token_hash, prefix, subject, created_at)
		select 'seed-' || (n % 500) || '@example.com', 'seed ' || n, sha256(('seed-' || n)::bytea), 'seed',
			case when n % 2 = 0 then 'account-seed' end, now() - make_interval(secs => n)
		from generate_series(1, 5000) n`); err != nil {
		t.Fatalf("setup: seed agent_tokens: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `analyze agent_tokens`); err != nil {
		t.Fatalf("setup: analyze agent_tokens: %v", err)
	}
	legacyWindow, markWindow := store.AccountBindingWindows()
	hash := sha256.Sum256([]byte("dsp_the token the plan is read for"))
	plan := planWithoutSeqScans(t, database, personalTokenActorSQL, hash[:], true, legacyWindow, markWindow)
	if explaintest.SeqScansRelation(t, plan, "agent_tokens") {
		t.Fatalf("a personal token's check scans agent_tokens sequentially; want each read of it through an index:\n%s", plan)
	}
	keys := explaintest.NewIndexKeys(database.Pool)
	keys.RequireIndexKeyed(t, plan, "agent_tokens_canonical_owner", "the arm's read of the address's tokens", explaintest.CondRequired)
	keys.RequireIndexKeyed(t, plan, explaintest.AnyIndex, "a personal token's check", explaintest.CondRequired)
}

// A change of the address's account that no row keeps still wakes the revocation loop: the trigger
// people_account_binding_replacement announces every change of sub on BrokerRevocationsChannel,
// kept or not (migration 20261121180429), so a token the binding refuses and what it minted are
// revoked by the loop's next pass rather than at whatever wake comes next. The overlap child of a
// refused token is set up (refusedParentAndOverlapChild), with the loop asleep; then a write of sub
// marked as this build's sign-in's (markBindingWriterSQL, which the trigger keeps nothing for)
// records account-c. It is announced, nothing is kept, and the pass the announcement wakes revokes
// the overlap child.
//
// Control, run by hand: with the trigger announcing only a change it keeps, no announcement comes,
// and the row fails at "announced".
func TestAChangeOfTheAccountNoRowKeepsStillWakesTheRevocationLoop(t *testing.T) {
	t.Parallel()
	const google = "https://accounts.google.com"
	handler, database, deps := newTestServer(t, testServerOptions{signInIssuer: google})
	ctx := context.Background()
	const carol = "carol@example.com"
	_, childID := refusedParentAndOverlapChild(t, handler, database, carol)
	listener, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `listen `+store.BrokerRevocationsChannel); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select set_config('dispatch.account_binding_writer', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `update people set sub = 'account-c' where email = $1`, carol); err != nil {
		t.Fatalf("a marked write of account-c: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := database.Pool.QueryRow(ctx, `select count(*) from account_binding_replacements`).Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("setup: the marked write left %d changes kept, %v; want none", kept, err)
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = listener.Conn().WaitForNotification(wait)
	cancel()
	if err != nil {
		t.Fatalf("a change of sub that no row keeps was not announced on %s (%v); want the revocation loop woken", store.BrokerRevocationsChannel, err)
	}
	directServer(deps).revokeOwedBrokerCredentials(ctx)
	var revoked bool
	if err := database.Pool.QueryRow(ctx, `select revoked_at is not null from agent_tokens where id = $1::uuid`, childID).Scan(&revoked); err != nil || !revoked {
		t.Errorf("after the pass the announcement wakes, the overlap child is revoked %t, %v; want it revoked", revoked, err)
	}
}
