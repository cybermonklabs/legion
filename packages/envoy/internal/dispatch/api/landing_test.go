package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The landing composes its answer out of the loaders the per-resource routes call, so each part
// of it must be exactly what the route it stands in for answers. This walk reads one issue
// landing and then reads every route behind it, and compares the two bodies part by part: a part
// that goes missing, or that drifts from its route, fails here rather than drawing a page that
// disagrees with the page the same dashboard draws from the routes themselves.
//
// The fake listener answers the interests and sessions reads, so the subscribers and the agents
// parts are lists on both sides rather than the 503 a deployment with no listener gives. The
// documents never settle during the test (settle), so the spec's versions, text and blocks are
// the ones this test's own requests wrote and cannot change between the landing read and the
// per-resource reads.
func TestLandingAnswersWhatThePerResourceRoutesAnswer(t *testing.T) {
	listenerURL, _ := newFakeListener(t,
		map[string][]string{"ses-1": {"notifications.dispatch.>"}},
		[]map[string]any{{"session_id": "ses-1", "title": "planner", "machine_id": "box-1", "last_seen": 100}})
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour, envoyURL: listenerURL})
	createTestProject(t, handler, "LAND")
	// A second project with a source of its own: the issue page's sources are the sources of the
	// issue's own project, so a landing that read any other project's would answer this row here
	// and fail against what the route answers.
	createTestProject(t, handler, "OTHER")
	addArchitectureSource(t, database, "LAND", "legion/land")
	addArchitectureSource(t, database, "OTHER", "legion/other")
	mapRepoToProject(t, handler, "acme/widgets", "LAND")
	// The spec marks two requirements, so the coverage part carries requirements and gap lines
	// rather than the one line a spec that marks none earns.
	issue := createSpecIssue(t, handler, "LAND", "# Spec\n\nThe landing reads this document.\n\n"+twoRequirements)
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "a comment the landing carries"}, "alice")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks",
		map[string]any{"question": "Does the landing carry this ask?"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state",
		map[string]any{"pinned": true}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("pin issue: status=%d body=%s", response.Code, response.Body.String())
	}

	landing := readLanding(t, handler, "/api/v1/landing?page=issue&key="+issue.Key)

	// The viewer: GET /auth/whoami is the routes package's row and is not mounted on this mux,
	// so its two stable fields are checked by value and the head is checked for the one property
	// the page needs of it, that it is an event id the feed can resume from.
	var viewer landingViewer
	if err := json.Unmarshal(landing.Viewer, &viewer); err != nil {
		t.Fatalf("decode the landing's viewer %s: %v", landing.Viewer, err)
	}
	if viewer.Kind != "user" || viewer.Login != "alice" {
		t.Errorf("the landing's viewer = %+v, want kind user and login alice", viewer)
	}
	if head, err := strconv.ParseInt(viewer.EventHead, 10, 64); err != nil || head < 0 {
		t.Errorf("the landing's event_head = %q, want the decimal id the event feed resumes from", viewer.EventHead)
	}

	spec := landing.issueDocument(t)
	for _, part := range []struct {
		part  string
		body  json.RawMessage
		route string
	}{
		{"shell.inbox", landing.Shell["inbox"], "/api/v1/inbox"},
		{"shell.pinned_issues", landing.Shell["pinned_issues"], "/api/v1/issues?pinned=true"},
		{"shell.projects", landing.Shell["projects"], "/api/v1/projects"},
		{"shell.agent_states", landing.Shell["agent_states"], "/api/v1/me/agents/state"},
		{"shell.credential_requests", landing.Shell["credential_requests"], "/api/v1/credential-requests?approver=me"},
		{"shell.agents", landing.Shell["agents"], "/api/v1/agents"},
		{"page.issue.issue", landing.Issue["issue"], "/api/v1/issues/" + issue.Key},
		{"page.issue.user_state", landing.Issue["user_state"], "/api/v1/me/state"},
		{"page.issue.comments", landing.Issue["comments"], "/api/v1/issues/" + issue.Key + "/comments"},
		{"page.issue.asks", landing.Issue["asks"], "/api/v1/issues/" + issue.Key + "/asks?state=all&thread=1"},
		{"page.issue.subscribers", landing.Issue["subscribers"], "/api/v1/issues/" + issue.Key + "/subscribers"},
		{"page.issue.decisions", landing.Issue["decisions"], "/api/v1/issues/" + issue.Key + "/decisions"},
		{"page.issue.coverage", landing.Issue["coverage"], "/api/v1/issues/" + issue.Key + "/coverage"},
		{"page.issue.repo_projects", landing.Issue["repo_projects"], "/api/v1/settings/repo-projects"},
		{"page.issue.architecture_sources", landing.Issue["architecture_sources"], "/api/v1/projects/LAND/architecture-sources"},
		{"page.issue.document.artifact", spec["artifact"], "/api/v1/artifacts/" + issue.PrimaryArtifactID},
		{"page.issue.document.text", spec["text"], "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/text"},
		{"page.issue.document.blocks", spec["blocks"], "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/blocks"},
		{"page.issue.document.read", spec["read"], "/api/v1/me/documents/" + issue.PrimaryArtifactID + "/read"},
	} {
		assertSameJSON(t, part.part, part.body, readRouteBody(t, handler, part.route))
	}

	// The unread-reply count is one field of one route's answer, not a whole body: the landing
	// carries the total of a one-row read, which is the number the Inbox badge shows.
	unread := decodeBody[struct {
		Total int `json:"total"`
	}](t, dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/me/replies?kind=reply,agent&limit=1", nil, "alice"))
	var carried int
	if err := json.Unmarshal(landing.Shell["unread_replies"], &carried); err != nil {
		t.Fatalf("decode the landing's unread_replies %s: %v", landing.Shell["unread_replies"], err)
	}
	if carried != unread.Total {
		t.Errorf("the landing's unread_replies = %d, want the %d its route counts", carried, unread.Total)
	}

	// The three pages the issue landing does not answer are null, so a client reading the wrong
	// member finds nothing rather than another page's data.
	for _, member := range []string{"board", "inbox", "document"} {
		if answered, held := landing.Page[member]; held && string(answered) != "null" {
			t.Errorf("an issue landing's page.%s = %s, want null", member, answered)
		}
	}
}

// Every other page of the landing answers its own member and leaves the issue's null, and each
// member carries what that page's own routes answer.
func TestLandingBoardInboxAndDocumentPagesAnswerWhatTheirRoutesAnswer(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createTestProject(t, handler, "LAND")
	addArchitectureSource(t, database, "LAND", "legion/land")
	issue := createSpecIssue(t, handler, "LAND", "# Spec\n\nAn open issue for the board.\n")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/LAND/artifacts", map[string]any{
		"name": "rollout.md", "content": "# Rollout\n\nThe document page reads this.\n",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("upload project document: status=%d body=%s", response.Code, response.Body.String())
	}
	document := decodeBody[struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/LAND/artifacts/rollout-md", nil, "alice"))
	if document.ID == "" {
		t.Fatalf("the project document has no id: %+v", document)
	}

	board := readLanding(t, handler, "/api/v1/landing?page=board&project=LAND").partOf(t, "board")
	for _, part := range []struct {
		part  string
		body  json.RawMessage
		route string
	}{
		{"page.board.issues", board["issues"], "/api/v1/issues?project=LAND&open=true"},
		{"page.board.inbox", board["inbox"], "/api/v1/inbox?project=LAND"},
		{"page.board.user_state", board["user_state"], "/api/v1/me/state"},
		{"page.board.architecture_sources", board["architecture_sources"], "/api/v1/projects/LAND/architecture-sources"},
	} {
		assertSameJSON(t, part.part, part.body, readRouteBody(t, handler, part.route))
	}
	if issue.Key == "" {
		t.Fatalf("the seeded issue has no key: %+v", issue)
	}

	inbox := readLanding(t, handler, "/api/v1/landing?page=inbox").partOf(t, "inbox")
	// Every field but as_of, which is the database time of the snapshot each read took and so is
	// a different moment on the two reads by design (listMyReplies).
	assertSameJSONExcept(t, "page.inbox.threads", inbox["threads"],
		readRouteBody(t, handler, "/api/v1/me/replies?kind=reply,agent&state=inbox"), "as_of")

	page := readLanding(t, handler, "/api/v1/landing?page=document&project=LAND&slug="+document.Slug).partOf(t, "document")
	for _, part := range []struct {
		part  string
		body  json.RawMessage
		route string
	}{
		{"page.document.artifact", page["artifact"], "/api/v1/projects/LAND/artifacts/" + document.Slug},
		{"page.document.text", page["text"], "/api/v1/artifacts/" + document.ID + "/text"},
		{"page.document.blocks", page["blocks"], "/api/v1/artifacts/" + document.ID + "/blocks"},
		{"page.document.asks", page["asks"], "/api/v1/artifacts/" + document.ID + "/asks?state=all"},
		{"page.document.comments", page["comments"], "/api/v1/artifacts/" + document.ID + "/comments"},
	} {
		assertSameJSON(t, part.part, part.body, readRouteBody(t, handler, part.route))
	}
}

// A page the route does not know is refused by name, and so is a page that needs a parameter the
// request did not carry: a landing answered empty would have the dashboard draw nothing, where a
// refusal has it read its page for itself.
func TestLandingRefusesAPageItDoesNotKnow(t *testing.T) {
	handler := newTestHandler(t)
	for _, refused := range []struct {
		name   string
		target string
	}{
		{"an unknown page", "/api/v1/landing?page=timeline"},
		{"no page at all", "/api/v1/landing"},
		{"an issue page with no key", "/api/v1/landing?page=issue"},
		{"a board with no project", "/api/v1/landing?page=board"},
		{"a document page with no slug", "/api/v1/landing?page=document&project=LAND"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodGet, refused.target, nil, "alice")
			refusal := decodeBody[struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}](t, response)
			if response.Code != http.StatusBadRequest || refusal.Code != "LANDING_INPUT" {
				t.Fatalf("GET %s = %d %s, want 400 LANDING_INPUT", refused.target, response.Code, response.Body.String())
			}
			if refusal.Error == "" {
				t.Errorf("GET %s refused with no reason to act on", refused.target)
			}
		})
	}
}

// An issue key that is no issue leaves the issue's own parts null rather than refusing the whole
// landing: the shell is still what every page draws, and the dashboard reads the issue itself,
// which answers the 404 it already draws. The decisions part is no exception to that rule: its
// own route answers an issue it has no row of with empty content rather than a refusal
// (getIssueDecisions), and the landing carries exactly that.
//
// The repository mappings are no exception either, the other way about: their route reads no
// issue at all, so the landing answers them whatever the key was.
func TestLandingLeavesAnUnknownIssueNullAndAnswersTheShell(t *testing.T) {
	handler := newTestHandler(t)
	createTestProject(t, handler, "LAND")
	mapRepoToProject(t, handler, "acme/widgets", "LAND")
	landing := readLanding(t, handler, "/api/v1/landing?page=issue&key=LAND-404")
	issue := landing.partOf(t, "issue")
	// The coverage and the sources are the issue's own: the coverage route answers 404 for a key
	// that is no issue, and the project whose sources the page draws is the one the issue names.
	for _, member := range []string{"issue", "document", "coverage", "architecture_sources"} {
		if string(issue[member]) != "null" {
			t.Errorf("an unknown issue's page.issue.%s = %s, want null", member, issue[member])
		}
	}
	assertSameJSON(t, "page.issue.decisions", issue["decisions"],
		readRouteBody(t, handler, "/api/v1/issues/LAND-404/decisions"))
	assertSameJSON(t, "page.issue.repo_projects", issue["repo_projects"],
		readRouteBody(t, handler, "/api/v1/settings/repo-projects"))
	assertSameJSON(t, "shell.projects", landing.Shell["projects"], readRouteBody(t, handler, "/api/v1/projects"))
}

// addArchitectureSource makes repo a source of project's architecture model, as GET
// /api/v1/projects/{key}/architecture-sources answers it and the landing carries it.
//
// The row is written straight to the table rather than through the route that saves one: that
// route checks the branch against the GitHub App before it writes, and these tests run against
// no GitHub. What they must prove is only that the landing carries the row the read route
// answers, and both read the same row however it was written.
func addArchitectureSource(t *testing.T, database *store.Store, project, repo string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ($1, $2, 'main', 1, '{"kind":"user","id":"alice"}'::jsonb)
	`, project, repo); err != nil {
		t.Fatalf("make %s a source of %s: %v", repo, project, err)
	}
}

// mapRepoToProject maps a repository to a project, which is one row of what GET
// /api/v1/settings/repo-projects answers.
func mapRepoToProject(t *testing.T, handler http.Handler, repo, project string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo,
		map[string]any{"project": project}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("map %s to %s: status=%d body=%s", repo, project, response.Code, response.Body.String())
	}
}

// landingBody is one landing answer read as the JSON it is, so each part is compared against its
// route's own bytes rather than through a Go type that could hide a difference in either.
type landingBody struct {
	Viewer json.RawMessage            `json:"viewer"`
	Shell  map[string]json.RawMessage `json:"shell"`
	Page   map[string]json.RawMessage `json:"page"`
	// Issue is the issue page's own members, read once for a test that walks them.
	Issue map[string]json.RawMessage `json:"-"`
}

// partOf is one page's members, and fails when the landing answered that page null.
func (b landingBody) partOf(t *testing.T, page string) map[string]json.RawMessage {
	t.Helper()
	members := map[string]json.RawMessage{}
	if err := json.Unmarshal(b.Page[page], &members); err != nil {
		t.Fatalf("decode the landing's page.%s %s: %v", page, b.Page[page], err)
	}
	return members
}

// issueDocument is the issue page's document members.
func (b landingBody) issueDocument(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	members := map[string]json.RawMessage{}
	if err := json.Unmarshal(b.Issue["document"], &members); err != nil {
		t.Fatalf("decode the landing's page.issue.document %s: %v", b.Issue["document"], err)
	}
	return members
}

// readLanding reads one landing as alice and fails unless it answered 200.
func readLanding(t *testing.T, handler http.Handler, target string) landingBody {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	body := decodeBody[landingBody](t, response)
	if issue, held := body.Page["issue"]; held && string(issue) != "null" {
		if err := json.Unmarshal(issue, &body.Issue); err != nil {
			t.Fatalf("decode the landing's page.issue %s: %v", issue, err)
		}
	}
	return body
}

// readRouteBody reads one per-resource route as alice and fails unless it answered 200.
func readRouteBody(t *testing.T, handler http.Handler, target string) []byte {
	t.Helper()
	var response *httptest.ResponseRecorder = dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

// assertSameJSON fails when a part of the landing is not the JSON value its route answered. The
// two are compared as decoded values, so a difference in whitespace or in the order the two
// encoders wrote the same object is no failure and a difference in the data always is.
func assertSameJSON(t *testing.T, part string, landing json.RawMessage, route []byte) {
	t.Helper()
	assertSameJSONExcept(t, part, landing, route)
}

// assertSameJSONExcept is assertSameJSON over every member but the named ones, which it takes
// out of both objects first. It is for a field whose value is the moment of its own read - the
// database time of a snapshot - which two reads cannot carry the same value of and which says
// nothing about whether the landing composed the right answer.
func assertSameJSONExcept(t *testing.T, part string, landing json.RawMessage, route []byte, ignored ...string) {
	t.Helper()
	var carried, answered any
	if err := json.Unmarshal(landing, &carried); err != nil {
		t.Fatalf("decode the landing's %s %s: %v", part, landing, err)
	}
	if err := json.Unmarshal(route, &answered); err != nil {
		t.Fatalf("decode the route behind %s %s: %v", part, route, err)
	}
	for _, member := range ignored {
		for _, value := range []any{carried, answered} {
			object, isObject := value.(map[string]any)
			if !isObject {
				t.Fatalf("%s: %q is no member of a JSON object", part, member)
			}
			delete(object, member)
		}
	}
	if !reflect.DeepEqual(carried, answered) {
		t.Errorf("the landing's %s is not what its route answers:\nlanding: %s\n  route: %s", part, landing, route)
	}
}
