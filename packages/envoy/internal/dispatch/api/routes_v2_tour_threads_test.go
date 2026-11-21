package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

func init() { addTourSteps("threads", threadsTourSteps) }

// threadsTourSteps are the route tour's steps of the comments, messages, broadcasts and handoffs:
// each route, each of the four states of PUT /api/v2/comments/{id}/state, and the
// reads with what include= and the filters add.
func threadsTourSteps() []tourStep {
	alice := tourPerson("alice")
	// The session the tour's listener delivers to, writing under the shared agent token as an
	// agent's session does, naming itself in actor.
	recipient := tourBearer("agent-token")
	asRecipient := map[string]any{"kind": "session", "id": tourRecipient}
	commentMissing := []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.CommentNotFound}}
	messageMissing := []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.MessageNotFound}}
	issueMissing := []tourMissing{{Param: "key", Value: "NOPE-404", Code: apicode.IssueNotFound}}
	// on is a setup that makes the object param names with object and sends query, body and as.
	on := func(param string, object func(*testing.T, *routeTour) string, query url.Values, body any, as tourCaller) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			return tourRequest{Path: map[string]string{param: object(t, tour)}, Query: query, Body: body, As: as}
		}
	}
	comment := func(t *testing.T, tour *routeTour) string { return tour.comment(t, tour.issue(t)) }
	mentioned := func(t *testing.T, tour *routeTour) string { return tour.mentionedComment(t, tour.issue(t)) }
	targeted := func(t *testing.T, tour *routeTour) string { return tour.targetedMessage(t, tour.issue(t)) }
	issueKey := func(t *testing.T, tour *routeTour) string { return tour.issue(t).Key }
	commentState := func(name string, object func(*testing.T, *routeTour) string, body map[string]any) tourStep {
		return tourStep{
			Method: http.MethodPut, Pattern: "/comments/{id}/state", Name: name, Status: http.StatusOK,
			Setup: on("id", object, nil, body, alice), Missing: commentMissing,
		}
	}
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "/comments/{id}", Status: http.StatusOK,
			Setup: on("id", comment, nil, nil, alice), Missing: commentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/comments/{id}", Name: "with its blocks", Status: http.StatusOK,
			Setup: on("id", comment, url.Values{"include": {"blocks"}}, nil, alice), Missing: commentMissing,
		},
		{
			Method: http.MethodPatch, Pattern: "/comments/{id}", Status: http.StatusOK,
			Setup: on("id", comment, nil, map[string]any{"body": "A toured comment, edited."}, alice), Missing: commentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/comments/{id}/deliveries", Status: http.StatusCreated,
			Setup: on("id", mentioned, nil, map[string]any{"delivery": "btw"}, alice), Missing: commentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/comments/{id}/replies", Status: http.StatusCreated,
			Setup: on("id", mentioned, nil, map[string]any{
				"actor": asRecipient, "attempt": 1, "body": "The tour can ship.",
			}, recipient),
			Missing: commentMissing,
		},
		commentState("resolved", comment, map[string]any{"state": "resolved"}),
		commentState("open", func(t *testing.T, tour *routeTour) string { return tour.resolvedComment(t, tour.issue(t)) },
			map[string]any{"state": "open"}),
		commentState("accepted", func(t *testing.T, tour *routeTour) string { return tour.suggestion(t, tour.issue(t), true) },
			map[string]any{"state": "accepted"}),
		commentState("rejected", func(t *testing.T, tour *routeTour) string { return tour.suggestion(t, tour.issue(t), false) },
			map[string]any{"state": "rejected"}),
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/comments", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.comment(t, issue)
				return tourRequest{Path: map[string]string{"key": issue.Key}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/comments", Name: "one document's, a page of one", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.suggestion(t, issue, true)
				return tourRequest{
					Path: map[string]string{"key": issue.Key}, Query: url.Values{"artifact": {issue.Document}, "limit": {"1"}}, As: alice,
				}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/comments", Status: http.StatusCreated,
			Setup: on("key", issueKey, nil, map[string]any{"body": "A comment the tour leaves on an issue."}, alice), Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/messages/{id}", Status: http.StatusOK,
			Setup: on("id", targeted, nil, nil, alice), Missing: messageMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/messages/{id}", Name: "a direct conversation read as its session, with the blocks",
			Status: http.StatusOK,
			Setup: on("id", func(t *testing.T, tour *routeTour) string { return tour.directMessage(t) },
				url.Values{"session": {tourRecipient}, "include": {"blocks"}}, nil, recipient),
			Missing: messageMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/messages/{id}/deliveries", Status: http.StatusCreated,
			Setup: on("id", targeted, nil, map[string]any{"delivery": "btw"}, alice), Missing: messageMissing,
		},
		{
			Method: http.MethodPut, Pattern: "/messages/{id}/deliveries/{attempt}/state", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{
					Path: map[string]string{"id": tour.directMessage(t), "attempt": "1"},
					Body: map[string]any{"state": "accepted", "actor": asRecipient}, As: recipient,
				}
			},
			Missing: []tourMissing{
				{Param: "id", Value: uuid.NewString(), Code: apicode.MessageNotFound},
				{Param: "attempt", Value: "9", Code: apicode.DeliveryNotFound},
			},
		},
		{
			Method: http.MethodPost, Pattern: "/messages/{id}/replies", Status: http.StatusCreated,
			Setup:   on("id", targeted, nil, map[string]any{"actor": asRecipient, "attempt": 1, "body": "Green."}, recipient),
			Missing: messageMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/messages/{id}/replies", Name: "a follow-up", Status: http.StatusCreated,
			Setup: on("id", func(t *testing.T, tour *routeTour) string { return tour.answeredMessage(t, tour.issue(t)) },
				url.Values{"follow_up": {"true"}}, map[string]any{"actor": asRecipient, "attempt": 1, "body": "Still green."}, recipient),
			Missing: messageMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/messages", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.targetedMessage(t, issue)
				return tourRequest{Path: map[string]string{"key": issue.Key}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/messages", Status: http.StatusCreated,
			Setup: on("key", issueKey, nil, map[string]any{
				"body": "A message the tour posts.", "target": "session:" + tourRecipient, "delivery": "btw",
			}, alice),
			Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/agents/{session_id}/messages", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				tour.directMessage(t)
				return tourRequest{Path: map[string]string{"session_id": tourRecipient}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/agents/{session_id}/messages", Name: "the unread ones", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"session_id": tourRecipient}, Query: url.Values{"unread": {"true"}}, As: alice}
			},
		},
		{
			Method: http.MethodPost, Pattern: "/agents/{session_id}/messages", Status: http.StatusCreated,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{
					Path: map[string]string{"session_id": tourRecipient},
					Body: map[string]any{"body": "A BTW the tour sends.", "delivery": "btw"}, As: alice,
				}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/broadcasts", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				tour.broadcast(t)
				return tourRequest{As: alice}
			},
		},
		{
			Method: http.MethodPost, Pattern: "/broadcasts", Status: http.StatusCreated,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{
					Body: map[string]any{"body": "The tour passes through.", "delivery": "btw", "session_ids": []string{tourRecipient}},
					As:   alice,
				}
			},
			// Each recipient's delivery runs behind the first answer, and a repeat answers its
			// attempts as they stand (replayBroadcast), the attempt the first answer showed pending
			// sent by then.
			ReplayMoves: []string{"recipients.*.message.deliveries"},
		},
		{
			Method: http.MethodGet, Pattern: "/broadcasts/{id}", Status: http.StatusOK,
			Setup:   on("id", func(t *testing.T, tour *routeTour) string { return tour.broadcast(t) }, nil, nil, alice),
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.BroadcastNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/handoffs/{id}", Status: http.StatusOK,
			Setup:   on("id", func(t *testing.T, tour *routeTour) string { return tour.handoff(t, tour.issue(t)) }, nil, nil, alice),
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.HandoffNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/handoffs", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.handoff(t, issue)
				return tourRequest{Path: map[string]string{"key": issue.Key}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/handoffs", Name: "a phase's latest, with its body", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.handoff(t, issue)
				tour.handoff(t, issue)
				return tourRequest{
					Path:  map[string]string{"key": issue.Key},
					Query: url.Values{"phase": {"plan"}, "sort": {"-version"}, "limit": {"1"}, "include": {"body"}}, As: alice,
				}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/handoffs", Status: http.StatusCreated,
			Setup:   on("key", issueKey, nil, map[string]any{"actor": asRecipient, "phase": "plan", "body": `{"plan":"the tour"}`}, recipient),
			Missing: issueMissing,
		},
	}
}

// resolvedComment makes a comment on issue by alice and resolves it, answering its id.
func (tour *routeTour) resolvedComment(t *testing.T, issue tourIssue) string {
	t.Helper()
	id := tour.comment(t, issue)
	if resolved := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/comments/"+id+"/resolve", nil, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve comment: %d %s", resolved.Code, resolved.Body.String())
	}
	return id
}

// suggestion makes a suggestion on issue by alice, answering its id once the spec owes no
// settlement: anchored to a quote of the issue's spec, which an accept applies, or floating, which
// a reject alone decides.
func (tour *routeTour) suggestion(t *testing.T, issue tourIssue, anchored bool) string {
	t.Helper()
	input := map[string]any{"body": "Say it plainer.", "suggestion": map[string]any{"replace_with": "A paragraph"}}
	if anchored {
		input["anchor"] = map[string]any{"artifact": "spec", "quote": "A passage"}
	}
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", input, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create suggestion: %d %s", created.Code, created.Body.String())
	}
	awaitSettled(t, tour.database, issue.Document)
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
}

// mentionedComment makes a comment on issue by alice mentioning tourRecipient, answering its id
// once its mention's first attempt has settled: the mention sender sends it after the create
// answers, and a step's write that lands between the two fingerprints of unknown-keys would read
// as the refused request's.
func (tour *routeTour) mentionedComment(t *testing.T, issue tourIssue) string {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Can the tour ship?", "mentions": []map[string]any{{"target": "session:" + tourRecipient}}, "delivery": "btw",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create mentioned comment: %d %s", created.Code, created.Body.String())
	}
	id := decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
	settledCommentDeliveries(t, tour.database, id)
	return id
}

// targetedMessage posts alice's message on issue to tourRecipient, answering its id; the post
// answers once the first attempt has settled.
func (tour *routeTour) targetedMessage(t *testing.T, issue tourIssue) string {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Is the tour green?", "target": "session:" + tourRecipient, "delivery": "btw",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("post targeted message: %d %s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
}

// answeredMessage is targetedMessage with tourRecipient's reply to its first attempt, answering the
// message's id.
func (tour *routeTour) answeredMessage(t *testing.T, issue tourIssue) string {
	t.Helper()
	id := tour.targetedMessage(t, issue)
	if replied := sessionRequest(t, tour.handler, http.MethodPost, "/api/v1/messages/"+id+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": tourRecipient}, "attempt": 1, "body": "Green.",
	}); replied.Code != http.StatusCreated {
		t.Fatalf("reply to message: %d %s", replied.Code, replied.Body.String())
	}
	return id
}

// directMessage sends alice's Aside to tourRecipient, answering its id; the send answers once its
// first attempt has settled. It is a person's direct message, the one kind of message its
// session takes as its own turn.
func (tour *routeTour) directMessage(t *testing.T) string {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/agents/"+tourRecipient+"/messages", map[string]any{
		"body": "Where does the tour go next?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("send direct message: %d %s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
}

// broadcast sends alice's broadcast to tourRecipient, answering its id once the delivery that runs
// behind the send's answer has settled.
func (tour *routeTour) broadcast(t *testing.T) string {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "The tour is passing through.", "delivery": "btw", "session_ids": []string{tourRecipient},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("send broadcast: %d %s", created.Code, created.Body.String())
	}
	awaitBackgroundWrites(t, tour.served)
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
}

// handoff records tourRecipient's plan handoff on issue, answering its id.
func (tour *routeTour) handoff(t *testing.T, issue tourIssue) string {
	t.Helper()
	created := sessionRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/handoffs/plan", map[string]any{
		"actor": map[string]any{"kind": "session", "id": tourRecipient}, "body": `{"plan":"the tour"}`,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("record handoff: %d %s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
}

// An issue's comments read through /api/v1 and then /api/v2, one after the other and then again,
// answer each version its own body - a bare array and an envelope - though the second round of each
// is the answer that version's route kept (list_answers.go): the kept answers are filed under each
// version's own route, and a validator one version gave is no validator of the other's answer.
// A missing issue's list is 404 ISSUE_NOT_FOUND under
// /api/v2, where /api/v1 answers it the empty list as it always has.
func TestTheIssueCommentListAnswersEachVersionItsOwnShape(t *testing.T) {
	tour := newRouteTour(t)
	issue := tour.issue(t)
	comment := tour.comment(t, issue)
	read := func(t *testing.T, version, key, validator string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/"+version+"/issues/"+key+"/comments", nil)
		request.Header.Set("X-Dispatch-User", "alice")
		if validator != "" {
			request.Header.Set("If-None-Match", validator)
		}
		response := httptest.NewRecorder()
		tour.handler.ServeHTTP(response, request)
		return response
	}
	type listed struct {
		ID string `json:"id"`
	}
	var v1Tag, v2Tag string
	for round, kept := range []string{"made", "kept"} {
		answered := tour.served.listAnswers.answered()
		v1 := read(t, "v1", issue.Key, "")
		var rows []listed
		if v1.Code != http.StatusOK || json.Unmarshal(v1.Body.Bytes(), &rows) != nil || len(rows) != 1 || rows[0].ID != comment {
			t.Fatalf("round %d: GET /api/v1 answered %d %s, want a bare array of the one comment %s", round, v1.Code, v1.Body.String(), comment)
		}
		v2 := read(t, "v2", issue.Key, "")
		var envelope struct {
			Items  []listed `json:"items"`
			Total  int      `json:"total"`
			Limit  int      `json:"limit"`
			Offset int      `json:"offset"`
		}
		if v2.Code != http.StatusOK || json.Unmarshal(v2.Body.Bytes(), &envelope) != nil ||
			len(envelope.Items) != 1 || envelope.Items[0].ID != comment || envelope.Total != 1 || envelope.Limit != 50 || envelope.Offset != 0 {
			t.Fatalf("round %d: GET /api/v2 answered %d %s, want an envelope of the one comment %s, total 1, limit 50, offset 0",
				round, v2.Code, v2.Body.String(), comment)
		}
		if kept == "kept" && tour.served.listAnswers.answered() != answered+2 {
			t.Fatalf("the second round wrote %d kept answers, want each version's own", tour.served.listAnswers.answered()-answered)
		}
		v1Tag, v2Tag = v1.Header().Get("ETag"), v2.Header().Get("ETag")
	}
	if v1Tag == "" || v1Tag == v2Tag {
		t.Fatalf("the two versions answered the ETags %q and %q, want one of each version's own", v1Tag, v2Tag)
	}
	if crossed := read(t, "v2", issue.Key, v1Tag); crossed.Code != http.StatusOK {
		t.Fatalf("GET /api/v2 under /api/v1's validator answered %d, want 200 with its own body", crossed.Code)
	}
	if current := read(t, "v2", issue.Key, v2Tag); current.Code != http.StatusNotModified {
		t.Fatalf("GET /api/v2 under its own validator answered %d, want 304", current.Code)
	}

	missing := read(t, "v2", "NOPE-9", "")
	if code, _ := refusalOf(t, missing); missing.Code != http.StatusNotFound || code != "ISSUE_NOT_FOUND" {
		t.Fatalf("GET /api/v2/issues/NOPE-9/comments answered %d %s, want 404 ISSUE_NOT_FOUND", missing.Code, missing.Body.String())
	}
	if v1Missing := read(t, "v1", "NOPE-9", ""); v1Missing.Code != http.StatusOK || v1Missing.Body.String() != "[]\n" {
		t.Fatalf("GET /api/v1/issues/NOPE-9/comments answered %d %s, want 200 [] as before", v1Missing.Code, v1Missing.Body.String())
	}
}
