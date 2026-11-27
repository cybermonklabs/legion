package api

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type askIdentity struct {
	ID            string `json:"id"`
	OpenedEventID int64  `json:"opened_event_id"`
}

func TestAskReadsAttachCanonicalOpenedEventID(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask read identity", "A spec")
	openResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Keep open?",
		"options":  []map[string]string{{"label": "Yes"}},
		"actor":    sessionActor(),
	})
	if openResponse.Code != http.StatusCreated {
		t.Fatalf("create open ask: status=%d body=%s", openResponse.Code, openResponse.Body.String())
	}
	openAsk := decodeBody[askIdentity](t, openResponse)
	legacyResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Answer for fallback?",
		"options":  []map[string]string{{"label": "Yes"}},
		"actor":    sessionActor(),
	})
	if legacyResponse.Code != http.StatusCreated {
		t.Fatalf("create fallback ask: status=%d body=%s", legacyResponse.Code, legacyResponse.Body.String())
	}
	legacyAsk := decodeBody[askIdentity](t, legacyResponse)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+legacyAsk.ID+"/answer", map[string]any{
		"selected": []string{"Yes"},
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer fallback ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	rows := decodeBody[[]struct {
		ID      int64 `json:"id"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
		Type string `json:"type"`
	}](t, events)
	var opened, answeredEvent int64
	for _, row := range rows {
		if row.Payload.ID != legacyAsk.ID {
			continue
		}
		if row.Type == "ask.opened" {
			opened = row.ID
		}
		if row.Type == "ask.answered" {
			answeredEvent = row.ID
		}
	}
	if opened == 0 || answeredEvent == 0 {
		t.Fatalf("missing fallback events: opened=%d answered=%d", opened, answeredEvent)
	}
	if _, err := database.Pool.Exec(context.Background(), `delete from events where id = $1`, opened); err != nil {
		t.Fatalf("delete legacy opened event: %v", err)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+legacyAsk.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("get ask: status=%d body=%s", read.Code, read.Body.String())
	}
	readAsk := decodeBody[struct {
		Ask askIdentity `json:"ask"`
	}](t, read).Ask
	if readAsk.OpenedEventID != answeredEvent {
		t.Fatalf("get ask opened_event_id=%d, want legacy fallback %d", readAsk.OpenedEventID, answeredEvent)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=all", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list asks: status=%d body=%s", listed.Code, listed.Body.String())
	}
	listedLegacy := false
	for _, ask := range decodeBody[[]askIdentity](t, listed) {
		if ask.ID == legacyAsk.ID {
			listedLegacy = true
			if ask.OpenedEventID != answeredEvent {
				t.Fatalf("listed ask opened_event_id=%d, want legacy fallback %d", ask.OpenedEventID, answeredEvent)
			}
		}
	}
	if !listedLegacy {
		t.Fatal("fallback ask missing from issue ask list")
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("get issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	detailOpen := false
	for _, ask := range decodeBody[struct {
		OpenAsks []askIdentity `json:"open_asks"`
	}](t, detail).OpenAsks {
		if ask.ID == openAsk.ID {
			detailOpen = true
			if ask.OpenedEventID != openAsk.OpenedEventID {
				t.Fatalf("issue open ask opened_event_id=%d, want %d", ask.OpenedEventID, openAsk.OpenedEventID)
			}
		}
	}
	if !detailOpen {
		t.Fatal("open ask missing from issue detail")
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("list inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	inboxOpen := false
	for _, ask := range decodeBody[[]askIdentity](t, inbox) {
		if ask.ID == openAsk.ID {
			inboxOpen = true
			if ask.OpenedEventID != openAsk.OpenedEventID {
				t.Fatalf("inbox ask opened_event_id=%d, want %d", ask.OpenedEventID, openAsk.OpenedEventID)
			}
		}
	}
	if !inboxOpen {
		t.Fatal("open ask missing from inbox")
	}
}

type openAsksRead struct {
	SessionID      string `json:"session_id"`
	AsOf           string `json:"as_of"`
	OpenedSince    bool   `json:"opened_since"`
	Count          int    `json:"count"`
	WaitingOnHuman int    `json:"waiting_on_human"`
	WaitingOnAgent int    `json:"waiting_on_agent"`
	Asks           []struct {
		ID           string `json:"id"`
		Question     string `json:"question"`
		Ref          string `json:"ref"`
		HumanReplied bool   `json:"human_replied"`
		Priority     *int   `json:"priority"`
		WaitingOn    string `json:"waiting_on"`
		Owner        struct {
			Issue *struct {
				Key   string `json:"key"`
				Title string `json:"title"`
			} `json:"issue,omitempty"`
			Document *struct {
				Project string `json:"project"`
				Slug    string `json:"slug"`
				Name    string `json:"name"`
			} `json:"document,omitempty"`
		} `json:"owner"`
		LastReply *model.AskLastReply `json:"last_reply"`
	} `json:"asks"`
}

func listOpenAsks(t *testing.T, handler http.Handler, sessionID string, since string) openAsksRead {
	t.Helper()
	target := "/api/v1/asks/open?author_session=" + url.QueryEscape(sessionID)
	if since != "" {
		target += "&since=" + url.QueryEscape(since)
	}
	response := sessionRequest(t, handler, http.MethodGet, target, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list open asks: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[openAsksRead](t, response)
}

func TestListOpenAsksForSession(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Open-ask owner", "A spec")
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n")
	const ownerSession = "session-open-asks"
	baseline := time.Now().UTC().Format(time.RFC3339Nano)

	createAsk := func(target, question, sessionID string) model.Ask {
		t.Helper()
		response := sessionRequest(t, handler, http.MethodPost, target, map[string]any{
			"question": question,
			"actor":    map[string]any{"kind": "session", "id": sessionID},
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("create ask %q: status=%d body=%s", question, response.Code, response.Body.String())
		}
		return decodeBody[model.Ask](t, response)
	}

	if empty := listOpenAsks(t, handler, "session-with-no-asks", ""); empty.Count != 0 || len(empty.Asks) != 0 {
		t.Fatalf("empty open asks = %#v, want no asks", empty)
	}

	issueAsk := createAsk("/api/v1/issues/"+issue.Key+"/asks", "Issue question", ownerSession)
	priorityIssueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST", "title": "Priority owner", "priority": 0,
	}, "alice")
	if priorityIssueResponse.Code != http.StatusCreated {
		t.Fatalf("create priority issue: status=%d body=%s", priorityIssueResponse.Code, priorityIssueResponse.Body.String())
	}
	priorityIssue := decodeBody[model.Issue](t, priorityIssueResponse)
	priorityAsk := createAsk("/api/v1/issues/"+priorityIssue.Key+"/asks", "Priority question", ownerSession)
	documentAsk := createAsk("/api/v1/artifacts/"+document.ID+"/asks", "Document question", ownerSession)
	otherSessionAsk := createAsk("/api/v1/issues/"+issue.Key+"/asks", "Other session", "session-other")
	answered := createAsk("/api/v1/issues/"+issue.Key+"/asks", "Answered question", ownerSession)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answered.ID+"/answer", map[string]any{
		"text": "Done",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", response.Code, response.Body.String())
	}
	retracted := createAsk("/api/v1/issues/"+issue.Key+"/asks", "Retracted question", ownerSession)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+retracted.ID+"/resolve", map[string]any{
		"kind": "retracted", "reason": "No longer needed",
		"actor": map[string]any{"kind": "session", "id": ownerSession},
	}); response.Code != http.StatusOK {
		t.Fatalf("retract ask: status=%d body=%s", response.Code, response.Body.String())
	}

	humanReply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Could you clarify?", "ask_id": issueAsk.ID,
	}, "alice")
	if humanReply.Code != http.StatusCreated {
		t.Fatalf("human reply: status=%d body=%s", humanReply.Code, humanReply.Body.String())
	}
	agentReply := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Here is the clarification.", "ask_id": issueAsk.ID,
		"actor": map[string]any{"kind": "session", "id": ownerSession},
	})
	if agentReply.Code != http.StatusCreated {
		t.Fatalf("agent reply: status=%d body=%s", agentReply.Code, agentReply.Body.String())
	}
	documentReply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
		"body": "What is the rollout?", "ask_id": documentAsk.ID,
	}, "alice")
	if documentReply.Code != http.StatusCreated {
		t.Fatalf("document human reply: status=%d body=%s", documentReply.Code, documentReply.Body.String())
	}

	first := listOpenAsks(t, handler, ownerSession, baseline)
	if first.SessionID != ownerSession || first.AsOf == "" || !first.OpenedSince {
		t.Fatalf("open asks metadata = %#v, want session/as_of/opened_since", first)
	}
	if first.Count != 3 || first.WaitingOnHuman != 2 || first.WaitingOnAgent != 1 || len(first.Asks) != 3 {
		t.Fatalf("open ask counts = %#v, want three split between human and agent", first)
	}
	if first.Asks[0].ID != priorityAsk.ID || first.Asks[0].Priority == nil || *first.Asks[0].Priority != 0 {
		t.Fatalf("open asks first row = %#v, want the later P0 ask", first.Asks[0])
	}
	var gotIssue, gotDocument, gotPriority bool
	for _, ask := range first.Asks {
		switch ask.ID {
		case issueAsk.ID:
			gotIssue = ask.Question == "Issue question" &&
				ask.Owner.Issue != nil && ask.Owner.Issue.Key == issue.Key && ask.Owner.Issue.Title == "Open-ask owner" &&
				ask.Owner.Document == nil && ask.HumanReplied && ask.WaitingOn == "human" &&
				ask.LastReply != nil && ask.LastReply.Author.Kind == "session" && ask.LastReply.Author.ID == ownerSession &&
				// The SPA's item route: the bare issue page reads no `?ask=`, so `/issues/KEY?ask=ID`
				// would land an agent following it on the issue and not the ask.
				ask.Ref == "/issues/"+issue.Key+"/asks/"+issueAsk.ID
		case documentAsk.ID:
			gotDocument = ask.Question == "Document question" && ask.Owner.Issue == nil &&
				ask.Owner.Document != nil && ask.Owner.Document.Project == "TEST" && ask.Owner.Document.Slug == "runbook" &&
				ask.Owner.Document.Name == "Runbook" && ask.HumanReplied && ask.WaitingOn == "agent" &&
				ask.LastReply != nil && ask.LastReply.Author.Kind == "user" && ask.LastReply.Author.ID == "alice" &&
				ask.Ref == "/projects/TEST/documents/runbook?ask="+documentAsk.ID
		case priorityAsk.ID:
			gotPriority = ask.Question == "Priority question" && ask.Priority != nil && *ask.Priority == 0 &&
				ask.Owner.Issue != nil && ask.Owner.Issue.Key == priorityIssue.Key && ask.WaitingOn == "human"
		}
	}
	if !gotIssue || !gotDocument || !gotPriority {
		t.Fatalf("open ask ownership/replies = %#v", first.Asks)
	}

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"status": "done",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}
	// The close withdrew the issue's open asks, so they leave the list; the answered and the
	// retracted ask keep their own records.
	closed := listOpenAsks(t, handler, ownerSession, "")
	if closed.Count != 2 || len(closed.Asks) != 2 || closed.Asks[0].ID != priorityAsk.ID || closed.Asks[1].ID != documentAsk.ID {
		t.Fatalf("closed issue open asks = %#v, want priority and document asks", closed)
	}
	for _, id := range []string{issueAsk.ID, otherSessionAsk.ID} {
		requireWithdrawn(t, handler, issue.Key, id, model.Actor{Kind: "user", ID: "alice", Proven: true})
	}
	if got := readBlockAsk(t, handler, answered.ID); got.State != "answered" {
		t.Fatalf("answered ask after the close = %s, want answered", got.State)
	}
	if got := readBlockAsk(t, handler, retracted.ID); got.State != "resolved" || got.Resolution == nil || got.Resolution.Kind != "retracted" {
		t.Fatalf("retracted ask after the close = %s %#v, want retracted", got.State, got.Resolution)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"status": "todo",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("reopen issue: status=%d body=%s", response.Code, response.Body.String())
	}
	// The reopen restored them.
	reopened := listOpenAsks(t, handler, ownerSession, "")
	restored := false
	for _, ask := range reopened.Asks {
		restored = restored || ask.ID == issueAsk.ID
	}
	if reopened.Count != 3 || !restored {
		t.Fatalf("reopened issue open asks = %#v, want all active asks", reopened)
	}
}

func TestListOpenAsksRejectsInvalidSelectors(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	for _, target := range []string{
		"/api/v1/asks/open?waiting_on=%20%20",
		"/api/v1/asks/open?author_session=%20%20",
		"/api/v1/asks/open?author_session=session&since=not-a-timestamp",
	} {
		response := sessionRequest(t, handler, http.MethodGet, target, nil)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s, want 400", target, response.Code, response.Body.String())
		}
	}
}

func listProjectOpenAsks(t *testing.T, handler http.Handler, project string) openAsksRead {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/open?project="+url.QueryEscape(project), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list project open asks: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[openAsksRead](t, response)
}

// Project scope covers both ownership paths — an ask on an issue (issues.project_key) and an
// ask on an unlinked project document (artifacts.project_key) — for every author, and stops at
// the project boundary.
func TestListOpenAsksForProject(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Project-scope owner", "A spec")
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n")
	otherIssue := createInteractionIssue(t, handler, "OTHER", "Other project", "A spec")

	createAsk := func(target, question, sessionID string) model.Ask {
		t.Helper()
		response := sessionRequest(t, handler, http.MethodPost, target, map[string]any{
			"question": question,
			"actor":    map[string]any{"kind": "session", "id": sessionID},
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("create ask %q: status=%d body=%s", question, response.Code, response.Body.String())
		}
		return decodeBody[model.Ask](t, response)
	}

	issueAsk := createAsk("/api/v1/issues/"+issue.Key+"/asks", "Issue question", "session-a")
	documentAsk := createAsk("/api/v1/artifacts/"+document.ID+"/asks", "Document question", "session-b")
	createAsk("/api/v1/issues/"+otherIssue.Key+"/asks", "Other project question", "session-a")

	listed := listProjectOpenAsks(t, handler, "TEST")
	if listed.Count != 2 || len(listed.Asks) != 2 {
		t.Fatalf("project open asks = %#v, want the issue ask and the document ask", listed)
	}
	// Oldest first: the issue ask was opened before the document ask, and neither issue carries
	// a priority, so creation order decides.
	if listed.Asks[0].ID != issueAsk.ID || listed.Asks[1].ID != documentAsk.ID {
		t.Fatalf("project open ask order = %#v, want the issue ask then the document ask", listed.Asks)
	}
	if listed.Asks[0].Owner.Issue == nil || listed.Asks[0].Owner.Issue.Key != issue.Key {
		t.Fatalf("issue-owned row = %#v, want issue %s", listed.Asks[0], issue.Key)
	}
	if listed.Asks[0].WaitingOn != "human" || listed.Asks[1].WaitingOn != "human" {
		t.Fatalf("project open asks waiting_on = %#v, want human on both", listed.Asks)
	}
	document1 := listed.Asks[1].Owner.Document
	if document1 == nil || document1.Project != "TEST" || document1.Slug != "runbook" {
		t.Fatalf("document-owned row = %#v, want the TEST runbook document", listed.Asks[1])
	}

	// Closing the issue withdraws its ask, so it leaves project scope exactly as it leaves session
	// scope; the unlinked document's ask stays.
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"status": "done",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}
	closed := listProjectOpenAsks(t, handler, "TEST")
	if closed.Count != 1 || len(closed.Asks) != 1 || closed.Asks[0].ID != documentAsk.ID {
		t.Fatalf("project open asks after close = %#v, want only the document ask", closed)
	}
	requireWithdrawn(t, handler, issue.Key, issueAsk.ID, model.Actor{Kind: "user", ID: "alice", Proven: true})
}

// The read takes at most one scope, and neither is every project
// (TestOpenAsksWithNeitherScopeAnswerEveryVisibleProjectAndNoInvisibleAsk). A scope key sent blank
// names nothing, and is refused by name rather than read as the key left out, which would widen a
// caller's read to every project.
func TestListOpenAsksTakesAtMostOneScope(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	for _, test := range []struct {
		target string
		code   string
	}{
		{"/api/v1/asks/open?author_session=%20%20", "INVALID_QUERY"},
		{"/api/v1/asks/open?project=", "INVALID_QUERY"},
		{"/api/v1/asks/open?author_session=session&project=TEST", "AUTHOR_SESSION_OR_PROJECT_CONFLICT"},
	} {
		response := sessionRequest(t, handler, http.MethodGet, test.target, nil)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s, want 400", test.target, response.Code, response.Body.String())
			continue
		}
		body := decodeBody[struct {
			Code string `json:"code"`
		}](t, response)
		if body.Code != test.code {
			t.Errorf("%s: code=%q, want %q", test.target, body.Code, test.code)
		}
	}
}

// openAsksRows reads GET /api/v1/asks/open at query as login, failing on anything but a 200.
func openAsksRows(t *testing.T, handler http.Handler, query url.Values, login string) []openAsk {
	t.Helper()
	target := "/api/v1/asks/open"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s as %s: status=%d body=%s", target, login, response.Code, response.Body.String())
	}
	return decodeBody[openAsksResponse](t, response).Asks
}

// askIDsOf is the ids of rows, sorted, so two reads that order their rows by different rules
// are compared by what they hold.
func askIDsOf(rows []openAskRow) []string {
	ids := make([]string, len(rows))
	for index, row := range rows {
		ids[index] = row.ID
	}
	slices.Sort(ids)
	return ids
}

// openAskIDsOf is askIDsOf over the rows of GET /api/v1/asks/open, each the open-ask row it
// carries.
func openAskIDsOf(rows []openAsk) []string {
	listed := make([]openAskRow, len(rows))
	for index, row := range rows {
		listed[index] = row.openAskRow
	}
	return askIDsOf(listed)
}

// waitingOnFixture is three questions on two issues of one project, each waiting on one of the
// two people the test server records as signed in: one on alice's issue, one on bob's, and one on
// alice's issue that she handed to bob, which waits on bob and not on the issue's assignee.
type waitingOnFixture struct {
	onAlices, onBobs, handedToBob string
}

func seedWaitingOn(t *testing.T, handler http.Handler) waitingOnFixture {
	t.Helper()
	alices := createInteractionIssue(t, handler, "TEST", "Alice's issue", "A spec")
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST", "title": "Bob's issue", "assignee": "bob",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create bob's issue: status=%d body=%s", response.Code, response.Body.String())
	}
	bobs := decodeBody[model.Issue](t, response)
	seeded := waitingOnFixture{
		onAlices:    createIssueAsk(t, handler, alices.Key, "On alice's issue"),
		onBobs:      createIssueAsk(t, handler, bobs.Key, "On bob's issue"),
		handedToBob: createIssueAsk(t, handler, alices.Key, "Handed to bob"),
	}
	handAskOn(t, handler, seeded.handedToBob, "bob", "alice")
	return seeded
}

// ?waiting_on=<login> answers the asks that wait on that person - the waiting_on_person each row
// names, whom a hand-on moves - and the same asks the Inbox's ?assignee=<login> answers for them,
// whatever the casing the login is written in.
func TestOpenAsksWaitingOnALoginAnswersTheAsksThatWaitOnThatPerson(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	seeded := seedWaitingOn(t, handler)
	for _, test := range []struct {
		login, written string
		want           []string
	}{
		{"alice", "alice", []string{seeded.onAlices}},
		{"bob", "bob", []string{seeded.onBobs, seeded.handedToBob}},
		{"bob", "  BOB  ", []string{seeded.onBobs, seeded.handedToBob}},
	} {
		rows := openAsksRows(t, handler, url.Values{"waiting_on": {test.written}}, "alice")
		for _, row := range rows {
			if row.WaitingOnPerson != test.login {
				t.Errorf("waiting_on=%q answers %s, which waits on %q", test.written, row.ID, row.WaitingOnPerson)
			}
		}
		want := slices.Sorted(slices.Values(test.want))
		if got := openAskIDsOf(rows); !slices.Equal(got, want) {
			t.Errorf("waiting_on=%q answers %v, want %v", test.written, got, want)
		}
		if inbox := askIDsOf(inboxFor(t, handler, "alice", test.login)); !slices.Equal(inbox, want) {
			t.Errorf("the Inbox's assignee=%s answers %v, and waiting_on=%q answers %v", test.login, inbox, test.written, want)
		}
	}
}

// A waiting_on that names nobody the read could narrow to is refused by name: a blank value, a
// repeated key, a login nobody has signed in as, `unassigned`, which is such a login (the read
// narrows to one person and to nobody else), and `me`, which the read does not take, since it
// answers about any person and the caller's own mail is the one read's (GET /api/v1/me/inbox).
func TestOpenAsksRefuseABadWaitingOn(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	seedWaitingOn(t, handler)
	for _, query := range []url.Values{
		{"waiting_on": {""}},
		{"waiting_on": {"   "}},
		{"waiting_on": {"alice", "bob"}},
		{"waiting_on": {"nobody-signed-in"}},
		{"waiting_on": {"me"}},
		{"waiting_on": {"unassigned"}},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/open?"+query.Encode(), nil, "alice")
		var refusal struct{ Code, Error string }
		if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &refusal) != nil ||
			refusal.Code != "INVALID_QUERY" || !strings.HasPrefix(refusal.Error, "waiting_on ") {
			t.Errorf("?%s answers %d %s, want 400 INVALID_QUERY naming waiting_on", query.Encode(), response.Code, response.Body.String())
		}
	}
}

// With neither scope the read answers every project's open asks the caller may see, and no ask
// whose node is reserved for another author: the ask node's visibility predicate binds every row.
// The author the node is reserved for reads it, so what keeps it from the other reader is that
// predicate and nothing else of the read.
func TestOpenAsksWithNeitherScopeAnswerEveryVisibleProjectAndNoInvisibleAsk(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	test := createInteractionIssue(t, handler, "TEST", "A TEST issue", "A spec")
	other := createInteractionIssue(t, handler, "OTHER", "An OTHER issue", "A spec")
	onTest := createIssueAsk(t, handler, test.Key, "On TEST")
	onOther := createIssueAsk(t, handler, other.Key, "On OTHER")
	reserved := createIssueAsk(t, handler, other.Key, "Reserved for bob")
	author, err := json.Marshal(model.Actor{Kind: "user", ID: "bob"})
	if err != nil {
		t.Fatalf("encode the reserved node's author: %v", err)
	}
	if tag, err := database.Pool.Exec(context.Background(), `
		update graph_nodes set visibility = $3, made_by = $4::jsonb where kind = $1 and id = $2
	`, graphstore.KindAsk, reserved, graphstore.VisibilityAuthor, string(author)); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("reserve the ask node %s for bob: %v (%d rows)", reserved, err, tag.RowsAffected())
	}

	everyProject := openAskIDsOf(openAsksRows(t, handler, nil, "alice"))
	if want := slices.Sorted(slices.Values([]string{onTest, onOther})); !slices.Equal(everyProject, want) {
		t.Fatalf("alice's read with neither scope answers %v, want the TEST and OTHER asks %v and not the reserved %s",
			everyProject, want, reserved)
	}
	if oneProject := openAskIDsOf(openAsksRows(t, handler, url.Values{"project": {"OTHER"}}, "alice")); !slices.Equal(oneProject, []string{onOther}) {
		t.Fatalf("alice's read of OTHER answers %v, want %s alone", oneProject, onOther)
	}
	bobs := openAskIDsOf(openAsksRows(t, handler, nil, "bob"))
	if want := slices.Sorted(slices.Values([]string{onTest, onOther, reserved})); !slices.Equal(bobs, want) {
		t.Fatalf("bob's read with neither scope answers %v, want %v with the ask reserved for him", bobs, want)
	}
}

// One ask, read through the three reads that answer its row - GET /api/v1/inbox, the Ask entry of
// GET /api/v1/me/inbox and GET /api/v1/asks/open - answers the same row in each, field for field:
// the row is built once (openAskRow), and the open-asks read only adds its own four fields beside
// it. The ask carries a reply in its thread and the reader's snooze, so the row is not mostly
// nulls.
func TestTheOpenAskRowOfTheThreeReadsIsBuiltOnce(t *testing.T) {
	// Not parallel: it reads GET /api/v1/me/inbox for what its writes through the routes stated,
	// and that read answers only what stands at or below the event horizon (sinceHorizon), which a
	// test beside it holding a transaction id on the cluster (holdClusterTransactionID) holds back.
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "One row", "A spec")
	askID := createIssueAsk(t, handler, issue.Key, "Which row?")
	created(t, "alice's reply in the ask's thread", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "This one.", "ask_id": askID}, "alice"))
	if snoozed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+askID+"/snooze", map[string]any{
		"snoozed_until": time.Now().UTC().Add(3 * time.Hour).Format(time.RFC3339Nano),
	}, "alice"); snoozed.Code != http.StatusOK {
		t.Fatalf("snooze the ask: status=%d body=%s", snoozed.Code, snoozed.Body.String())
	}

	rowOf := func(what string, rows []map[string]json.RawMessage) map[string]json.RawMessage {
		t.Helper()
		for _, row := range rows {
			var id string
			if json.Unmarshal(row["id"], &id) == nil && id == askID {
				return row
			}
		}
		t.Fatalf("%s holds no row of %s", what, askID)
		return nil
	}
	read := func(target string) []byte {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
		return response.Body.Bytes()
	}
	inbox := rowOf("GET /api/v1/inbox", decodeJSONBytes[[]map[string]json.RawMessage](t, read("/api/v1/inbox?assignee=me")))
	var entries []map[string]json.RawMessage
	for _, entry := range decodeJSONBytes[struct {
		Entries []struct {
			Ask map[string]json.RawMessage `json:"ask"`
		} `json:"entries"`
	}](t, read("/api/v1/me/inbox?state=inbox")).Entries {
		if entry.Ask != nil {
			entries = append(entries, entry.Ask)
		}
	}
	one := rowOf("GET /api/v1/me/inbox", entries)
	open := rowOf("GET /api/v1/asks/open", decodeJSONBytes[struct {
		Asks []map[string]json.RawMessage `json:"asks"`
	}](t, read("/api/v1/asks/open?waiting_on=alice")).Asks)

	if string(inbox["snoozed_until"]) == "null" || string(inbox["last_reply"]) == "null" {
		t.Fatalf("the Inbox's row carries snoozed_until %s and last_reply %s, want both set", inbox["snoozed_until"], inbox["last_reply"])
	}
	for _, other := range []struct {
		what  string
		row   map[string]json.RawMessage
		extra []string
	}{
		{"GET /api/v1/me/inbox", one, nil},
		{"GET /api/v1/asks/open", open, []string{"age_seconds", "human_replied", "owner", "ref"}},
	} {
		if got, want := slices.Sorted(maps.Keys(other.row)), slices.Sorted(slices.Values(append(slices.Collect(maps.Keys(inbox)), other.extra...))); !slices.Equal(got, want) {
			t.Errorf("%s's row carries %v, want the Inbox row's fields and %v: %v", other.what, got, other.extra, want)
		}
		for field, value := range inbox {
			if !bytes.Equal(compactJSON(t, other.row[field]), compactJSON(t, value)) {
				t.Errorf("%s's row carries %s %s, and the Inbox's %s", other.what, field, other.row[field], value)
			}
		}
	}
}

// decodeJSONBytes decodes body as a T, failing the test on a body that is not one.
func decodeJSONBytes[T any](t *testing.T, body []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return value
}

// compactJSON is raw with its insignificant whitespace taken out, so two encodings of one value
// compare equal.
func compactJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return compacted.Bytes()
}

func TestTargetedReadsRejectNonUUIDIDsWithA400(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Short ids", "A spec")
	for _, test := range []struct {
		target string
		code   string
		text   string
	}{
		{target: "/api/v1/asks/7430fab", code: "ASK_ID_INPUT", text: "ask id must be a whole uuid or a unique prefix of 8 characters or more"},
		{target: "/api/v1/comments/7430fab", code: "COMMENT_ID_INPUT", text: "comment id must be a whole uuid or a unique prefix of 8 characters or more"},
		{target: "/api/v1/issues/" + issue.Key + "/messages/7430fab", code: "MESSAGE_ID_INPUT", text: "message id must be a whole uuid or a unique prefix of 8 characters or more"},
	} {
		response := sessionRequest(t, handler, http.MethodGet, test.target, nil)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) || !strings.Contains(response.Body.String(), test.text) {
			t.Errorf("%s: status=%d body=%s, want 400 %s", test.target, response.Code, response.Body.String(), test.code)
		}
	}
}

// An id written in RFC 4122's URN form is a whole uuid to Go's uuid.Parse, so requireUUIDPath
// lets it through and the prefix layer leaves it as written (text.IsIDPrefix refuses the colons).
// What the route must not do is send those characters to Postgres, which refuses them
// ("invalid input syntax for type uuid", SQLSTATE 22P02) and makes the read answer 500. The same
// defect was fixed for a document's reference key in this change by sending the canonical uuid;
// an ask's, a comment's and a message's own routes must answer as the canonical id answers — the
// item when it exists, and that route's own 404 when nothing carries it.
func TestTargetedReadsReadAUrnUuidIDAsTheCanonicalID(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "URN ids", "A spec")
	askID := createIssueAsk(t, handler, issue.Key, "Which store do we use?")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "A message to read by its id.", "actor": sessionActor(),
	}, "alice")
	const unknown = "00000000-0000-4000-8000-000000000000"
	for _, test := range []struct {
		target string
		want   int
	}{
		{target: "/api/v1/asks/urn:uuid:" + askID, want: http.StatusOK},
		{target: "/api/v1/messages/urn:uuid:" + message.ID, want: http.StatusOK},
		{target: "/api/v1/asks/urn:uuid:" + unknown, want: http.StatusNotFound},
		{target: "/api/v1/comments/urn:uuid:" + unknown, want: http.StatusNotFound},
	} {
		response := sessionRequest(t, handler, http.MethodGet, test.target, nil)
		if response.Code != test.want {
			t.Errorf("%s: status=%d body=%s, want %d", test.target, response.Code, response.Body.String(), test.want)
		}
	}
}

// The same spelling on a route the prefix layer does not wrap. canonicalizeUUIDPath runs only
// for the 48 routes whose ID names a kind to resolve, so a handoff's, a broadcast's and a Delivery
// session group's id still reaches Postgres as written and answers 500 where the canonical
// spelling answers that route's own refusal. Each pair below is measured over live HTTP: the
// canonical id's answer first, which is what the URN spelling must give too.
func TestRoutesThatResolveNoPrefixReadAUrnUuidIDAsTheCanonicalID(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	const unknown = "00000000-0000-4000-8000-000000000000"
	if response := sessionRequest(t, handler, http.MethodGet, "/api/v1/handoffs/urn:uuid:"+unknown, nil); response.Code != http.StatusNotFound {
		t.Errorf("handoff by urn:uuid: status=%d body=%s, want 404 as the canonical id answers", response.Code, response.Body.String())
	}
	for _, test := range []struct {
		method string
		target string
		body   any
	}{
		{method: http.MethodGet, target: "/api/v1/broadcasts/urn:uuid:" + unknown},
		{method: http.MethodPatch, target: "/api/v1/delivery/session-groups/urn:uuid:" + unknown, body: map[string]any{"name": "A group that does not exist"}},
	} {
		response := dispatchRequest(t, handler, test.method, test.target, test.body, "alice")
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s: status=%d body=%s, want 404 as the canonical id answers", test.method, test.target, response.Code, response.Body.String())
		}
	}
}

func TestResolveAskRejectsNumberedIDWithClientGuidance(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/3/resolve", map[string]any{
		"kind": "resolved", "reason": "Resolved elsewhere", "actor": sessionActor(),
	})
	const want = "ask id must be a whole uuid or a unique prefix of 8 characters or more"
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"ASK_ID_INPUT"`) || !strings.Contains(response.Body.String(), want) {
		t.Fatalf("resolve numbered ask: status=%d body=%s, want 400 ASK_ID_INPUT %q", response.Code, response.Body.String(), want)
	}
}

// Every ask indexed from a document block names that document on the wire, however many
// asks the issue detail lists at once.
func TestIssueDetailAttachesTheBlockArtifactToEveryBlockAsk(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Three decisions", strings.Join([]string{
		"Context\n",
		":::ask{#ask-one urgency=\"med\" multiple=\"false\"}\nShip one?\n:::\n",
		":::ask{#ask-two urgency=\"med\" multiple=\"false\"}\nShip two?\n:::\n",
		":::ask{#ask-three urgency=\"med\" multiple=\"false\"}\nShip three?\n:::\n",
	}, "\n"))
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ask-one", "Ship one?")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ask-two", "Ship two?")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ask-three", "Ship three?")

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("get issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	openAsks := decodeBody[struct {
		OpenAsks []model.Ask `json:"open_asks"`
	}](t, detail).OpenAsks
	if len(openAsks) != 3 {
		t.Fatalf("issue detail lists %d open asks, want 3: %s", len(openAsks), detail.Body.String())
	}
	for _, ask := range openAsks {
		if ask.BlockID == nil || ask.BlockArtifact == nil {
			t.Fatalf("open ask %s: block_id=%v block_artifact=%v, want both", ask.ID, ask.BlockID, ask.BlockArtifact)
		}
		want := model.AskBlockArtifact{ID: issue.PrimaryArtifactID, Slug: "spec", Primary: true}
		if *ask.BlockArtifact != want {
			t.Fatalf("open ask %s block_artifact = %#v, want %#v", *ask.BlockID, *ask.BlockArtifact, want)
		}
	}
}
