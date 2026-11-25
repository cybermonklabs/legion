package api

// v2_merge64_routes_test.go holds the /api/v2 successors of what main's #3125 and #4601 added to
// /api/v1 (merge 64 of dispatch://INFRA-4071): GET /api/v2/known-sessions, the successor of GET
// /api/v1/agents/known; the authority of GET /api/v2/agents/{session_id}/artifacts, whose list
// form keeps the person scope GET /api/v1/agents/{session_id}/artifacts needs while its slug= form
// keeps the authority of GET /api/v1/agents/{session_id}/artifacts/{slug}; and R26 (#4601) on
// PATCH /api/v2/issues/{key}, both its plain form and its status-change form.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// TestV2AgentFileListNeedsThePersonScopeAndItsSlugReadDoesNot: the deployment's shared token, which
// holds no scope, is refused the list of a conversation's files, 403 OAUTH_SCOPE_REQUIRED naming
// no file, also where slug= is given empty, and is answered the one file it names by slug, as
// /api/v1 refuses it the list and answers it the file; a person's browser is answered both.
func TestV2AgentFileListNeedsThePersonScopeAndItsSlugReadDoesNot(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedListedFile(t, database)
	path := "/api/v2/agents/" + listedSession + "/artifacts"
	shared := func(query url.Values) *httptest.ResponseRecorder {
		target := path
		if len(query) > 0 {
			target += "?" + query.Encode()
		}
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer agent-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	for name, query := range map[string]url.Values{"no slug": nil, "an empty slug": {"slug": {""}}} {
		refused := shared(query)
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusForbidden || code != apicode.OAuthScopeRequired.Name() {
			t.Fatalf("the shared token's list with %s: status=%d body=%s, want 403 %s",
				name, refused.Code, refused.Body.String(), apicode.OAuthScopeRequired.Name())
		}
		if strings.Contains(refused.Body.String(), listedFile) {
			t.Fatalf("the shared token's list with %s was refused and named the file: %s", name, refused.Body.String())
		}
	}

	one := shared(url.Values{"slug": {"photo-png"}})
	if one.Code != http.StatusOK {
		t.Fatalf("the shared token's read of one file by its slug: status=%d body=%s, want 200", one.Code, one.Body.String())
	}
	if got := decodeBody[page[artifactV2]](t, one).Items; len(got) != 1 || got[0].Name != listedFile {
		t.Fatalf("the shared token's read of one file by its slug answers %+v, want %s alone", got, listedFile)
	}
	if v1 := dispatchRequestBearer(t, handler, "/api/v1/agents/"+listedSession+"/artifacts/photo-png"); v1.Code != http.StatusOK {
		t.Fatalf("the shared token's /api/v1 read of the file: status=%d body=%s, want 200", v1.Code, v1.Body.String())
	}

	for _, target := range []string{path, path + "?slug=photo-png"} {
		admitted := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if got := decodeBody[page[artifactV2]](t, admitted).Items; admitted.Code != http.StatusOK || len(got) != 1 || got[0].Name != listedFile {
			t.Fatalf("the person's browser reads %s: status=%d body=%s, want 200 with %s alone",
				target, admitted.Code, admitted.Body.String(), listedFile)
		}
	}
}

// dispatchRequestBearer is a GET of target under the deployment's shared token.
func dispatchRequestBearer(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// TestV2KnownSessionsAnswersWhatTheV1RouteAnswers: GET /api/v2/known-sessions answers, as one page
// of the list envelope, the rows GET /api/v1/agents/known answers for the same ids (a session that
// holds a file of its conversation, and one nothing names), refuses the same id that is no session
// id 400 INVALID_QUERY, and refuses the shared token, which holds no person scope, 403
// OAUTH_SCOPE_REQUIRED, as /api/v1 does.
func TestV2KnownSessionsAnswersWhatTheV1RouteAnswers(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedListedFile(t, database)
	ids := url.Values{"id": {"s-nobody-recorded", listedSession}}

	v1 := dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/known?"+ids.Encode(), nil, "alice")
	if v1.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/agents/known: status=%d body=%s", v1.Code, v1.Body.String())
	}
	v2 := dispatchRequest(t, handler, http.MethodGet, "/api/v2/known-sessions?"+ids.Encode(), nil, "alice")
	if v2.Code != http.StatusOK {
		t.Fatalf("GET /api/v2/known-sessions: status=%d body=%s", v2.Code, v2.Body.String())
	}
	want, got := decodeBody[knownSessions](t, v1).Sessions, decodeBody[page[knownSession]](t, v2)
	if len(want) != 1 || want[0].SessionID != listedSession {
		t.Fatalf("GET /api/v1/agents/known answers %+v, want %s alone", want, listedSession)
	}
	if len(got.Items) != len(want) || got.Items[0].SessionID != want[0].SessionID || got.Total != len(want) {
		t.Fatalf("GET /api/v2/known-sessions answers %+v, want the rows /api/v1 answers, %+v", got, want)
	}

	for _, target := range []string{"/api/v1/agents/known?id=no%20session", "/api/v2/known-sessions?id=no%20session"} {
		refused := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusBadRequest || code != apicode.InvalidQuery.Name() {
			t.Fatalf("GET %s: status=%d body=%s, want 400 INVALID_QUERY", target, refused.Code, refused.Body.String())
		}
	}
	for _, target := range []string{"/api/v1/agents/known?" + ids.Encode(), "/api/v2/known-sessions?" + ids.Encode()} {
		refused := dispatchRequestBearer(t, handler, target)
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusForbidden || code != apicode.OAuthScopeRequired.Name() {
			t.Fatalf("GET %s under the shared token: status=%d body=%s, want 403 OAUTH_SCOPE_REQUIRED",
				target, refused.Code, refused.Body.String())
		}
	}
}

// TestV2PatchHoldsALegionIssuesStatusToTheLegionStatusScope: with DISPATCH_LEGION_STATUS_SCOPE on,
// PATCH /api/v2/issues/{key} refuses a status write of a Legion issue from each credential of the
// inventory that holds neither the person scope nor legion-status, 403
// LEGION_STATUS_SCOPE_REQUIRED, in its plain form and in its status-change form, and leaves the
// issue as it was; it takes the same write from each credential that holds one of the two, as
// PATCH /api/v1/issues/{key} and POST /api/v1/issues/{key}/status-change do (R26, #4601).
func TestV2PatchHoldsALegionIssuesStatusToTheLegionStatusScope(t *testing.T) {
	t.Parallel()
	rig := newLegionStatusRig(t, true)
	forms := map[string]map[string]any{
		"the plain form":         {"status": "todo"},
		"the status-change form": {"status": "todo", "include_parent": true},
	}
	for form, body := range forms {
		t.Run(form, func(t *testing.T) {
			for _, credential := range rig.credentials {
				if credential.verdict == legionRefusedFirst {
					continue
				}
				t.Run(credential.name, func(t *testing.T) {
					key := rig.issue(t, []string{"legion"}, "", "")
					before := readLegionIssue(t, rig.database, key)
					response := credential.send(t, http.MethodPatch, "/api/v2/issues/"+key, body)
					if credential.verdict == legionRefused {
						requireRefused(t, response, http.StatusForbidden, apicode.LegionStatusScopeRequired.Name())
						requireLegionIssueUnchanged(t, rig.database, key, before)
						return
					}
					if response.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s, want 200: the credential holds person or legion-status", response.Code, response.Body.String())
					}
					if after := readLegionIssue(t, rig.database, key); after.status != "todo" {
						t.Fatalf("the taken write left %s, want status todo", after)
					}
				})
			}
		})
	}
}
