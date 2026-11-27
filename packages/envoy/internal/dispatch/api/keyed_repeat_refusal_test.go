package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// keyedAnswerOf is a keyed answer's code and the top-level members of its body, for a test that
// reads what a refusal names.
func keyedAnswerOf(t *testing.T, response *httptest.ResponseRecorder) (code string, members map[string]json.RawMessage) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), &members); err != nil {
		t.Fatalf("decode the answer %d %s: %v", response.Code, response.Body.String(), err)
	}
	if raw, ok := members["code"]; ok {
		if err := json.Unmarshal(raw, &code); err != nil {
			t.Fatalf("decode the answer's code %s: %v", raw, err)
		}
	}
	return code, members
}

// TestAHiddenRepeatOfARefusedWriteIsAnsweredItsRefusalNamingNothing: a keyed repeat whose caller
// does not see the object its first request addressed is answered by what that request did. A
// first request that landed is answered 409 KEY_COMMITTED, which a client reads as a write that
// landed. A first request that was refused wrote nothing, so it is answered its refusal again -
// its status and its code, marked replayed - with a message that names nothing of the object, and
// never KEY_COMMITTED: a refused write read as landed would tell the client's session a write
// happened that did not.
//
// carol, who sees PRIV, renames PRIV's issue at her browser under a key with an expected_sequence
// the issue has moved past, and is refused 409 ISSUE_CHANGED; her token that lists no project,
// which does not see PRIV, repeats it and is answered 409 ISSUE_CHANGED, replayed, holding a code
// and a message alone. Then she renames it under another key and is answered 200; the token's
// repeat of that is 409 KEY_COMMITTED.
//
// Control: every hidden repeat answered 409 KEY_COMMITTED, as it was, under which the refused
// arm's repeat says the write was made.
func TestAHiddenRepeatOfARefusedWriteIsAnsweredItsRefusalNamingNothing(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	issue := seed.issue[seedPrivate]
	path := "/api/v1/issues/" + issue

	t.Run("a refused first request", func(t *testing.T) {
		stale := map[string]any{"title": "Renamed from a stale read", "expected_sequence": 999, "actor": carolsSession}
		first := keyedSeedRequest(t, seed, callerCarol, http.MethodPatch, path, stale, "refused-rename")
		if code, _ := keyedAnswerOf(t, first); first.Code != http.StatusConflict || code != "ISSUE_CHANGED" {
			t.Fatalf("carol's keyed rename from a stale read = %d %s, want 409 ISSUE_CHANGED: the case proves nothing", first.Code, first.Body.String())
		}
		repeat := keyedSeedRequest(t, seed, callerCarolToken, http.MethodPatch, path, stale, "refused-rename")
		code, members := keyedAnswerOf(t, repeat)
		if repeat.Code != http.StatusConflict || code != "ISSUE_CHANGED" || repeat.Header().Get(contracts.IdempotencyReplayedHeader) == "" {
			t.Fatalf("the token's repeat of the refused rename = %d %s (replayed %q), want its 409 ISSUE_CHANGED replayed",
				repeat.Code, repeat.Body.String(), repeat.Header().Get(contracts.IdempotencyReplayedHeader))
		}
		var message string
		_ = json.Unmarshal(members["error"], &message)
		if len(members) != 2 || strings.Contains(message, seedPrivate) || strings.Contains(message, "made") {
			t.Errorf("the token's replayed refusal = %s, want its code and a message that names nothing and says nothing was made", repeat.Body.String())
		}
	})

	t.Run("a first request that landed", func(t *testing.T) {
		rename := map[string]any{"title": "Renamed and landed", "actor": carolsSession}
		if first := keyedSeedRequest(t, seed, callerCarol, http.MethodPatch, path, rename, "landed-rename"); first.Code != http.StatusOK {
			t.Fatalf("carol's keyed rename = %d %s, want 200", first.Code, first.Body.String())
		}
		wantKeyedRefusal(t, "the token's repeat of the landed rename",
			keyedSeedRequest(t, seed, callerCarolToken, http.MethodPatch, path, rename, "landed-rename"), `"title":"Renamed and landed"`)
	})
}

// TestARefusedRepeatAnswersAHiddenProjectAsAMissingOne: a repeat whose first request was refused
// is answered alike whether the project it names is one its caller does not see or one that does
// not exist, so a repeat tells no caller whether a project it cannot see exists. carol creates an
// issue with no title in PRIV and in NOPE, a project nobody holds, each under a key, and each is
// refused 400 INVALID_ISSUE before the project is read, in the same words; her token that lists no
// project repeats each under its key, and the two answers are one answer but for the key they
// echo.
//
// Control: a hidden repeat answered 409 KEY_COMMITTED, as it was, under which PRIV's repeat is a
// 409 and NOPE's the replayed 400.
func TestARefusedRepeatAnswersAHiddenProjectAsAMissingOne(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	untitled := func(project string) map[string]any {
		return map[string]any{"project": project, "title": "", "force": true, "actor": carolsSession}
	}
	for _, project := range []string{seedPrivate, "NOPE"} {
		first := keyedSeedRequest(t, seed, callerCarol, http.MethodPost, "/api/v1/issues", untitled(project), "untitled-"+project)
		if code, _ := keyedAnswerOf(t, first); first.Code != http.StatusBadRequest || code != "INVALID_ISSUE" {
			t.Fatalf("carol's keyed create with no title in %s = %d %s, want 400 INVALID_ISSUE: the case proves nothing", project, first.Code, first.Body.String())
		}
	}
	answer := func(project string) (int, string) {
		repeat := keyedSeedRequest(t, seed, callerCarolToken, http.MethodPost, "/api/v1/issues", untitled(project), "untitled-"+project)
		return repeat.Code, strings.ReplaceAll(repeat.Body.String(), "untitled-"+project, "<key>")
	}
	hiddenStatus, hidden := answer(seedPrivate)
	missingStatus, missing := answer("NOPE")
	if hiddenStatus != missingStatus || hidden != missing {
		t.Errorf("the token's repeat in %s, a project it does not see, = %d %s; in NOPE, which nobody holds, = %d %s: want one answer",
			seedPrivate, hiddenStatus, hidden, missingStatus, missing)
	}
	if hiddenStatus != http.StatusBadRequest {
		t.Errorf("the token's repeat of a refused create = %d %s, want the 400 it was refused", hiddenStatus, hidden)
	}
}

// TestAStoredStatusChangeIsNotReplayedToACallerThatDoesNotSeeAnIssueItChanged: a status change's
// stored answer names every issue its steps changed (keyedRoute.pointIssues), so a repeat is
// answered from the key only to a caller that sees each of them, not only the issue its address
// names. carol closes PUB's issue at her browser under a key with include_children, which closes
// its child in PRIV too; her token that lists no project sees PUB's issue and not the child, and
// its repeat is refused 409 KEY_COMMITTED, naming neither; carol's own repeat is replayed with
// the child among the issues it changed.
//
// Control: the stored row judged by its addressed issue alone, under which the token is replayed
// the child.
func TestAStoredStatusChangeIsNotReplayedToACallerThatDoesNotSeeAnIssueItChanged(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	parent := seed.issue[seedPublic]
	created := seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": seedPrivate, "title": "A child of the public issue", "parent": parent, "force": true,
	}, http.StatusCreated)
	child := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	path := "/api/v1/issues/" + parent + "/status-change"
	closing := map[string]any{"status": "done", "include_children": true, "actor": carolsSession}
	first := keyedSeedRequest(t, seed, callerCarol, http.MethodPost, path, closing, "close-with-children")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), child) {
		t.Fatalf("carol's keyed close of %s with its children = %d %s, want 200 naming the child %s: the case proves nothing",
			parent, first.Code, first.Body.String(), child)
	}
	if again := keyedSeedRequest(t, seed, callerCarol, http.MethodPost, path, closing, "close-with-children"); again.Code != http.StatusOK ||
		!strings.Contains(again.Body.String(), child) {
		t.Fatalf("carol's repeat = %d %s, want the close replayed naming %s: the comparison proves nothing", again.Code, again.Body.String(), child)
	}
	wantKeyedRefusal(t, "the token's repeat, which sees "+parent+" and not "+child,
		keyedSeedRequest(t, seed, callerCarolToken, http.MethodPost, path, closing, "close-with-children"), child)
}

// TestAReusedKeyNamesADocumentOnlyToACallerWhoSeesIt: 422 KEY_REUSED names the request that used
// the key only to a caller that sees what its path names, for a document as for an issue
// (heldRequestNamed reads the path's object as the project check does). carol renames PRIV's
// issue's document at her browser under a key; a different request under the key is told its
// method and path when carol sends it, and neither when her token that lists no project does.
//
// Control: KEY_REUSED naming the first request to every caller, under which the token is told the
// document's path.
func TestAReusedKeyNamesADocumentOnlyToACallerWhoSeesIt(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	read := seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/issues/"+seed.issue[seedPrivate], nil, http.StatusOK)
	document := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, read).PrimaryArtifactID
	if document == "" {
		t.Fatalf("%s's issue holds no document: the case proves nothing", seedPrivate)
	}
	path := "/api/v1/artifacts/" + document
	keyedSeedRequest(t, seed, callerCarol, http.MethodPatch, path, map[string]any{"name": "Renamed spec", "actor": carolsSession}, "document-key")
	reused := func(caller privateCaller) string {
		response := keyedSeedRequest(t, seed, caller, http.MethodPatch, path, map[string]any{"name": "Another name", "actor": carolsSession}, "document-key")
		code, message := refusalOf(t, response)
		if response.Code != http.StatusUnprocessableEntity || code != contracts.IdempotencyKeyReused {
			t.Fatalf("%s's other request under the key = %d %s, want 422 %s", caller, response.Code, response.Body.String(), contracts.IdempotencyKeyReused)
		}
		return message
	}
	if message := reused(callerCarol); !strings.Contains(message, http.MethodPatch+" "+path) {
		t.Fatalf("carol's reuse answered %q, want it naming %s %s: the comparison proves nothing", message, http.MethodPatch, path)
	}
	if message := reused(callerCarolToken); strings.Contains(message, document) {
		t.Errorf("the token's reuse answered %q, want it naming nothing of the document", message)
	}
}

// TestARecoveryPointIsReadForACallerWhoseRequestHasEnded: the keyed layer's answers to a repeat
// whose body stopped arriving run whatever the client does (writeUnjudgedKeyInFlight), and so does
// the read of whether its caller sees the issues a recovery point names (recoveryPointFor), which
// net/http's cancellation of the request's context does not end. carol sees PRIV's issue; a point
// naming it is handed to her through a request whose context has ended.
//
// Control: the read made on the request's own context, as it was, under which it fails with the
// context's error.
func TestARecoveryPointIsReadForACallerWhoseRequestHasEnded(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	route := seed.server.keyedReplays()[statusChangeRoute]
	recovery := json.RawMessage(`{"changed":[{"key":"` + seed.issue[seedPrivate] + `","status":"done"}]}`)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequestWithContext(ended, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/status-change", nil)
	request.Header.Set("X-Dispatch-User", callerCarol.login())
	point, err := seed.server.recoveryPointFor(request, route, recovery)
	if err != nil {
		t.Fatalf("the recovery point read for carol on an ended request: %v, want it read", err)
	}
	if !slices.Equal(point, recovery) {
		t.Errorf("carol, who sees %s, was handed the point %s, want %s", seed.issue[seedPrivate], point, recovery)
	}
}
