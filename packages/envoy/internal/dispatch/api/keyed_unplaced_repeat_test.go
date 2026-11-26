package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// keyedSeedRequest sends method target with body as caller of the seed, under the Idempotency-Key
// key.
func keyedSeedRequest(t *testing.T, seed privateSeed, caller privateCaller, method, target string, body any, key string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the body of %s %s: %v", method, target, err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	if login := caller.login(); login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer "+seed.bearer(t, caller))
	}
	request.Header.Set(contracts.IdempotencyKeyHeader, key)
	response := httptest.NewRecorder()
	seed.handler.ServeHTTP(response, request)
	return response
}

// wantKeyedRefusal fails unless response is the refusal of a repeat heldAnswerHidden neither runs
// nor replays: 409 KEY_COMMITTED, not replayed, holding none of first's answer.
func wantKeyedRefusal(t *testing.T, what string, response *httptest.ResponseRecorder, first string) {
	t.Helper()
	code, _ := refusalOf(t, response)
	if response.Code != http.StatusConflict || code != contracts.IdempotencyKeyCommitted ||
		response.Header().Get(contracts.IdempotencyReplayedHeader) != "" || strings.Contains(response.Body.String(), first) {
		t.Errorf("%s = %d %s (replayed %q), want 409 %s holding none of the first answer",
			what, response.Code, response.Body.String(), response.Header().Get(contracts.IdempotencyReplayedHeader), contracts.IdempotencyKeyCommitted)
	}
}

// TestAKeyedRepeatOfACreateIsPlacedByTheProjectItsBodyNames: a key belongs to a person whatever
// credential they write through (requestKeyLogin), so a repeat is held to what its caller sees now
// (heldAnswerHidden), and a create, whose address names no object, is placed by the project its
// body names, as its first request's check places it (refuseOutsideCallerProjects). PRIV is
// private, so no caller here sees every object, and each is placed by the project alone:
//
//   - an agent's token retries its writes: carol's token that lists no project creates an issue of
//     PUB, which it sees, under a key, and its retry is answered the first 201 replayed;
//   - carol, at her browser, creates an issue of PRIV, which she sees, under a key, and her retry is
//     answered that 201 replayed; her token that lists no project, which does not see PRIV, retries
//     the same create under the same key and is refused 409 KEY_COMMITTED, holding none of the
//     first answer, and no second issue is made: at this slice the create holds no caller to the
//     project its body names, so running it again would make the issue twice;
//   - a project create, whose body names the project it makes: carol's token creates SECR under a
//     key, SECR is made private by SQL, which takes it from that token's sight, and the token's
//     retry is refused the same way, never answered SECR's row.
//
// Control: heldAnswerHidden refusing every repeat of a route whose address names no object, under
// which each "replays" row answers 409 KEY_COMMITTED and the test fails; and placing a stored answer
// by the names in its JSON, under which the project create's retry replays SECR's row.
func TestAKeyedRepeatOfACreateIsPlacedByTheProjectItsBodyNames(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	actor := map[string]string{"kind": "session", "id": "session-of-carols-token"}

	t.Run("an agent token's create in a project it sees replays", func(t *testing.T) {
		create := map[string]any{"project": seedPublic, "title": "Filed by an agent's token under a key", "force": true, "actor": actor}
		first := keyedSeedRequest(t, seed, callerCarolToken, http.MethodPost, "/api/v1/issues", create, "token-create-pub")
		if first.Code != http.StatusCreated {
			t.Fatalf("the token's keyed create in %s = %d %s, want 201", seedPublic, first.Code, first.Body.String())
		}
		if again := keyedSeedRequest(t, seed, callerCarolToken, http.MethodPost, "/api/v1/issues", create, "token-create-pub"); again.Code != http.StatusCreated ||
			again.Header().Get(contracts.IdempotencyReplayedHeader) == "" || again.Body.String() != first.Body.String() {
			t.Errorf("the token's retried create = %d %s (replayed %q), want its 201 replayed",
				again.Code, again.Body.String(), again.Header().Get(contracts.IdempotencyReplayedHeader))
		}
	})

	t.Run("a create in a project the retrying credential does not see", func(t *testing.T) {
		const title = "Filed by carol under a key"
		create := map[string]any{"project": seedPrivate, "title": title, "force": true, "actor": actor}
		first := keyedSeedRequest(t, seed, callerCarol, http.MethodPost, "/api/v1/issues", create, "carol-create-priv")
		if first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), title) {
			t.Fatalf("carol's keyed create in %s = %d %s, want 201 with the issue", seedPrivate, first.Code, first.Body.String())
		}
		issueKey := decodeBody[struct {
			Key string `json:"key"`
		}](t, first).Key
		if again := keyedSeedRequest(t, seed, callerCarol, http.MethodPost, "/api/v1/issues", create, "carol-create-priv"); again.Code != http.StatusCreated ||
			again.Header().Get(contracts.IdempotencyReplayedHeader) == "" || again.Body.String() != first.Body.String() {
			t.Errorf("carol's retried create = %d %s (replayed %q), want its 201 replayed",
				again.Code, again.Body.String(), again.Header().Get(contracts.IdempotencyReplayedHeader))
		}
		wantKeyedRefusal(t, "the retry by carol's token, which does not see "+seedPrivate,
			keyedSeedRequest(t, seed, callerCarolToken, http.MethodPost, "/api/v1/issues", create, "carol-create-priv"), issueKey)
		var made int
		if err := seed.database.Pool.QueryRow(t.Context(), `select count(*) from issues where title = $1`, title).Scan(&made); err != nil || made != 1 {
			t.Errorf("issues titled %q = %d (%v), want the one the first request made", title, made, err)
		}
	})

	t.Run("a project create", func(t *testing.T) {
		create := map[string]any{"key": "SECR", "name": "The secret project", "actor": actor}
		first := keyedSeedRequest(t, seed, callerCarolToken, http.MethodPost, "/api/v1/projects", create, "token-creates-secr")
		if first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), `"SECR"`) {
			t.Fatalf("the token's keyed create of SECR = %d %s, want 201 with the project", first.Code, first.Body.String())
		}
		if _, err := seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'private' where key = 'SECR'`); err != nil {
			t.Fatalf("make SECR private: %v", err)
		}
		wantKeyedRefusal(t, "the token's retry once SECR is private",
			keyedSeedRequest(t, seed, callerCarolToken, http.MethodPost, "/api/v1/projects", create, "token-creates-secr"), `"name":"The secret project"`)
		var projects int
		if err := seed.database.Pool.QueryRow(t.Context(), `select count(*) from projects where key = 'SECR'`).Scan(&projects); err != nil || projects != 1 {
			t.Errorf("projects named SECR = %d (%v), want the one the first request made", projects, err)
		}
	})
}
