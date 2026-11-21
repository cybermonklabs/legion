package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A personal token records, at its mint, the Google account of the credential that minted it
// (agent_tokens.subject), and acts only while people.sub records that account or none. The
// reviewer's case: account A held carol's address at the secrets broker and minted a token there,
// never signing in to Dispatch, so Dispatch recorded no account for carol; then account B, which
// Google gave the address to, became the account people records. A's token is refused as a revoked
// one at its next request (personalTokenActor), at an approve's or deny's re-read once its body has
// arrived (approverStanding), and in the mint's own re-check (mintAgentTokenSQL), whether or not the
// sign-in that recorded B revoked it, while B's token and a token that recorded no account act.
//
// Control: tokenAccountReplaced answering false (the account check removed); A's token then
// answers 200, its approve stands, and its mint inserts.
func TestAPersonalTokenOfAnAccountPeopleNoLongerRecordsActsOnNothing(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{signInIssuer: "https://accounts.google.com"})
	ctx := context.Background()
	const carol = "carol@example.com"
	seedPeople(t, database, carol)
	tokenOf := func(subject string) (plaintext, id string) {
		t.Helper()
		plaintext, hash, err := newAgentToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := database.Pool.QueryRow(ctx, `
			insert into agent_tokens (owner, name, token_hash, prefix, scopes, subject)
			values ($1, $2, $3, $4, $5, nullif($6, ''))
			returning id::text`, carol, "the token of "+subject, hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength],
			[]string{model.ScopePerson, model.ScopeApprove}, subject).Scan(&id); err != nil {
			t.Fatalf("store the token recording %q: %v", subject, err)
		}
		return plaintext, id
	}
	tokenA, idA := tokenOf("account-a")
	tokenB, idB := tokenOf("account-b")
	tokenNone, idNone := tokenOf("")
	whoami := func(plaintext string) *httptest.ResponseRecorder {
		t.Helper()
		return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, plaintext)
	}
	if response := whoami(tokenA); response.Code != http.StatusOK {
		t.Fatalf("A's token while people records no account for carol: %d %s, want 200", response.Code, response.Body.String())
	}

	if _, err := database.Pool.Exec(ctx, `update people set sub = 'account-b' where email = $1`, carol); err != nil {
		t.Fatalf("record B for carol: %v", err)
	}
	// No recording time, as a row from before the column holds, so the token recording no account
	// is held to the account alone here; TestACredentialRecordingNoAccountIsHeldToWhenTheAddressAccountWasRecorded
	// holds it to the time.
	storetest.SetRecordingTime(t, database, carol, nil)
	if response := whoami(tokenA); response.Code != http.StatusUnauthorized || responseCode(t, response) != "TOKEN_REVOKED" {
		t.Errorf("A's token once people records B: %d %s, want 401 TOKEN_REVOKED", response.Code, response.Body.String())
	}
	for name, plaintext := range map[string]string{"B's token": tokenB, "a token recording no account": tokenNone} {
		if response := whoami(plaintext); response.Code != http.StatusOK {
			t.Errorf("%s once people records B: %d %s, want 200", name, response.Code, response.Body.String())
		}
	}

	served := directServer(deps)
	standing := func(id string) (string, error) {
		owner := carol
		actor := model.Actor{Kind: "session", Owner: &owner, ID: carol, Act: &model.ActorAct{TokenID: id}}
		account, _, err := served.approverStanding(httptest.NewRequest(http.MethodPost, "/api/v1/credential-requests/x/approve", nil), actor)
		return account.subject, err
	}
	if _, err := standing(idA); err == nil {
		t.Error("an approve's re-read of A's token once people records B stood, want it refused")
	} else {
		wantAPIError(t, err, http.StatusUnauthorized, "TOKEN_REVOKED", "an approve's re-read of A's token once people records B")
	}
	for name, id := range map[string]string{"B's token": idB, "a token recording no account": idNone} {
		if _, err := standing(id); err != nil {
			t.Errorf("an approve's re-read of %s: %v, want it to stand", name, err)
		}
	}

	mint := func(id string) error {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var minted string
		return tx.QueryRow(ctx, mintAgentTokenSQL, carol, "minted under "+id, []byte("hash-"+id), "prefix00", []string{model.ScopePerson}, []string{},
			nil, nil, id, true).Scan(&minted, new(string), new(string), new([]string), new([]string), new(any), new(any), new(any))
	}
	if err := mint(idA); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("the mint's re-check of A's token once people records B answered %v, want no row inserted", err)
	}
	if err := mint(idB); err != nil {
		t.Errorf("the mint's re-check of B's token: %v, want a row inserted", err)
	}
}
