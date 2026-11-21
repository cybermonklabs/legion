package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestEveryPathKeyedBehaviourHoldsOnV2 runs, through its /api/v2 route, each behaviour server code
// once keyed on a /api/v1 route path (R10 of dispatch://INFRA-4071/spec), where no test of a route
// group already proves it. Those that one does:
//
//   - an issue document read on an issue path, not a project's:
//     TestEachV2ArtifactListAnswersTheDocumentItsV1SlugReadAnswers, beside the v1 slug reads whose
//     owner is the row's DocumentOwner;
//   - the session check of an agent mark, which /api/v2 holds as the person scope its row needs:
//     TestV2DeclaredAuthority, "an agent mark by a broker token";
//   - the replay of a keyed message, comment and broadcast create: TestV2RouteTour's replays, every
//     write step sent twice under one key, each of those rows' Replay among them;
//   - the agent token's KEY_COMMITTED and the upload's own body limit: TestV2Retries, "an agent
//     token's mint" and "a keyed upload above the JSON limit";
//   - the comment list's kept answer read through /api/v1 and then /api/v2:
//     TestTheIssueCommentListAnswersEachVersionItsOwnShape;
//   - the no-route hint: TestAuthAndOriginRefusalsCarryACode in the routes package.
func TestEveryPathKeyedBehaviourHoldsOnV2(t *testing.T) {
	tour := newRouteTour(t)

	t.Run("an upload to a closed issue is refused before its body is read", func(t *testing.T) {
		// A body that is no JSON object: the route refuses it 400 once it reads it, so a 409
		// ISSUE_CLOSED for it is an answer made before the body was read.
		const unreadable = `{"name": "late.md", "content": `
		upload := func(key string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, "/api/v2/issues/"+key+"/artifacts", strings.NewReader(unreadable))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Dispatch-User", "alice")
			request.Header.Set(contracts.IdempotencyKeyHeader, uuid.NewString())
			response := httptest.NewRecorder()
			tour.handler.ServeHTTP(response, request)
			return response
		}
		open := tour.issue(t)
		if read := upload(open.Key); read.Code != http.StatusBadRequest {
			t.Fatalf("the unreadable upload to an open issue answered %d %s, want 400: the route reads the body", read.Code, read.Body.String())
		}
		issue := tour.issue(t)
		if closed := dispatchRequest(t, tour.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
			t.Fatalf("close %s: %d %s", issue.Key, closed.Code, closed.Body.String())
		}
		refused := upload(issue.Key)
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
			t.Fatalf("the unreadable upload to a closed issue answered %d %s, want 409 ISSUE_CLOSED, made before the body is read",
				refused.Code, refused.Body.String())
		}
	})

	t.Run("a refusal names the route of the version that answers", func(t *testing.T) {
		issue := tour.issue(t)
		for _, version := range []string{"/api/v1", "/api/v2"} {
			response := dispatchRequest(t, tour.handler, http.MethodGet, version+"/artifacts/"+issue.Document+"/asks", nil, "alice")
			code, message := refusalOf(t, response)
			if response.Code != http.StatusBadRequest || code != "ARTIFACT_LINKED" || !strings.Contains(message, version+"/issues/"+issue.Key+"/") {
				t.Errorf("%s: an issue document's asks answered %d %s, want 400 ARTIFACT_LINKED naming %s/issues/%s/",
					version, response.Code, response.Body.String(), version, issue.Key)
			}
		}
	})

	t.Run("a project-scoped write is refused on another project", func(t *testing.T) {
		caller := newBrokerCaller(t)
		const token = "a-token-scoped-to-one-project"
		tour.brokerTokens.Says(token, liveNamedToken(caller.jkt))
		present := func(method, path string, body any) *httptest.ResponseRecorder {
			t.Helper()
			response := httptest.NewRecorder()
			tour.handler.ServeHTTP(response, caller.presenting(t, method, path, token, body))
			return response
		}
		other := tour.issue(t)
		project := tour.projectOf(t, other)
		// One write of each object a /api/v2 row's ProjectOf resolves its project from, each
		// pointed at the other project's object: a refusal naming both projects is the check
		// having resolved the object, where one it resolved to nothing names the token's alone.
		for _, write := range []struct {
			resolver, method, path string
			body                   any
		}{
			{"issue", http.MethodPatch, "/api/v2/issues/" + other.Key, map[string]any{"title": "Retitled by a scoped token"}},
			{"project", http.MethodPost, "/api/v2/projects/" + project + "/artifacts", map[string]any{"name": "scoped.md", "content": "# Scoped"}},
			{"artifact", http.MethodPost, "/api/v2/artifacts/" + other.Document + "/comments", map[string]any{"body": "A scoped remark."}},
			{"ask", http.MethodPatch, "/api/v2/asks/" + tour.ask(t, other), map[string]any{"question": "Which way now?"}},
			// The comment's state, not PATCH /comments/{id}: that row needs the person scope, and
			// its gate answers a broker token OAUTH_SCOPE_REQUIRED before this check is reached.
			{"comment", http.MethodPut, "/api/v2/comments/" + tour.comment(t, other) + "/state", map[string]any{"state": "resolved"}},
			{"message", http.MethodPost, "/api/v2/messages/" + tour.targetedMessage(t, other) + "/deliveries", map[string]any{"delivery": "btw"}},
			{"result", http.MethodPut, "/api/v2/results/" + tour.result(t, other) + "/state", map[string]any{"state": "retracted", "reason": "Scoped."}},
		} {
			response := present(write.method, write.path, write.body)
			if code, message := refusalOf(t, response); response.Code != http.StatusForbidden || code != "PROJECT_SCOPE" ||
				!strings.Contains(message, brokerTestProject) || !strings.Contains(message, project) {
				t.Errorf("%s %s (its %s's project %s) under a token scoped to %s: %d %s, want 403 PROJECT_SCOPE naming both",
					write.method, write.path, write.resolver, project, brokerTestProject, response.Code, response.Body.String())
			}
		}

		if created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/projects",
			map[string]string{"key": brokerTestProject, "name": "The token's project"}, "alice"); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: %d %s", brokerTestProject, created.Code, created.Body.String())
		}
		created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues",
			map[string]string{"project": brokerTestProject, "title": "The token's own issue"}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create an issue in %s: %d %s", brokerTestProject, created.Code, created.Body.String())
		}
		own := decodeBody[tourIssue](t, created)
		if answered := present(http.MethodPatch, "/api/v2/issues/"+own.Key, map[string]any{"title": "Retitled by a scoped token"}); answered.Code != http.StatusOK {
			t.Fatalf("the same write to %s in the token's own project: %d %s, want 200", own.Key, answered.Code, answered.Body.String())
		}
	})

	t.Run("the handoff write is held to its own body limit", func(t *testing.T) {
		issue := tour.issue(t)
		route := tour.stepRoute(t, tourStep{Method: http.MethodPost, Pattern: "/issues/{key}/handoffs"})
		response := tour.send(t, route, tourRequest{
			Path: map[string]string{"key": issue.Key},
			Body: map[string]any{"phase": "plan", "body": strings.Repeat("a", int(handoffRequestLimit))},
			As:   tourBearer("agent-token"),
		}, "")
		if code, message := refusalOf(t, response); response.Code != http.StatusRequestEntityTooLarge || code != "REQUEST_TOO_LARGE" ||
			!strings.Contains(message, "a handoff body is at most") {
			t.Fatalf("a handoff past its limit answered %d %s, want 413 REQUEST_TOO_LARGE naming the handoff's bound",
				response.Code, response.Body.String())
		}
	})

	t.Run("the markdown round trip is held to its own body limit", func(t *testing.T) {
		const path = "/api/v2/markdown-round-trips"
		if above := dispatchRequest(t, tour.handler, http.MethodPost, path,
			map[string]any{"markdown": strings.Repeat("a", int(maxJSONRequestBytes))}, "alice"); above.Code != http.StatusOK {
			t.Fatalf("a text past a JSON body's bound answered %d %s, want 200 within the route's own", above.Code, above.Body.String())
		}
		past := dispatchRequest(t, tour.handler, http.MethodPost, path,
			map[string]any{"markdown": strings.Repeat("a", int(markdownRoundTripLimit))}, "alice")
		if code, _ := refusalOf(t, past); past.Code != http.StatusRequestEntityTooLarge || code != "REQUEST_TOO_LARGE" {
			t.Fatalf("a body past the route's own bound answered %d %s, want 413 REQUEST_TOO_LARGE", past.Code, past.Body.String())
		}
	})
}
