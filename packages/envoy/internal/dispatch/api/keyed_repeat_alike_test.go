package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// recordedKeyedRepeat sends method target with body as caller under key, on a context the
// recorder watches, and answers the response and the statements the request made.
func recordedKeyedRepeat(
	t *testing.T, seed privateSeed, recorder *statementRecorder, caller privateCaller, method, target string, body any, key string,
) (*httptest.ResponseRecorder, []recordedStatement) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the body of %s %s: %v", method, target, err)
	}
	var response *httptest.ResponseRecorder
	statements := recorder.record(func() {
		request := httptest.NewRequestWithContext(context.WithValue(t.Context(), recordingKey{}, true), method, target, bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		if login := caller.login(); login != "" {
			request.Header.Set("X-Dispatch-User", login)
		} else {
			request.Header.Set("Authorization", "Bearer "+seed.bearer(t, caller))
		}
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		response = httptest.NewRecorder()
		seed.handler.ServeHTTP(response, request)
	})
	return response, statements
}

// TestAHiddenAndAMissingObjectCostARepeatTheSameStatements: a keyed repeat whose caller does not
// see the object its address names reads that object as the server and as the caller whether the
// object exists or not, so the number of statements it makes, like its answer, tells no caller
// whether an object it does not see exists. carol renames PRIV's issue and NOPE-1, an issue nobody
// holds, at her browser, each under a key, with a body the route refuses before it reads the
// issue; her token that lists no project repeats each, and the two repeats make as many
// statements, and are answered alike but for the key and the path.
//
// Control: the caller's read made only when the server found the object, as it was, under which
// the repeat on PRIV's issue makes one statement more.
func TestAHiddenAndAMissingObjectCostARepeatTheSameStatements(t *testing.T) {
	t.Parallel()
	recorder := &statementRecorder{}
	seed := seedPrivateProjectWith(t, testServerOptions{tracer: recorder})
	refused := map[string]any{"title": 5, "actor": carolsSession}
	for _, issue := range []string{seed.issue[seedPrivate], "NOPE-1"} {
		first := keyedSeedRequest(t, seed, callerCarol, http.MethodPatch, "/api/v1/issues/"+issue, refused, "cost-"+issue)
		if first.Code != http.StatusBadRequest {
			t.Fatalf("carol's keyed rename of %s with a number for a title = %d %s, want 400: the case proves nothing", issue, first.Code, first.Body.String())
		}
	}
	// The token is used once before the two repeats are counted, so whatever its first use writes
	// (its last use, a person's record) is in neither count.
	seed.mustRequest(t, callerCarolToken, http.MethodGet, "/api/v1/projects", nil, http.StatusOK)
	repeat := func(issue string) (int, string, int) {
		response, statements := recordedKeyedRepeat(t, seed, recorder, callerCarolToken, http.MethodPatch, "/api/v1/issues/"+issue, refused, "cost-"+issue)
		answer := strings.ReplaceAll(strings.ReplaceAll(response.Body.String(), "cost-"+issue, "<key>"), issue, "<issue>")
		return response.Code, answer, len(statements)
	}
	hiddenStatus, hiddenAnswer, hiddenStatements := repeat(seed.issue[seedPrivate])
	missingStatus, missingAnswer, missingStatements := repeat("NOPE-1")
	if hiddenStatus != missingStatus || hiddenAnswer != missingAnswer {
		t.Fatalf("the token's repeat on %s, which it does not see, = %d %s; on NOPE-1, which nobody holds, = %d %s: want one answer",
			seed.issue[seedPrivate], hiddenStatus, hiddenAnswer, missingStatus, missingAnswer)
	}
	if hiddenStatements == 0 || hiddenStatements != missingStatements {
		t.Errorf("the token's repeat on %s made %d statements and on NOPE-1 %d, want as many: the count tells a hidden issue from a missing one",
			seed.issue[seedPrivate], hiddenStatements, missingStatements)
	}
	t.Logf("measured: the repeat on %s made %d statements, on NOPE-1 %d", seed.issue[seedPrivate], hiddenStatements, missingStatements)
}

// TestADeletedObjectsRepeatIsAnsweredAsAHiddenOnes: a keyed repeat whose first request landed is
// answered alike, to a caller that does not see every object, whether its object is one that caller
// does not see or one deleted since: 409 KEY_COMMITTED, in the words a write that landed and whose
// answer the key does not replay gets, so the answer tells no caller "deleted" from "hidden".
// carol makes two documents of PRIV's issue and renames each at her browser under a key; she
// removes the first. Her token that lists no project repeats both renames, and the two answers
// are one but for the key and the path.
//
// Control: the deleted document's stored 200 replayed, as it was, under which the two answers
// differ.
func TestADeletedObjectsRepeatIsAnsweredAsAHiddenOnes(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	issue := seed.issue[seedPrivate]
	documents := map[string]string{}
	for _, name := range []string{"gone", "hidden"} {
		made := seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{"name": name + ".md"}, http.StatusCreated)
		documents[name] = decodeBody[uploadedArtifact](t, made).Artifact.ID
		rename := map[string]any{"name": "Renamed " + name, "actor": carolsSession}
		if renamed := keyedSeedRequest(t, seed, callerCarol, http.MethodPatch, "/api/v1/artifacts/"+documents[name], rename, "rename-"+name); renamed.Code != http.StatusOK {
			t.Fatalf("carol's keyed rename of %s = %d %s, want 200", name, renamed.Code, renamed.Body.String())
		}
	}
	seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/artifacts/"+documents["gone"], nil, http.StatusNoContent)
	repeat := func(name string) (int, string) {
		response := keyedSeedRequest(t, seed, callerCarolToken, http.MethodPatch, "/api/v1/artifacts/"+documents[name],
			map[string]any{"name": "Renamed " + name, "actor": carolsSession}, "rename-"+name)
		return response.Code, strings.ReplaceAll(strings.ReplaceAll(response.Body.String(), "rename-"+name, "<key>"), documents[name], "<document>")
	}
	goneStatus, gone := repeat("gone")
	hiddenStatus, hidden := repeat("hidden")
	if hiddenStatus != http.StatusConflict || !strings.Contains(hidden, contracts.IdempotencyKeyCommitted) {
		t.Fatalf("the token's repeat on the hidden document = %d %s, want 409 %s: the comparison proves nothing", hiddenStatus, hidden, contracts.IdempotencyKeyCommitted)
	}
	if goneStatus != hiddenStatus || gone != hidden {
		t.Errorf("the token's repeat on the deleted document = %d %s; on the hidden one = %d %s: want one answer", goneStatus, gone, hiddenStatus, hidden)
	}
}

// TestALostAnswerAndAHiddenOneAreRefusedInOneText: 409 KEY_COMMITTED is given to a repeat whose
// first request landed and lost its answer (writeKeyCommitted), and to a repeat whose caller may
// not read the answer the key holds (heldAnswerRefusal), in one text (keyCommittedRefusal), so the
// answer tells no caller that an object it does not see holds an answer. carol renames PRIV's
// issue at her browser under two keys; the first key's answer is taken away as a lost one is - no
// answer kept, the write committed, the holder gone - and carol's repeat of it is refused; her
// token that lists no project repeats the second, and is refused in the same words but for the key.
//
// Control: the two refusals in the two texts they had, under which the answers differ.
func TestALostAnswerAndAHiddenOneAreRefusedInOneText(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	path := "/api/v1/issues/" + seed.issue[seedPrivate]
	rename := map[string]any{"title": "Renamed under a key", "actor": carolsSession}
	for _, key := range []string{"lost-answer", "hidden-answer"} {
		if first := keyedSeedRequest(t, seed, callerCarol, http.MethodPatch, path, rename, key); first.Code != http.StatusOK {
			t.Fatalf("carol's keyed rename under %s = %d %s, want 200", key, first.Code, first.Body.String())
		}
	}
	if tag, err := seed.database.Pool.Exec(t.Context(), `
		update request_keys set status = null, headers = null, response = null,
		       committed_at = now(), leased_at = now() - interval '2 minutes'
		where idempotency_key = 'lost-answer'`); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("take the first key's answer away as a lost one: %v (%d rows)", err, tag.RowsAffected())
	}
	answer := func(caller privateCaller, key string) (int, string) {
		response := keyedSeedRequest(t, seed, caller, http.MethodPatch, path, rename, key)
		return response.Code, strings.ReplaceAll(response.Body.String(), key, "<key>")
	}
	lostStatus, lost := answer(callerCarol, "lost-answer")
	if lostStatus != http.StatusConflict || !strings.Contains(lost, contracts.IdempotencyKeyCommitted) {
		t.Fatalf("carol's repeat of the write that lost its answer = %d %s, want 409 %s: the comparison proves nothing",
			lostStatus, lost, contracts.IdempotencyKeyCommitted)
	}
	hiddenStatus, hidden := answer(callerCarolToken, "hidden-answer")
	if hiddenStatus != lostStatus || hidden != lost {
		t.Errorf("the token's hidden repeat = %d %s; the lost answer's = %d %s: want one text", hiddenStatus, hidden, lostStatus, lost)
	}
}
