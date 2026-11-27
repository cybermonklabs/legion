// agent_tokens_p0_test.go holds the personal tokens to G2 of dispatch://INFRA-5637: a personal
// token holds p0 (model.ScopeP0) only where its maker gave it, at the mint or at a later change of
// its scopes, and only a credential that holds p0 gives it. A mint that names no scopes gives every
// scope its maker holds but p0. read and p0 never share a token. The tests need
// DISPATCH_TEST_DATABASE_URL, as every test of this package that opens a store.
package api

import (
	"context"
	"crypto/sha256"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// agentTokensPath is the route that lists and mints the caller's personal tokens; a token's own
// path, which changes its scopes, is agentTokensPath, a slash and its id.
const agentTokensPath = "/api/v1/me/agent-tokens"

// personalTokenID is the id of the personal token whose plaintext is token, which
// personalTokenHolding wrote straight to agent_tokens.
func personalTokenID(t *testing.T, database *store.Store, token string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(token))
	var id string
	if err := database.Pool.QueryRow(context.Background(), `select id::text from agent_tokens where token_hash = $1`, hash[:]).Scan(&id); err != nil {
		t.Fatalf("read the id of a personal token: %v", err)
	}
	return id
}

// listedTokenScopes is alice's tokens as GET /api/v1/me/agent-tokens lists them to her browser: the
// scopes each one holds, by its id.
func listedTokenScopes(t *testing.T, handler http.Handler) map[string][]string {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, agentTokensPath, nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list alice's tokens: status=%d body=%s", listed.Code, listed.Body.String())
	}
	scopes := map[string][]string{}
	for _, token := range decodeBody[[]agentTokenRow](t, listed) {
		scopes[token.ID] = token.Scopes
	}
	return scopes
}

// requireTokensUnchanged fails unless alice's tokens are listed as before, each holding the scopes
// it held, and no token holds p0 that did not before.
func requireTokensUnchanged(t *testing.T, handler http.Handler, before map[string][]string) {
	t.Helper()
	after := listedTokenScopes(t, handler)
	if !maps.EqualFunc(after, before, slices.Equal) {
		t.Errorf("alice's tokens after the refused calls: %v, want them as they were: %v", after, before)
	}
	for id, scopes := range after {
		if slices.Contains(scopes, model.ScopeP0) && !slices.Contains(before[id], model.ScopeP0) {
			t.Errorf("token %s holds %q after the refused calls, and held %q before: a refused call gave it p0", id, scopes, before[id])
		}
	}
}

// tokenSender sends one request from one credential of alice's.
type tokenSender func(t *testing.T, method, path string, body any) *httptest.ResponseRecorder

// browserSender is alice at a browser, and bearerSender a personal token of hers.
func browserSender(handler http.Handler) tokenSender {
	return func(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, method, path, body, "alice")
	}
}

func bearerSender(handler http.Handler, token string) tokenSender {
	return func(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
		return agentRequest(t, handler, method, path, body, token)
	}
}

// TestAMintGivesP0OnlyWhereItsMakerNamesIt: a browser's mint that names no scopes holds person and
// approve, and no p0; one naming person, approve and p0 holds the three; one naming p0 alone holds
// p0 alone. A token holding person, approve and p0 that mints naming no scopes gives person and
// approve, and naming person and p0 gives those two. GET /api/v1/me/agent-tokens lists each token
// with the scopes it holds, p0 among them where it is held.
func TestAMintGivesP0OnlyWhereItsMakerNamesIt(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	holder := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove, model.ScopeP0)
	browser, holding := browserSender(handler), bearerSender(handler, holder)
	for _, mint := range []struct {
		name   string
		by     tokenSender
		scopes []string
		want   []string
	}{
		{"a browser's mint naming no scopes", browser, nil, []string{model.ScopePerson, model.ScopeApprove}},
		{"a browser's mint naming person, approve and p0", browser,
			[]string{model.ScopePerson, model.ScopeApprove, model.ScopeP0}, []string{model.ScopePerson, model.ScopeApprove, model.ScopeP0}},
		{"a browser's mint naming p0 alone", browser, []string{model.ScopeP0}, []string{model.ScopeP0}},
		{"a mint naming no scopes by a token holding person, approve and p0", holding, nil, []string{model.ScopePerson, model.ScopeApprove}},
		{"a mint naming person and p0 by a token holding person, approve and p0", holding,
			[]string{model.ScopePerson, model.ScopeP0}, []string{model.ScopePerson, model.ScopeP0}},
	} {
		t.Run(mint.name, func(t *testing.T) {
			body := map[string]any{"name": mint.name}
			if mint.scopes != nil {
				body["scopes"] = mint.scopes
			}
			response := mint.by(t, http.MethodPost, agentTokensPath, body)
			if response.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s, want 201", response.Code, response.Body.String())
			}
			minted := decodeBody[agentTokenResponse](t, response)
			if !slices.Equal(minted.Scopes, mint.want) {
				t.Errorf("the minted token holds %q, want %q", minted.Scopes, mint.want)
			}
			if listed := listedTokenScopes(t, handler)[minted.ID]; !slices.Equal(listed, mint.want) {
				t.Errorf("the token list shows the minted token %s holding %q, want %q", minted.ID, listed, mint.want)
			}
		})
	}
	want := []string{model.ScopePerson, model.ScopeApprove, model.ScopeP0}
	if listed := listedTokenScopes(t, handler)[personalTokenID(t, database, holder)]; !slices.Equal(listed, want) {
		t.Errorf("the token list shows the token that minted holding %q, want %q", listed, want)
	}
}

// TestACredentialWithoutP0CannotGiveIt: a token holding person and approve, which may mint and
// change its person's tokens, is refused 403 OAUTH_SCOPE_REQUIRED, with the challenge naming p0,
// each time it names p0: a mint, a change of its own scopes, a change of another token, and a
// change of a token that holds p0 already to a set that names p0. After them alice's tokens are
// listed as before, and no token holds p0 that did not.
func TestACredentialWithoutP0CannotGiveIt(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	without := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove)
	other := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove)
	holding := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove, model.ScopeP0)
	withP0 := []string{model.ScopePerson, model.ScopeApprove, model.ScopeP0}
	before := listedTokenScopes(t, handler)
	send := bearerSender(handler, without)
	for _, attempt := range []struct {
		name, method, path string
		body               map[string]any
	}{
		{"its mint naming p0", http.MethodPost, agentTokensPath, map[string]any{"name": "a token with p0", "scopes": withP0}},
		{"its change of its own scopes adding p0", http.MethodPatch, agentTokensPath + "/" + personalTokenID(t, database, without), map[string]any{"scopes": withP0}},
		{"its change of another token adding p0", http.MethodPatch, agentTokensPath + "/" + personalTokenID(t, database, other), map[string]any{"scopes": withP0}},
		{"its change of a token that holds p0 already to a set that names p0", http.MethodPatch, agentTokensPath + "/" + personalTokenID(t, database, holding), map[string]any{"scopes": withP0}},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			requireP0Refusal(t, send(t, attempt.method, attempt.path, attempt.body))
		})
	}
	requireTokensUnchanged(t, handler, before)
}

// TestAChangeOfScopesGivesP0FromACredentialThatHoldsIt: a browser's change of a token holding
// person and approve to a set that adds p0 is taken, and so is the same change of another such
// token by a token holding person, approve and p0. Each answers 200 with the token holding p0, and
// the token list shows p0 on the changed token.
func TestAChangeOfScopesGivesP0FromACredentialThatHoldsIt(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	holder := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove, model.ScopeP0)
	want := []string{model.ScopePerson, model.ScopeApprove, model.ScopeP0}
	for _, change := range []struct {
		name string
		by   tokenSender
	}{
		{"a browser's change", browserSender(handler)},
		{"a change by a token holding person, approve and p0", bearerSender(handler, holder)},
	} {
		t.Run(change.name, func(t *testing.T) {
			id := personalTokenID(t, database, personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove))
			response := change.by(t, http.MethodPatch, agentTokensPath+"/"+id, map[string]any{"scopes": want})
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", response.Code, response.Body.String())
			}
			if changed := decodeBody[agentTokenRow](t, response); !slices.Equal(changed.Scopes, want) {
				t.Errorf("the changed token holds %q, want %q", changed.Scopes, want)
			}
			if listed := listedTokenScopes(t, handler)[id]; !slices.Equal(listed, want) {
				t.Errorf("the token list shows the changed token %s holding %q, want %q", id, listed, want)
			}
		})
	}
}

// TestReadAndP0NeverShareAToken: a browser's mint naming read and p0, and one naming read, person
// and p0, are 400 TOKEN_SCOPES_INPUT, and so are a change of a read-only token to person and p0 and
// a change of a token that writes to read and p0. No answer is a 500, and alice's tokens are listed
// as before.
func TestReadAndP0NeverShareAToken(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	browser := browserSender(handler)
	writer := personalTokenID(t, database, personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove))
	minted := browser(t, http.MethodPost, agentTokensPath, map[string]any{"name": "a reader", "scopes": []string{model.ScopeRead, model.ScopePerson}})
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint a read-only token as a person: status=%d body=%s", minted.Code, minted.Body.String())
	}
	reader := decodeBody[agentTokenResponse](t, minted).ID
	before := listedTokenScopes(t, handler)
	for _, attempt := range []struct {
		name, method, path string
		body               map[string]any
	}{
		{"a mint naming read and p0", http.MethodPost, agentTokensPath,
			map[string]any{"name": "read and p0", "scopes": []string{model.ScopeRead, model.ScopeP0}}},
		{"a mint naming read, person and p0", http.MethodPost, agentTokensPath,
			map[string]any{"name": "read, person and p0", "scopes": []string{model.ScopeRead, model.ScopePerson, model.ScopeP0}}},
		{"a change of a read-only token to person and p0", http.MethodPatch, agentTokensPath + "/" + reader,
			map[string]any{"scopes": []string{model.ScopePerson, model.ScopeP0}}},
		{"a change of a token that writes to read and p0", http.MethodPatch, agentTokensPath + "/" + writer,
			map[string]any{"scopes": []string{model.ScopeRead, model.ScopeP0}}},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			response := browser(t, attempt.method, attempt.path, attempt.body)
			if response.Code >= http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s, want 400 and never a 5xx", response.Code, response.Body.String())
			}
			requireRefused(t, response, http.StatusBadRequest, codeTokenScopesInput)
		})
	}
	requireTokensUnchanged(t, handler, before)
}
