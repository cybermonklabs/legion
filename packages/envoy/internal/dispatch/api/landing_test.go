package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
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
	t.Parallel()
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
	// A default repository, so the projects part carries the member the issue header's repository
	// control reads its suggestion from (`default_repo`), which a project with none leaves out.
	setDefaultRepository(t, handler, "LAND", "acme/widgets")
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
	// One fault of each kind, in two projects, so the priority summary counts work on both sides
	// rather than answering the sentence a deployment with no priority answers: the in-flight work
	// under no priority is OTHER's, so a summary narrowed to the issue's own project would count
	// none of it and fail against the rail's read, which narrows to no project.
	createTestPriority(t, handler, map[string]any{"id": "landing-goal", "title": "Landing goal"}, "alice")
	seedPriorityIssue(t, handler, "OTHER", "Work in flight under no priority", "in_progress", "", nil)
	seedPriorityIssue(t, handler, "LAND", "Priority work parked", "backlog", "", []string{"landing-goal"})

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
		// Null on both sides: newTestServer configures no broker. The list itself, read from a
		// broker, is TestLandingCredentialRequestsAreTheValuesTheRouteRelays' row.
		{"shell.credential_requests", landing.Shell["credential_requests"], "/api/v1/credential-requests?approver=me"},
		{"shell.priority_summary", landing.Shell["priority_summary"], "/api/v1/priorities?summary=true"},
		{"shell.agents", landing.Shell["agents"], "/api/v1/agents"},
		{"page.issue.issue", landing.Issue["issue"], "/api/v1/issues/" + issue.Key},
		{"page.issue.user_state", landing.Issue["user_state"], "/api/v1/me/state"},
		{"page.issue.comments", landing.Issue["comments"], "/api/v1/issues/" + issue.Key + "/comments"},
		// With no thread: the reader the dashboard seeds this part for asks for none.
		{"page.issue.asks", landing.Issue["asks"], "/api/v1/issues/" + issue.Key + "/asks?state=all"},
		{"page.issue.subscribers", landing.Issue["subscribers"], "/api/v1/issues/" + issue.Key + "/subscribers"},
		{"page.issue.decisions", landing.Issue["decisions"], "/api/v1/issues/" + issue.Key + "/decisions"},
		{"page.issue.coverage", landing.Issue["coverage"], "/api/v1/issues/" + issue.Key + "/coverage"},
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
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createTestProject(t, handler, "LAND")
	addArchitectureSource(t, database, "LAND", "legion/land")
	// The board needs an open issue to list; nothing here reads the issue itself, and
	// createSpecIssue already fails this test where the create did not answer.
	createSpecIssue(t, handler, "LAND", "# Spec\n\nAn open issue for the board.\n")
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

// The landing reads the secrets broker's pending list through the same client as GET
// /api/v1/credential-requests?approver=me, but the two do not hand it on in the same way. The route
// relays the broker's bytes unparsed. The landing decodes them into wire.PendingResponse and
// encodes them again (landingCredentialRequests). That round trip keeps only the fields the type
// declares, and it writes each value in Go's own spelling. This row reads one list through both
// and holds the two to the same values. TestLandingAnswersWhatThePerResourceRoutesAnswer compares
// this part too, but on a server with no broker, where both sides are null.
//
// The list holds one row that is pending and one row that expired, and every field of both is
// set. Both rows are secret requests, because a machine login carries no approve_url and no
// session ids. The fake broker spells each time with three digits of milliseconds
// (`2026-11-24T09:30:15.120Z`), as the e2e fake broker (packages/dispatch/e2e/fake-broker.ts)
// serves the times that Date.toISOString wrote. Each time is at a millisecond that ends in zero,
// which Go spells shorter (`.12Z`). The route relays the broker's spelling unchanged and the
// landing writes Go's, so the row passes only because it compares each time as the moment it
// names.
//
// That the route's body is the broker's bytes as sent is the row of
// TestCredentialRouteForwardsBrokerSuccessBodyAndAuth, so what the route answers here is what the
// broker wrote.
func TestLandingCredentialRequestsAreTheValuesTheRouteRelays(t *testing.T) {
	t.Parallel()
	sent := wire.PendingResponse{Pending: []wire.PendingEntry{
		{
			RecordID:    "rec-pending",
			Kind:        "agent_secret",
			Identifiers: []string{"GITHUB_TOKEN", "NPM_TOKEN"},
			RequestedAt: time.Date(2026, 11, 24, 9, 30, 15, int(120*time.Millisecond), time.UTC),
			Session:     wire.RecordSessionResp{Request: new("ses-request-1"), Enrollment: new("ses-enrolled-1")},
			ApproveURL:  "https://secrets.example.com/approve/rec-pending",
			State:       "pending",
			ExpiresAt:   time.Date(2026, 11, 24, 9, 45, 15, int(120*time.Millisecond), time.UTC),
		},
		{
			RecordID:    "rec-expired",
			Kind:        "agent_secret",
			Identifiers: []string{"PYPI_TOKEN"},
			RequestedAt: time.Date(2026, 11, 23, 16, 2, 7, int(450*time.Millisecond), time.UTC),
			Session:     wire.RecordSessionResp{Request: new("ses-request-2"), Enrollment: new("ses-enrolled-2")},
			ApproveURL:  "https://secrets.example.com/approve/rec-expired",
			State:       "expired",
			ExpiresAt:   time.Date(2026, 11, 23, 16, 17, 7, int(450*time.Millisecond), time.UTC),
		},
	}}
	assertEveryFieldSet(t, "PendingResponse", reflect.ValueOf(sent))
	encoded, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("encode the broker's pending list: %v", err)
	}
	// The values of sent, with each RFC 3339 time that Go wrote spelled again with three digits of
	// milliseconds. A time of sent that the body does not hold so, or that Go spells the same way,
	// would leave the moment comparison below unexercised, so it fails here.
	const brokerTime = "2006-01-02T15:04:05.000Z07:00"
	body := regexp.MustCompile(`"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z"`).ReplaceAllStringFunc(string(encoded),
		func(quoted string) string {
			at, err := time.Parse(time.RFC3339Nano, strings.Trim(quoted, `"`))
			if err != nil {
				t.Fatalf("read the time %s of the broker's pending list: %v", quoted, err)
			}
			return `"` + at.Format(brokerTime) + `"`
		})
	for _, entry := range sent.Pending {
		for _, at := range []time.Time{entry.RequestedAt, entry.ExpiresAt} {
			spelled := at.Format(brokerTime)
			if spelled == at.Format(time.RFC3339Nano) || !strings.Contains(body, `"`+spelled+`"`) {
				t.Fatalf("the broker's body must spell %s as %s, which Go spells shorter: %s",
					at.Format(time.RFC3339Nano), spelled, body)
			}
		}
	}

	// The query that cipher.Client.Pending sends for alice. Each read is held to it, so neither
	// side can agree with the other by a different read.
	const pendingQuery = "approver=alice&state=pending&state=expired"
	broker := newFakeBrokerRig(t)
	broker.bodyByRoute["GET /v1/pending"] = body
	handler := newCredentialTestHandler(t, broker.server.URL)

	// Any page does: the shell, which carries the list, is the same on each page.
	carried := readLanding(t, handler, "/api/v1/landing?page=inbox").Shell["credential_requests"]
	if string(carried) == "null" {
		t.Fatalf("the landing's shell.credential_requests is null: its read of the broker failed, or answered after %v",
			landingServiceBound)
	}
	if broker.lastQuery != pendingQuery {
		t.Fatalf("the landing asked the broker for ?%s, want ?%s", broker.lastQuery, pendingQuery)
	}
	relayed := readRouteBody(t, handler, "/api/v1/credential-requests?approver=me")
	if broker.lastQuery != pendingQuery || broker.calls != 2 {
		t.Fatalf("the broker was asked %d times, the last for ?%s, want 2 reads of ?%s",
			broker.calls, broker.lastQuery, pendingQuery)
	}
	assertSameJSONMoments(t, "shell.credential_requests", carried, relayed)
}

// A page the route does not know is refused by name, and so is a page that needs a parameter the
// request did not carry: a landing answered empty would have the dashboard draw nothing, where a
// refusal has it read its page for itself.
func TestLandingRefusesAPageItDoesNotKnow(t *testing.T) {
	t.Parallel()
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

// A caller that asks for the display trees (`Dispatch-Trees: 1`, which the dashboard sends on
// every read) gets them in the coverage part as the coverage route gives them: each requirement
// registers its text with the issue page's reader, which the page resolves after the coverage
// read (landingIssuePage). A landing that read the coverage with no reader would answer every
// requirement with no text_tree, and the dashboard would draw it from its own parse.
func TestLandingCoverageCarriesTheTreesTheCoverageRouteAnswers(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createTestProject(t, handler, "LAND")
	issue := createSpecIssue(t, handler, "LAND", "# Spec\n\nThe landing reads this document.\n\n"+twoRequirements)

	target := "/api/v1/landing?page=issue&key=" + issue.Key
	landing := landingFrom(t, target, treesRequest(t, handler, target, true))
	route := treesRequest(t, handler, "/api/v1/issues/"+issue.Key+"/coverage", true).Body.Bytes()
	assertSameJSON(t, "page.issue.coverage", landing.partOf(t, "issue")["coverage"], route)

	var coverage struct {
		Requirements []struct {
			TextTree json.RawMessage `json:"text_tree"`
		} `json:"requirements"`
	}
	if err := json.Unmarshal(route, &coverage); err != nil {
		t.Fatalf("decode the coverage route's answer %s: %v", route, err)
	}
	if len(coverage.Requirements) != 2 {
		t.Fatalf("the coverage route answered %d requirements, want the spec's 2", len(coverage.Requirements))
	}
	for i, requirement := range coverage.Requirements {
		if len(requirement.TextTree) == 0 || string(requirement.TextTree) == "null" {
			t.Errorf("requirement %d of the coverage route has no text_tree, so the comparison above proves nothing", i)
		}
	}
}

// An issue key that is no issue leaves the issue's own parts null rather than refusing the whole
// landing: the shell is still what every page draws, and the dashboard reads the issue itself,
// which answers the 404 it already draws. The decisions part is no exception to that rule: its
// own route answers an issue it has no row of with empty content rather than a refusal
// (getIssueDecisions), and the landing carries exactly that.
func TestLandingLeavesAnUnknownIssueNullAndAnswersTheShell(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createTestProject(t, handler, "LAND")
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
	assertSameJSON(t, "shell.projects", landing.Shell["projects"], readRouteBody(t, handler, "/api/v1/projects"))
}

// A routed issue makes the database parts read the route roster from the listener: the inbox
// (an open ask on a routed issue, on every landing), the pinned issues, the board's issues and
// the issue's own detail. With the listener silent, each such read waits the roster's shared
// fetch out, so the landing - which holds every cold page behind it - answers well past the
// bound the service parts keep. Measured in a browser at acc4638d: with the
// listener silent and one open ask on a routed issue, the board drew after 7.6 s and an unrouted
// issue's page after 3.3 s, against 3.4 s and 1.3 s at the merge base d5e2a5fe, which held no page
// behind the inbox's read.
func TestLandingDoesNotWaitOnASilentListenerForARoutedIssue(t *testing.T) {
	// Not parallel: it holds each landing to a wall-clock bound, landingServiceBound plus
	// landingServiceTestMargin, and a landing read under the load of the package's parallel tests
	// could miss that bound.
	listenerURL, _ := silentService(t)
	brokerURL, _ := silentService(t)
	handler := newLandingServicesHandler(t, listenerURL, brokerURL)
	createTestProject(t, handler, "LAND")
	issueTitled := func(title string) model.Issue {
		t.Helper()
		response := createIssueRequest(t, handler, map[string]any{"project": "LAND", "title": title, "spec": "# Spec\n\n" + title + ".\n"})
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	routed := issueTitled("Routed reviewer handoff")
	plain := issueTitled("Unrouted gardening notes")
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+routed.Key,
		map[string]any{"route": "role:reviewer"}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("route %s: status=%d body=%s", routed.Key, patched.Code, patched.Body.String())
	}
	asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+routed.Key+"/asks",
		map[string]any{"question": "Is the routed issue ready?", "actor": sessionActor()})
	if asked.Code != http.StatusCreated {
		t.Fatalf("ask on %s: status=%d body=%s", routed.Key, asked.Code, asked.Body.String())
	}
	for _, target := range []string{
		"/api/v1/landing?page=board&project=LAND",
		"/api/v1/landing?page=issue&key=" + plain.Key,
		"/api/v1/landing?page=issue&key=" + routed.Key,
	} {
		ctx, cancel := context.WithTimeout(t.Context(), landingServiceTestLimit)
		started := time.Now()
		response := serveLanding(ctx, handler, target)
		took := time.Since(started)
		cancel()
		t.Logf("GET %s answered %d in %v", target, response.Code, took.Round(time.Millisecond))
		if response.Code != http.StatusOK {
			t.Errorf("GET %s: status=%d, want 200", target, response.Code)
		}
		if took > landingServiceBound+landingServiceTestMargin {
			t.Errorf("GET %s answered after %v, want within %v: a routed issue's roster read waits on the silent listener outside the bound",
				target, took.Round(time.Millisecond), landingServiceBound+landingServiceTestMargin)
		}
	}
}

// landingServiceTestLimit is where a test ends the request of a landing it holds to
// landingServiceBound. It is below the listener's and the broker's own five-second windows, so a
// landing that waits on a silent service fails its test on time rather than reaching the client's
// window, or hanging the package where a client honoured no window at all.
const landingServiceTestLimit = 4 * time.Second

// landingServiceTestMargin is how far past landingServiceBound a landing may answer and still
// pass: the database parts it reads beside the silent ones, and the document page's own document,
// which the page reads before it can ask the listener for that document's subscribers. It is
// under the second by which routeRosterTimeout outlasts the bound, so a landing whose agents part
// waited the roster's shared fetch out rather than the bound still fails here.
const landingServiceTestMargin = 500 * time.Millisecond

// The two lines the landing writes for a part it left out, written out here rather than taken
// from the code under test: a part whose read failed, and a part whose service did not answer
// within landingServiceBound.
const (
	landingPartWarning = "dispatch: the landing left a part out"
	landingLateWarning = "dispatch: the landing left a part out: its service did not answer within the bound"
)

// A cold page draws nothing but its skeleton until the landing answers, so a service outside the
// database that accepts the connection and never answers must cost the landing no more than
// landingServiceBound. The part that service answers is left null, which the page reads from its
// own route once it has drawn, and every database part is answered beside it. Both stand-ins here
// hold every request for as long as the test runs, so a landing that waited on the listener or the
// broker for the client's own window would answer far past the bound; the request itself ends at
// landingServiceTestLimit, so such a landing fails here on its time rather than hanging.
//
// Each part left null is named in the log exactly once, so an operator sees which service is slow,
// and the broker's part and the subscribers are named as late. The agents part is held only to
// being named once: the roster's shared fetch stores the listener's silence once its own bound
// (routeRosterTimeout) passes, and a landing that starts after that reads the stored silence as
// the failure it is rather than waiting out landingServiceBound.
func TestLandingDoesNotWaitOnASilentListenerOrBroker(t *testing.T) {
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	listenerURL, _ := silentService(t)
	brokerURL, _ := silentService(t)
	handler := newLandingServicesHandler(t, listenerURL, brokerURL)
	createTestProject(t, handler, "LAND")
	issue := createSpecIssue(t, handler, "LAND", "# Spec\n\nThe landing reads this document.\n")
	document := createProjectDocument(t, handler, "LAND", "rollout.md", "# Rollout\n\nThe document page reads this.\n")

	// readSilently reads one landing and holds it to the bound, to the shell's two silent parts,
	// to one warning for each silent part (subscribers names the page's own, empty for a page
	// with none), and to the shell's database parts.
	readSilently := func(t *testing.T, target, subscribers string) landingBody {
		t.Helper()
		mark := len(records.Records())
		ctx, cancel := context.WithTimeout(t.Context(), landingServiceTestLimit)
		defer cancel()
		started := time.Now()
		response := serveLanding(ctx, handler, target)
		took := time.Since(started)
		landing := landingFrom(t, target, response)
		t.Logf("GET %s answered in %v", target, took.Round(time.Millisecond))
		if took > landingServiceBound+landingServiceTestMargin {
			t.Errorf("GET %s answered after %v, want within %v: the bound of %v and a margin of %v",
				target, took.Round(time.Millisecond), landingServiceBound+landingServiceTestMargin,
				landingServiceBound, landingServiceTestMargin)
		}
		for _, part := range []struct {
			part string
			body json.RawMessage
		}{
			{"shell.agents", landing.Shell["agents"]},
			{"shell.credential_requests", landing.Shell["credential_requests"]},
		} {
			if string(part.body) != "null" {
				t.Errorf("GET %s: %s = %s, want null from a service that never answered", target, part.part, part.body)
			}
		}
		named, late := landingWarnings(records.Records()[mark:])
		want := map[string]int{"shell.agents": 1, "shell.credential_requests": 1}
		if subscribers != "" {
			want[subscribers] = 1
		}
		if !maps.Equal(named, want) {
			t.Errorf("GET %s: the warnings name %v, want each silent part once and no database part: %v", target, named, want)
		}
		for part := range want {
			if part != "shell.agents" && late[part] != 1 {
				t.Errorf("GET %s: %s is named late %d times, want once", target, part, late[part])
			}
		}
		for _, part := range []struct {
			part  string
			body  json.RawMessage
			route string
		}{
			{"shell.inbox", landing.Shell["inbox"], "/api/v1/inbox"},
			{"shell.projects", landing.Shell["projects"], "/api/v1/projects"},
			{"shell.priority_summary", landing.Shell["priority_summary"], "/api/v1/priorities?summary=true"},
		} {
			assertSameJSON(t, part.part, part.body, readRouteBody(t, handler, part.route))
		}
		return landing
	}

	t.Run("the issue page", func(t *testing.T) {
		landing := readSilently(t, "/api/v1/landing?page=issue&key="+issue.Key, "issue.subscribers")
		if string(landing.Issue["subscribers"]) != "null" {
			t.Errorf("page.issue.subscribers = %s, want null from a listener that never answered", landing.Issue["subscribers"])
		}
		spec := landing.issueDocument(t)
		for _, part := range []struct {
			part  string
			body  json.RawMessage
			route string
		}{
			{"page.issue.issue", landing.Issue["issue"], "/api/v1/issues/" + issue.Key},
			{"page.issue.comments", landing.Issue["comments"], "/api/v1/issues/" + issue.Key + "/comments"},
			{"page.issue.decisions", landing.Issue["decisions"], "/api/v1/issues/" + issue.Key + "/decisions"},
			{"page.issue.document.text", spec["text"], "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/text"},
		} {
			assertSameJSON(t, part.part, part.body, readRouteBody(t, handler, part.route))
		}
	})

	t.Run("the document page", func(t *testing.T) {
		page := readSilently(t, "/api/v1/landing?page=document&project=LAND&slug="+document.Slug, "document.subscribers").
			partOf(t, "document")
		if string(page["subscribers"]) != "null" {
			t.Errorf("page.document.subscribers = %s, want null from a listener that never answered", page["subscribers"])
		}
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
	})

	t.Run("the shell alone", func(t *testing.T) {
		inbox := readSilently(t, "/api/v1/landing?page=inbox", "").partOf(t, "inbox")
		assertSameJSONExcept(t, "page.inbox.threads", inbox["threads"],
			readRouteBody(t, handler, "/api/v1/me/replies?kind=reply,agent&state=inbox"), "as_of")
	})
}

// A part whose route would answer a not-found - an issue key that is no issue, a slug that is no
// document of the project, a slug that names a file, which has no text and no blocks - is left
// null and written to no log: the routes the landing stands in for answer each of these as the
// caller's own miss and log none of them, and a warn line an operator reads must be a read that
// failed. A part that did fail is still named, which the last case proves with a listener that
// refuses its interests list.
func TestLandingWarnsOfNoPartANotFoundAnswerLeftOut(t *testing.T) {
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	listenerURL, listener := newFakeListener(t, nil, nil)
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listenerURL})
	createTestProject(t, handler, "LAND")
	uploaded := multipartRequest(t, handler, "/api/v1/projects/LAND/artifacts", map[string]string{"name": "diagram.png"},
		"diagram.png", "image/png", pngBytes("diagram"), "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload project image: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	image := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact

	for _, landing := range []struct {
		name   string
		target string
		page   string
		// null is the page's members the not-found leaves null.
		null []string
	}{
		{"an issue key that is no issue", "/api/v1/landing?page=issue&key=LAND-404", "issue", []string{"issue"}},
		{"a slug that is no document", "/api/v1/landing?page=document&project=LAND&slug=no-such-document", "document", []string{"artifact"}},
		{"a slug that names a file", "/api/v1/landing?page=document&project=LAND&slug=" + image.Slug, "document", []string{"text", "blocks"}},
	} {
		t.Run(landing.name, func(t *testing.T) {
			mark := len(records.Records())
			page := readLanding(t, handler, landing.target).partOf(t, landing.page)
			for _, member := range landing.null {
				if string(page[member]) != "null" {
					t.Errorf("GET %s: page.%s.%s = %s, want null", landing.target, landing.page, member, page[member])
				}
			}
			if named, _ := landingWarnings(records.Records()[mark:]); len(named) > 0 {
				t.Errorf("GET %s warned of %v, want no warning: the route behind each such part answers its not-found and logs nothing",
					landing.target, named)
			}
		})
	}

	t.Run("a listener that refuses", func(t *testing.T) {
		listener.mu.Lock()
		listener.refuseList = func(w http.ResponseWriter, _ string) bool {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		listener.mu.Unlock()
		mark := len(records.Records())
		landing := readLanding(t, handler, "/api/v1/landing?page=issue&key=LAND-404")
		if string(landing.Issue["subscribers"]) != "null" {
			t.Errorf("page.issue.subscribers = %s, want null from a listener that refused", landing.Issue["subscribers"])
		}
		if named, _ := landingWarnings(records.Records()[mark:]); named["issue.subscribers"] != 1 {
			t.Errorf("the warnings name %v, want issue.subscribers once: a refused read is a failure the operator must see", named)
		}
	})
}

// A landing whose own request ends - the page navigated away, the tab closed - leaves every part it
// had not read null and writes no warning for any of them: nothing failed but the caller's wait,
// the head read above the parts already skips a request that ended (getLanding), and so do the
// routes the parts stand in for. The listener here holds every read, and the request is cancelled
// the moment a read reaches it, so every part read after that meets the request's end.
func TestLandingWarnsOfNoPartACancelledRequestLeftOut(t *testing.T) {
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	listenerURL, reached := silentService(t)
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listenerURL})
	createTestProject(t, handler, "LAND")
	issue := createSpecIssue(t, handler, "LAND", "# Spec\n\nA landing nobody waits for.\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// cancelled closes before the cancel, so a landing that ended on the cancel always finds it
	// closed, and one that ended without a read reaching the listener never does.
	cancelled := make(chan struct{})
	go func() {
		select {
		case <-reached:
			close(cancelled)
			cancel()
		case <-ctx.Done():
		}
	}()
	mark := len(records.Records())
	target := "/api/v1/landing?page=issue&key=" + issue.Key
	response := serveLanding(ctx, handler, target)
	select {
	case <-cancelled:
	default:
		t.Fatalf("GET %s never reached the listener, so its request was never cancelled: status=%d", target, response.Code)
	}
	landingFrom(t, target, response)
	if named, _ := landingWarnings(records.Records()[mark:]); len(named) > 0 {
		t.Errorf("GET %s, cancelled while it read, warned of %v, want no warning", target, named)
	}
}

// silentService stands in for a listener or a broker that accepts every connection and never
// answers: each request is held until its caller gives up on it or the test ends. It answers its
// URL and a channel that receives the path of each request it holds, for a test that acts the
// moment a read reaches the service; a test that does not read it loses nothing.
func silentService(t *testing.T) (string, <-chan string) {
	t.Helper()
	reached := make(chan string, 64)
	ended := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case reached <- r.URL.Path:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-ended:
		}
	}))
	t.Cleanup(server.Close)
	// Registered after Close, so it runs before it: Close waits out every request it holds.
	t.Cleanup(func() { close(ended) })
	return server.URL, reached
}

// newLandingServicesHandler mounts the real routes with the listener client pointed at listenerURL
// and the secrets broker's at brokerURL, each at the client's own five-second window. newTestServer
// configures no broker, on purpose (newCredentialTestHandler says why), so this builds its
// dependencies itself as that helper does, with the listener beside the broker.
func newLandingServicesHandler(t *testing.T, listenerURL, brokerURL string) http.Handler {
	t.Helper()
	database := storetest.Open(t)
	broker := events.NewBroker()
	// The documents never settle during the test, so every version it reads is one its own
	// requests wrote.
	documentService := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: testServerURL, Settle: time.Hour})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedPeople(t, database, "alice", "bob")
	deps, err := NewDeps(DepsInput{
		Store:                  database,
		Identity:               headerIdentity(database),
		AgentTokens:            sharedAgentTokens(t, "agent-token"),
		ServerURL:              testServerURL,
		Docs:                   documentService,
		Events:                 broker,
		EnvoyURL:               listenerURL,
		ListenerTakeoverWindow: testTakeoverWindow,
		AgentSecretsURL:        brokerURL,
		AgentSecretsTokenFile:  accountTokenFile(t, credentialTestAccountToken),
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	return mux
}

// landingWarnings counts the parts the landing's warnings in records name: named counts every
// warning of a part left out, and late those among them that name a service that did not answer
// within landingServiceBound.
func landingWarnings(records []slog.Record) (named, late map[string]int) {
	named, late = map[string]int{}, map[string]int{}
	for _, record := range records {
		if record.Message != landingPartWarning && record.Message != landingLateWarning {
			continue
		}
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "part" {
				named[attr.Value.String()]++
				if record.Message == landingLateWarning {
					late[attr.Value.String()]++
				}
			}
			return true
		})
	}
	return named, late
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
	return landingFrom(t, target, dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"))
}

// serveLanding reads one landing as alice under ctx, for a test that ends the request itself.
func serveLanding(ctx context.Context, handler http.Handler, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// landingFrom is the landing response answered, decoded, and fails unless it is a 200.
func landingFrom(t *testing.T, target string, response *httptest.ResponseRecorder) landingBody {
	t.Helper()
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
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
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
	carried, answered := decodeLandingPart(t, part, landing, route)
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

// assertSameJSONMoments is assertSameJSON for a part that the landing decodes into a Go type and
// encodes again, where its route relays the bytes of another service as that service wrote them.
// Go writes a time in its own spelling, which can differ from the relayed spelling of the same
// moment. Thus a string that reads as an RFC 3339 time on both sides is compared as the moment it
// names. Every other value is compared exactly, and a key on one side must be on the other side.
// Each difference is named by its path in the part, so a failure says which key or value differs.
func assertSameJSONMoments(t *testing.T, part string, landing json.RawMessage, route []byte) {
	t.Helper()
	carried, answered := decodeLandingPart(t, part, landing, route)
	assertSameJSONValue(t, part, carried, answered)
}

// decodeLandingPart is a part of the landing and the body of its route, each decoded as JSON, and
// fails when either is not.
func decodeLandingPart(t *testing.T, part string, landing json.RawMessage, route []byte) (carried, answered any) {
	t.Helper()
	if err := json.Unmarshal(landing, &carried); err != nil {
		t.Fatalf("decode the landing's %s %s: %v", part, landing, err)
	}
	if err := json.Unmarshal(route, &answered); err != nil {
		t.Fatalf("decode the route behind %s %s: %v", part, route, err)
	}
	return carried, answered
}

// assertSameJSONValue is assertSameJSONMoments for one decoded value at path, and for each value
// that it holds.
func assertSameJSONValue(t *testing.T, path string, carried, answered any) {
	t.Helper()
	differs := func() {
		t.Errorf("%s: the landing carries %s, the route answers %s", path, asJSON(t, carried), asJSON(t, answered))
	}
	switch answer := answered.(type) {
	case map[string]any:
		object, isObject := carried.(map[string]any)
		if !isObject {
			differs()
			return
		}
		for _, key := range slices.Sorted(maps.Keys(answer)) {
			if _, held := object[key]; !held {
				t.Errorf("%s.%s: the route answers %s, and the landing has no such key", path, key, asJSON(t, answer[key]))
				continue
			}
			assertSameJSONValue(t, path+"."+key, object[key], answer[key])
		}
		for _, key := range slices.Sorted(maps.Keys(object)) {
			if _, held := answer[key]; !held {
				t.Errorf("%s.%s: the landing carries %s, and the route has no such key", path, key, asJSON(t, object[key]))
			}
		}
	case []any:
		list, isList := carried.([]any)
		if !isList || len(list) != len(answer) {
			differs()
			return
		}
		for i := range answer {
			assertSameJSONValue(t, fmt.Sprintf("%s[%d]", path, i), list[i], answer[i])
		}
	case string:
		if text, isText := carried.(string); !isText || (text != answer && !sameMoment(text, answer)) {
			differs()
		}
	default:
		// A number, a boolean or null.
		if !reflect.DeepEqual(carried, answered) {
			differs()
		}
	}
}

// sameMoment reports whether both texts are RFC 3339 times that name the same moment.
func sameMoment(first, second string) bool {
	firstTime, err := time.Parse(time.RFC3339Nano, first)
	if err != nil {
		return false
	}
	secondTime, err := time.Parse(time.RFC3339Nano, second)
	return err == nil && firstTime.Equal(secondTime)
}

// assertEveryFieldSet fails for each field of value, and of each struct, pointer and list that
// value holds, that is the zero value of its type. A row that compares two encodings of one value
// holds only the fields that value sets: a zero field is the same zero on both sides, or is
// omitted on both, whatever either side does with it. The fields are read from the type, not from
// a list that this file keeps, so a field that the type gets later fails here until the row sets
// it. A time is one value, not a struct of parts, so it is not walked into.
func assertEveryFieldSet(t *testing.T, path string, value reflect.Value) {
	t.Helper()
	if value.IsZero() {
		t.Errorf("%s is the zero value of its type: set it, so that the row compares it", path)
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		assertEveryFieldSet(t, path, value.Elem())
	case reflect.Struct:
		if value.Type() == reflect.TypeFor[time.Time]() {
			return
		}
		for i := range value.NumField() {
			assertEveryFieldSet(t, path+"."+value.Type().Field(i).Name, value.Field(i))
		}
	case reflect.Slice:
		if value.Len() == 0 {
			t.Errorf("%s is empty: give it an element, so that the row compares one", path)
		}
		for i := range value.Len() {
			assertEveryFieldSet(t, fmt.Sprintf("%s[%d]", path, i), value.Index(i))
		}
	}
}
