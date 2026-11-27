package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
)

// TestAKeyedRepeatReplaysNoAnswerItsCallerNoLongerSees: a key belongs to a person whatever
// credential they write through (requestKeyLogin), and a repeat under it is answered the first
// request's stored answer, so a repeat is held to what its caller sees now (heldAnswerHidden). bob,
// a restricted member PRIV's list names, renames PRIV's issue under a key and is answered 200 with
// the issue; PATCH /api/v1/issues/{key} keeps that answer's bytes and replays them. His repeat is
// replayed while he is on the list. carol takes him off PRIV's list, and his repeat under the same
// key answers exactly what the same patch under a fresh key answers - an issue nobody holds - and
// holds no part of the issue.
//
// Control: heldAnswerHidden answering false, under which the repeat replays the stored 200 with
// PRIV's issue in it, and the test fails.
func TestAKeyedRepeatReplaysNoAnswerItsCallerNoLongerSees(t *testing.T) {
	seed := seedPrivateProject(t)
	path := "/api/v1/issues/" + seed.issue[seedPrivate]
	const title = "Renamed by bob under a key"
	payload, err := json.Marshal(map[string]any{"title": title})
	if err != nil {
		t.Fatalf("encode the patch: %v", err)
	}
	keyed := func(key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", callerBob.login())
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		response := httptest.NewRecorder()
		seed.handler.ServeHTTP(response, request)
		return response
	}
	first := keyed("bob-rename-1")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), title) {
		t.Fatalf("bob's keyed rename of PRIV's issue = %d %s, want 200 with the issue", first.Code, first.Body.String())
	}
	if again := keyed("bob-rename-1"); again.Code != http.StatusOK || again.Header().Get(contracts.IdempotencyReplayedHeader) == "" ||
		again.Body.String() != first.Body.String() {
		t.Fatalf("bob's repeat while PRIV's list names him = %d %s, want the first answer replayed", again.Code, again.Body.String())
	}

	seed.removePrincipal(t, seedPrivate, access.PrincipalPerson, seedBob)
	fresh := keyed("bob-rename-2")
	if fresh.Code != http.StatusNotFound {
		t.Fatalf("bob's rename of PRIV's issue under a fresh key after he left PRIV's list = %d %s, want 404: the comparison proves nothing",
			fresh.Code, fresh.Body.String())
	}
	repeat := keyed("bob-rename-1")
	if repeat.Code != fresh.Code || repeat.Body.String() != fresh.Body.String() || strings.Contains(repeat.Body.String(), title) ||
		repeat.Header().Get(contracts.IdempotencyReplayedHeader) != "" {
		t.Errorf("bob's repeat after he left PRIV's list = %d %s (replayed %q), want what a fresh key answers, %d %s",
			repeat.Code, repeat.Body.String(), repeat.Header().Get(contracts.IdempotencyReplayedHeader), fresh.Code, fresh.Body.String())
	}
}

// TestAStatusChangeIsNotReplayedToACredentialThatDoesNotSeeWhatItsIssueNames: a status change's
// recovery point holds each step's issue as its writer's reader read it (writeIssue), and a key is
// carol's whatever credential she writes through (requestKeyLogin), so a repeat is answered from
// the point only to a caller that sees every object the point names, what its issues name
// included (statusChangePointReferences), as heldAnswerRefusal holds every replay. carol, at her
// browser, moves a PUB issue that PRIV's issue blocks under a key; her answer and her repeat's
// name PRIV's issue among its blockers. Her token that lists no project sees the PUB issue and not
// PRIV's, and its repeat under the same key is refused 409 KEY_COMMITTED, holding no part of
// PRIV's issue.
//
// Control: statusChangePointReferences naming the keys of the point's issues alone (its two
// nameIssueReferences calls taken out), under which the token's repeat is replayed carol's answer
// with PRIV's key in it, and wantKeyedRefusal fails.
func TestAStatusChangeIsNotReplayedToACredentialThatDoesNotSeeWhatItsIssueNames(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	blocker := seed.issue[seedPrivate]
	blocked := seed.issueUnder(t, seedPublic, "A PUB issue that PRIV's issue blocks", "")
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+blocked, map[string]any{"blocked_by": []string{blocker}}, http.StatusOK)
	path := "/api/v1/issues/" + blocked + "/status-change"
	body := map[string]any{"status": "todo", "actor": carolsSession}
	first := keyedSeedRequest(t, seed, callerCarol, http.MethodPost, path, body, "move-a-blocked-issue")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), blocker) {
		t.Fatalf("carol's keyed move of %s = %d %s, want 200 naming its blocker %s: the case proves nothing",
			blocked, first.Code, first.Body.String(), blocker)
	}
	if again := keyedSeedRequest(t, seed, callerCarol, http.MethodPost, path, body, "move-a-blocked-issue"); again.Code != http.StatusOK ||
		again.Header().Get(contracts.IdempotencyReplayedHeader) == "" || !strings.Contains(again.Body.String(), blocker) {
		t.Fatalf("carol's repeat = %d %s, want the move replayed naming %s: the comparison proves nothing", again.Code, again.Body.String(), blocker)
	}
	wantKeyedRefusal(t, "the token's repeat, which sees "+blocked+" and not "+blocker,
		keyedSeedRequest(t, seed, callerCarolToken, http.MethodPost, path, body, "move-a-blocked-issue"), blocker)
}
