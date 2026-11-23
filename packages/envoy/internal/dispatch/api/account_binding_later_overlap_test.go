package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A replaced account's token mints nothing that acts once the binding holds again, however many
// times the binding goes off and on: carol's address records account-a, which holds a token that
// records it, minted while the binding held. Account-b replaces account-a while a build without
// the binding is in the fleet, and once it holds the catch-up owes the broker account-a. Then a
// later overlap: a build without the binding arrives again, and account-a's token, which acts there
// as that build lets it, mints a token that records no account. Once that build is gone and its
// mark has aged out, the minted token is account-a's credential as much as the token that minted
// it, and answers 401 TOKEN_REVOKED (or account-a's token is refused its mint in the first place).
// No replacement is kept for the later overlap, so neither the kept-replacement arm of
// tokenAccountReplaced nor a catch-up can see the minted token: only a refusal that follows the
// mint back to account-a's token does.
//
// Control, in the test: account-b's own token mints a token in the same overlap, once account-a's
// token was revoked, and that token acts once the binding holds, before and after a catch-up of the
// address (store.CatchUpAccountBindingOf, what the revocation loop's pass runs): the refusal and the
// revocation are of what a refused token could have minted, not of every token minted in an overlap.
func TestATokenOfAReplacedAccountMintsNothingThatActsOnceTheBindingHoldsAgain(t *testing.T) {
	t.Parallel()
	const google = "https://accounts.google.com"
	handler, database, _ := newTestServer(t, testServerOptions{signInIssuer: google})
	ctx := context.Background()
	const carol = "carol@example.com"
	seedPeople(t, database, carol)
	if _, err := database.Pool.Exec(ctx, `update people set sub = 'account-a' where email = $1`, carol); err != nil {
		t.Fatal(err)
	}
	tokenA, _ := storeTokenOf(t, database, carol, "the token of account-a", "account-a")
	whoami := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, token)
	}
	refused := func(response *httptest.ResponseRecorder) bool {
		return response.Code == http.StatusUnauthorized && strings.Contains(response.Body.String(), "TOKEN_REVOKED")
	}
	// mint answers the plaintext of a token token mints named name, or "" with the response when
	// the mint is refused.
	mint := func(token, name string) (string, *httptest.ResponseRecorder) {
		t.Helper()
		response := agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": name}, token)
		if response.Code != http.StatusCreated {
			return "", response
		}
		var made struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &made); err != nil || made.Token == "" {
			t.Fatalf("read the token %s: %v: %s", name, err, response.Body.String())
		}
		return made.Token, response
	}
	if response := whoami(tokenA); response.Code != http.StatusOK {
		t.Fatalf("with the binding holding and account-a recorded, account-a's token answers %d %s; want 200", response.Code, response.Body.String())
	}

	// The first overlap: account-b replaces account-a; the catch-up owes account-a once the binding
	// holds.
	registerUnboundProcess(t, database)
	if _, err := database.Pool.Exec(ctx, `update people set sub = 'account-b' where email = $1`, carol); err != nil {
		t.Fatal(err)
	}
	clearUnboundFleet(t, database)
	held, caught, err := store.CatchUpAccountBindingOf(ctx, database.Pool, carol, google)
	if err != nil || !held || !slices.Equal(caught.Replaced, []string{"account-a"}) || caught.Holder != "account-b" {
		t.Fatalf("the catch-up answered held %t, %+v, %v; want account-a's replacement caught up, account-b holding", held, caught, err)
	}
	if response := whoami(tokenA); !refused(response) {
		t.Fatalf("once the binding holds after the replacement, account-a's token answers %d %s; want 401 TOKEN_REVOKED", response.Code, response.Body.String())
	}
	tokenB, _ := storeTokenOf(t, database, carol, "the token of account-b", "account-b")

	// The later overlap: a build without the binding arrives again; no account changes.
	registerUnboundProcess(t, database)
	childA, mintedA := mint(tokenA, "minted by account-a's token in a later overlap")
	childB, mintedB := mint(tokenB, "minted by account-b's token in a later overlap")
	if childB == "" {
		t.Fatalf("beside a build without the binding, account-b's token minted %d %s; want 201", mintedB.Code, mintedB.Body.String())
	}
	clearUnboundFleet(t, database)

	if childA == "" {
		if !refused(mintedA) {
			t.Errorf("beside a build without the binding, account-a's token minted %d %s; want 201, or 401 TOKEN_REVOKED", mintedA.Code, mintedA.Body.String())
		}
	} else if response := whoami(childA); !refused(response) {
		t.Errorf("once the binding holds again, the token account-a's token minted in the later overlap answers %d %s; want 401 TOKEN_REVOKED: it is the replaced account's credential, and no replacement is kept for it", response.Code, response.Body.String())
	}
	if response := whoami(tokenA); !refused(response) {
		t.Errorf("once the binding holds again, account-a's token answers %d %s; want 401 TOKEN_REVOKED", response.Code, response.Body.String())
	}
	if response := whoami(childB); response.Code != http.StatusOK {
		t.Errorf("control: once the binding holds again, the token account-b's token minted in the later overlap answers %d %s; want 200", response.Code, response.Body.String())
	}
	if held, _, err := store.CatchUpAccountBindingOf(ctx, database.Pool, carol, google); err != nil || !held {
		t.Fatalf("the catch-up after the later overlap answered held %t, %v", held, err)
	}
	if response := whoami(childB); response.Code != http.StatusOK {
		t.Errorf("control: after the catch-up, the token account-b's token minted in the later overlap answers %d %s; want 200: no refused token was live when it was made", response.Code, response.Body.String())
	}
}
