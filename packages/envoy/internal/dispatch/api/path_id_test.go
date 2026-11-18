package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// notAnID is a path value no route's id can be: it is no uuid, no prefix of one (it holds no hex
// digit run the prefix layer reads, text.IsIDPrefix) and no integer. It is what a client sends
// when it formats a missing id, as a JavaScript client formats null.
const notAnID = "null"

// pathIDProbe is the probe path of pattern with the one wildcard name holding notAnID and every
// other wildcard the well-formed placeholder routeProbePath gives it.
func pathIDProbe(pattern, name string) string {
	pattern = strings.Replace(pattern, "{"+name+"...}", notAnID, 1)
	pattern = strings.Replace(pattern, "{"+name+"}", notAnID, 1)
	return routeProbePath(pattern)
}

// INFRA-4540: a route that sent a path id to a uuid (or integer) column without parsing it first
// answered a value that is none 500 INTERNAL, the store's "invalid input syntax" (SQLSTATE
// 22P02), which a client cannot tell from Dispatch failing. Every route of the table, the test
// hooks' included, is sent each of its wildcards in turn as notAnID, the others well formed, by a
// signed-in person and by the shared agent token, on a server holding the project TEST and its
// issue TEST-1 the placeholders name, so a route reaches its id rather than refusing its project.
// None answers 500: each refuses the value as the route refuses an id it does not hold, or one
// that is malformed. A 503 is no such failure: it is a deployment that configured no Envoy
// listener, GitHub App or agent stream relay, which the test server is.
func TestNoRouteAnswersAPathValueThatIsNoIDWithA500(t *testing.T) {
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true})
	createArtifactIssue(t, handler)
	callers := []struct {
		name   string
		header http.Header
	}{
		{"a signed-in person", asUser("alice")},
		{"the shared agent token", withBearer("agent-token")},
	}
	rows := 0
	for _, route := range readOnlyTable() {
		for _, name := range patternWildcards(route.Pattern) {
			path := pathIDProbe(route.Pattern, name)
			for _, caller := range callers {
				rows++
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				var body *strings.Reader
				if route.Method == http.MethodGet || route.Method == http.MethodDelete {
					body = strings.NewReader("")
				} else {
					body = strings.NewReader(routeProbeBody(route))
				}
				request := httptest.NewRequestWithContext(ctx, route.Method, path, body)
				request.Header.Set("Content-Type", "application/json")
				for key, values := range caller.header {
					request.Header[key] = values
				}
				answer := httptest.NewRecorder()
				handler.ServeHTTP(answer, request)
				cancel()
				if answer.Code == http.StatusInternalServerError {
					t.Errorf("%s %s, {%s} = %q, by %s: status=%d body=%.200s, want the route's own 400 or 404",
						route.Method, route.Pattern, name, notAnID, caller.name, answer.Code, answer.Body.String())
				}
			}
		}
	}
	if rows == 0 {
		t.Fatal("no route has a wildcard, so nothing was sent")
	}
	if mounted := len(served.routeIndex); mounted == 0 {
		t.Fatal("the server mounts no route")
	}
	t.Logf("sent %d requests: every wildcard of every route, by each of %d callers", rows, len(callers))
}

// The route shapes INFRA-4540's walk (TestNoRouteAnswersAPathValueThatIsNoIDWithA500) found
// answering 500, one row each, with a body each route takes, so a row reaches the lookup its
// probe body would have stopped short of: the edit's body, a whole reply. A comment route that
// writes refuses a comment id that is no uuid as the comment reads refuse one (getComment's
// requireUUIDPath), 400 COMMENT_ID_INPUT, where it sent the value to the comments table's uuid
// column (lockOwnerOfComment). The read-state write refuses an issue key no issue holds as every
// issue route refuses one, 404 NOT_FOUND, where its insert broke the row's foreign key. Each
// row's control is the same request at a value that is well formed and names nothing, whose
// answer, the route's own 404, this change leaves as it was.
func TestEachRouteShapeRefusesAPathValueThatNamesNothing(t *testing.T) {
	handler := newTestHandler(t)
	createArtifactIssue(t, handler)
	const nobody = "00000000-0000-0000-0000-000000000001"
	reply := map[string]any{"actor": map[string]string{"kind": "session", "id": "s1"}, "attempt": 1, "body": "a reply"}
	commentRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+notAnID, nil, "alice")
	readCode, readText := refusalOf(t, commentRead)
	if commentRead.Code != http.StatusBadRequest || readCode != "COMMENT_ID_INPUT" {
		t.Fatalf("GET /api/v1/comments/%s: status=%d body=%s, want the 400 COMMENT_ID_INPUT the writes are held to", notAnID, commentRead.Code, commentRead.Body.String())
	}
	for _, row := range []struct {
		method, route string
		body          any
		agent         bool
		value         string
		status        int
		code, text    string
		control       string
	}{
		{http.MethodPost, "/api/v1/comments/%s/resolve", nil, false, notAnID, http.StatusBadRequest, readCode, readText, nobody},
		{http.MethodPost, "/api/v1/comments/%s/accept", nil, false, notAnID, http.StatusBadRequest, readCode, readText, nobody},
		{http.MethodPost, "/api/v1/comments/%s/reject", nil, false, notAnID, http.StatusBadRequest, readCode, readText, nobody},
		{http.MethodPost, "/api/v1/comments/%s/reopen", nil, false, notAnID, http.StatusBadRequest, readCode, readText, nobody},
		{http.MethodPatch, "/api/v1/comments/%s", map[string]string{"body": "edited"}, false, notAnID, http.StatusBadRequest, readCode, readText, nobody},
		{http.MethodPost, "/api/v1/comments/%s/reply", reply, true, notAnID, http.StatusBadRequest, readCode, readText, nobody},
		{http.MethodPut, "/api/v1/me/issues/%s/state", map[string]bool{"pinned": true}, false, notAnID, http.StatusNotFound, "NOT_FOUND", "not found", ""},
		{http.MethodPut, "/api/v1/me/issues/%s/state", map[string]bool{"pinned": true}, false, "TEST-999", http.StatusNotFound, "NOT_FOUND", "not found", ""},
	} {
		send := func(value string) *httptest.ResponseRecorder {
			t.Helper()
			target := fmt.Sprintf(row.route, value)
			if row.agent {
				return agentRequest(t, handler, row.method, target, row.body, "agent-token")
			}
			return dispatchRequest(t, handler, row.method, target, row.body, "alice")
		}
		name := row.method + " " + fmt.Sprintf(row.route, row.value)
		answer := send(row.value)
		if code, text := refusalOf(t, answer); answer.Code != row.status || code != row.code || text != row.text {
			t.Errorf("%s: status=%d body=%s, want %d %s %q", name, answer.Code, answer.Body.String(), row.status, row.code, row.text)
		}
		if row.control == "" {
			continue
		}
		if control := send(row.control); control.Code != http.StatusNotFound {
			t.Errorf("%s, the control at %s that names nothing: status=%d body=%s, want the route's own 404", name, row.control, control.Code, control.Body.String())
		}
	}
}
