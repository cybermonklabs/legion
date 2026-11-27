package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settleAskAsTheCatchUp writes change on the ask askID as the decisions step's catch-up states a
// write a process of the release before made (docs' applyCaughtUpEvent): through the one writer of
// an ask's answer state (asks.Settle), on the ask's node and its row, in a transaction of its own.
func settleAskAsTheCatchUp(t *testing.T, database *store.Store, askID string, change asks.Change) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the catch-up's write: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := asks.Settle(ctx, tx, askID, change); err != nil {
		t.Fatalf("state the answer state of ask %s: %v", askID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the catch-up's write: %v", err)
	}
}

// describedAnswer is answer as JSON, for a failure to print.
func describedAnswer(answer *model.AskAnswer) string {
	encoded, _ := json.Marshal(answer)
	return string(encoded)
}

// Every read of an answer reads the ask's node (R3), as the text routes' asks do (docs'
// TestTheTextsAsksReadTheAnswerOffTheAsksNodeNotItsRow): the rows the document page draws each
// ask's state and answer from (GET /api/v1/issues/{key}/asks, askReadColumns), one ask (GET
// /api/v1/asks/{id}, the same columns), the spec's open questions on the Decisions read
// (specOpenQuestions) and a search hit's ask (attachAskStates). So does every read that picks asks
// by their answer state: the issue's asks by state (listIssueAsksQueryOpen and
// listIssueAsksQueryAnswered), the issue's ask page by state (askThreadPageQueryOpen and
// askThreadPageQueryAnswered), the open asks of the issue's detail (the first of them) and the
// Inbox (listInbox, askNodeStateOf). The node and the row differ only while a process of the
// release before has written the row and the decisions step's catch-up has not yet stated that
// write on the node: here the catch-up stated alice's answer on both (asks.Settle with
// asks.Recorded), and then the release before's reopen wrote the row open again, alone. Each
// reader answers alice's answer as the node holds it, the open questions and every read of open
// asks leave the ask out, and the issue's answered asks hold it.
func TestEveryReadOfAnAnswerReadsTheAsksNodeNotItsRow(t *testing.T) {
	t.Parallel()
	handler, database, _ := blockAskHandlerWithDocuments(t)
	issue, askID := seedBlockAsk(t, handler, "Readers of the ask node", "decision", transportAsk, "Which transport?")
	ctx := context.Background()
	words := "REST, as the platform speaks."
	answer := model.AskAnswer{User: "alice", Selected: []string{"REST"}, Text: &words, At: time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)}
	change := asks.Recorded(asks.StateAnswered, &answer, nil)
	change.Who, change.At, change.Event = model.Actor{Kind: "user", ID: "alice"}, answer.At, graphstore.NoEvent
	settleAskAsTheCatchUp(t, database, askID, change)
	if tag, err := database.Pool.Exec(ctx, `update asks set state = 'open', answer = null where id = $1`, askID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("reopen the row of ask %s alone, as the release before writes it: %v", askID, err)
	}
	if state, held, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindAsk, askID, graphstore.TupleAnswerState); err != nil || !held || state != asks.StateAnswered {
		t.Fatalf("the node of ask %s holds answer_state %q (held %t, %v), want answered: the readers below would prove nothing", askID, state, held, err)
	}

	nodes := func(reader string, state string, read *model.AskAnswer) {
		t.Helper()
		if state != asks.StateAnswered || read == nil || read.User != answer.User || !slices.Equal(read.Selected, answer.Selected) ||
			read.Text == nil || *read.Text != words || !read.At.Equal(answer.At) {
			t.Errorf("%s reads ask %s %s with answer %s, want answered with alice's answer as its node holds it, %s",
				reader, askID, state, describedAnswer(read), describedAnswer(&answer))
		}
	}

	const rows = "the document page's ask rows (GET /api/v1/issues/{key}/asks, askReadColumns)"
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=all", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", rows, listed.Code, listed.Body.String())
	}
	found := false
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		if ask.ID == askID {
			found = true
			nodes(rows, ask.State, ask.Answer)
		}
	}
	if !found {
		t.Errorf("%s list no ask %s", rows, askID)
	}

	one := readBlockAsk(t, handler, askID)
	nodes("one ask (GET /api/v1/asks/{id}, askReadColumns)", one.State, one.Answer)

	const questions = "the spec's open questions (GET /api/v1/issues/{key}/decisions open_questions, specOpenQuestions)"
	decided := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/decisions", nil, "alice")
	if decided.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", questions, decided.Code, decided.Body.String())
	}
	for _, question := range decodeBody[struct {
		OpenQuestions []model.Ask `json:"open_questions"`
	}](t, decided).OpenQuestions {
		if question.ID == askID {
			t.Errorf("%s hold ask %s, %s, which its node holds answered by alice", questions, askID, question.State)
		}
	}

	const hit = "a search hit's ask (GET /api/v1/search, attachAskStates)"
	searched := rawHitAsk(t, rawSearchHit(t, rawSearchResults(t, searchRequest(t, handler, "q=transport&limit=50")), "ask", askID))
	var state string
	var read *model.AskAnswer
	if err := json.Unmarshal(searched["state"], &state); err != nil {
		t.Fatalf("%s: decode the state %s: %v", hit, searched["state"], err)
	}
	if raw, held := searched["answer"]; held {
		read = new(model.AskAnswer)
		if err := json.Unmarshal(raw, read); err != nil {
			t.Fatalf("%s: decode the answer %s: %v", hit, raw, err)
		}
	}
	nodes(hit, state, read)

	for _, list := range []struct {
		reader, state string
		holds         bool
	}{
		{"the issue's open asks (GET /api/v1/issues/{key}/asks?state=open, listIssueAsksQueryOpen)", "open", false},
		{"the issue's answered asks (GET /api/v1/issues/{key}/asks?state=answered, listIssueAsksQueryAnswered)", "answered", true},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state="+list.state, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", list.reader, response.Code, response.Body.String())
		}
		held := false
		for _, ask := range decodeBody[[]model.Ask](t, response) {
			if ask.ID == askID {
				held = true
				nodes(list.reader, ask.State, ask.Answer)
			}
		}
		if held != list.holds {
			t.Errorf("%s hold ask %s: %t, want %t: its node holds it answered, though its row reads open", list.reader, askID, held, list.holds)
		}
	}
	for _, page := range []struct {
		reader, state string
		holds         bool
	}{
		{"the issue's open ask page (GET /api/v1/issues/{key}/asks?order=activity&state=open, askThreadPageQueryOpen)", "open", false},
		{"the issue's answered ask page (GET /api/v1/issues/{key}/asks?order=activity&state=answered, askThreadPageQueryAnswered)", "answered", true},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?order=activity&state="+page.state, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", page.reader, response.Code, response.Body.String())
		}
		held := false
		for _, ask := range decodeBody[struct {
			Threads []model.Ask `json:"threads"`
		}](t, response).Threads {
			if ask.ID == askID {
				held = true
				nodes(page.reader, ask.State, ask.Answer)
			}
		}
		if held != page.holds {
			t.Errorf("%s hold ask %s: %t, want %t: its node holds it answered, though its row reads open", page.reader, askID, held, page.holds)
		}
	}

	const detail = "the issue detail's open asks (GET /api/v1/issues/{key} open_asks, listIssueAsksQueryOpen)"
	detailed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detailed.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", detail, detailed.Code, detailed.Body.String())
	}
	for _, ask := range decodeBody[struct {
		OpenAsks []model.Ask `json:"open_asks"`
	}](t, detailed).OpenAsks {
		if ask.ID == askID {
			t.Errorf("%s hold ask %s, %s, which its node holds answered by alice", detail, askID, ask.State)
		}
	}

	const inbox = "the Inbox's open asks (GET /api/v1/inbox, listInbox)"
	listed = dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", inbox, listed.Code, listed.Body.String())
	}
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		if ask.ID == askID {
			t.Errorf("%s hold ask %s, %s, which its node holds answered by alice", inbox, askID, ask.State)
		}
	}
}

// The rows a person's own pages draw an answer on read it off the ask's node too (R3): a person's
// answers (GET /api/v1/me/answers, listMyAnswers: the ask's state, and whether the answer is the
// one the ask holds now), the Reply row whose newest write is an ask's answer (GET
// /api/v1/me/replies, answerReplyRows: the answerer and the answer's line, and the unread count
// that counts the answer, myRepliesPageRest), and a document's asks by state (GET
// /api/v1/artifacts/{id}/asks?state=, listArtifactAsksQueryOpen and listArtifactAsksQueryAnswered).
// So do the reads that pick asks by their state across owners: the open asks the Replies list
// leaves out for the Inbox (myRepliesOwner), a project's open asks (GET /api/v1/asks/open,
// listOpenAsks) and a document's open approval request (GET /api/v1/artifacts/{id} approval,
// openApprovalAsksStatement); and a session's open asks a write advises it of (advice
// your_open_asks). Here alice answered bob's ask on an issue, her own ask on a project document, a
// session's ask on the issue and a session's request to approve the document (asking for
// changes), and bob answered alice's own ask on the issue, each through the answer route, which
// states each answer on the node and on the row; then the release before's reopen wrote each row
// open again, alone.
func TestEveryAnswerAPersonsPagesShowReadsTheAsksNode(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "NODES", "Answers on their nodes", "A spec")
	created := func(what string, response *httptest.ResponseRecorder) string {
		t.Helper()
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
		return decodeBody[model.Ask](t, response).ID
	}
	regions := []map[string]string{{"label": "eu-west-1"}, {"label": "us-east-1"}}
	issueAsk := created("bob's ask", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which region?", "options": regions,
	}, "bob"))
	aliceAsk := created("alice's ask", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which zone?", "options": regions,
	}, "alice"))
	sessionAsk := created("the session's ask", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which cache?", "actor": sessionActor(),
	}))
	document := createProjectDocument(t, handler, "NODES", "Runbook", "# Runbook\n")
	documentAsk := created("alice's ask on the document", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "Which shelf?", "options": []map[string]string{{"label": "Top"}, {"label": "Bottom"}},
	}, "alice"))
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("the session's approval request: status=%d body=%s", requested.Code, requested.Body.String())
	}
	approvalAsk := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	for _, answer := range []struct {
		id, by string
		body   map[string]any
	}{
		{issueAsk, "alice", map[string]any{"selected": []string{"eu-west-1"}, "text": "Closest to the users."}},
		{aliceAsk, "bob", map[string]any{"selected": []string{"us-east-1"}}},
		{sessionAsk, "alice", map[string]any{"text": "The page cache."}},
		{documentAsk, "alice", map[string]any{"selected": []string{"Top"}, "text": "Within reach."}},
		{approvalAsk, "alice", map[string]any{"selected": []string{"Request changes"}, "text": "Say which shelf."}},
	} {
		if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answer.id+"/answer", answer.body, answer.by); answered.Code != http.StatusOK {
			t.Fatalf("%s's answer to ask %s: status=%d body=%s", answer.by, answer.id, answered.Code, answered.Body.String())
		}
		if tag, err := database.Pool.Exec(ctx, `update asks set state = 'open', answer = null where id = $1`, answer.id); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("reopen the row of ask %s alone, as the release before writes it: %v", answer.id, err)
		}
	}

	const mine = "a person's answers (GET /api/v1/me/answers, listMyAnswers)"
	page := getMyAnswers(t, handler, "alice", "")
	for _, id := range []string{issueAsk, documentAsk} {
		found := false
		for _, row := range page.Rows {
			if row.Kind == "answer" && row.AskID == id {
				found = true
				if row.AskState != asks.StateAnswered || !row.Current {
					t.Errorf("%s show ask %s %s with alice's answer current %t, want answered with her answer current, as its node holds it",
						mine, id, row.AskState, row.Current)
				}
			}
		}
		if !found {
			t.Errorf("%s hold no answer of alice's to ask %s", mine, id)
		}
	}

	const reply = "the Reply row whose newest write is the ask's answer (GET /api/v1/me/replies, answerReplyRows)"
	row := replyRow(t, getMyReplies(t, handler, "bob", ""), issueAsk)
	if row.Reply.Kind != "answer" || row.Reply.Author != (model.Actor{Kind: "user", ID: "alice"}) || row.Reply.FirstLine != "eu-west-1 - Closest to the users." {
		t.Errorf("%s shows %#v on ask %s, want alice's answer as its node holds it, %q", reply, row.Reply, issueAsk, "eu-west-1 - Closest to the users.")
	}
	const unread = "the Reply row's unread count (GET /api/v1/me/replies unread_count, myRepliesPageRest)"
	if row.UnreadCount != 1 {
		t.Errorf("%s reads %d on ask %s, want 1: alice's answer, as its node holds it", unread, row.UnreadCount, issueAsk)
	}
	const leftOut = "the open asks the Replies list leaves out for the Inbox (GET /api/v1/me/replies, myRepliesOwner)"
	if !listsThread(getMyReplies(t, handler, "alice", ""), aliceAsk) {
		t.Errorf("%s leave out alice's ask %s, which its node holds answered by bob, though its row reads open", leftOut, aliceAsk)
	}

	const project = "a project's open asks (GET /api/v1/asks/open?project=, listOpenAsks)"
	opened := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/open?project=NODES", nil, "alice")
	if opened.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", project, opened.Code, opened.Body.String())
	}
	for _, ask := range decodeBody[struct {
		Asks []struct {
			ID string `json:"id"`
		} `json:"asks"`
	}](t, opened).Asks {
		if slices.Contains([]string{issueAsk, aliceAsk, sessionAsk, documentAsk, approvalAsk}, ask.ID) {
			t.Errorf("%s hold ask %s, which its node holds answered, though its row reads open", project, ask.ID)
		}
	}

	const approval = "a document's open approval request (GET /api/v1/artifacts/{id} approval, openApprovalAsksStatement)"
	if state := approvalStateOf(t, handler, document.ID); state != "changes_requested" {
		t.Errorf("%s reads document %s %s, want changes_requested: alice answered request %s, as its node holds it, though its row reads open",
			approval, document.ID, state, approvalAsk)
	}

	const advised = "a session's open asks a write advises it of (advice your_open_asks)"
	message := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{"body": "Noted.", "actor": sessionActor()})
	for _, ask := range adviceFromResponse(t, message.Code, message.Body.String()).YourOpenAsks {
		if ask.ID == sessionAsk {
			t.Errorf("%s hold ask %s, which its node holds answered by alice, though its row reads open", advised, sessionAsk)
		}
	}

	for _, list := range []struct {
		reader, state string
		holds         bool
	}{
		{"a document's open asks (GET /api/v1/artifacts/{id}/asks?state=open, listArtifactAsksQueryOpen)", "open", false},
		{"a document's answered asks (GET /api/v1/artifacts/{id}/asks?state=answered, listArtifactAsksQueryAnswered)", "answered", true},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document.ID+"/asks?state="+list.state, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", list.reader, response.Code, response.Body.String())
		}
		held := false
		for _, ask := range decodeBody[[]model.Ask](t, response) {
			if ask.ID == documentAsk {
				held = true
				if ask.State != asks.StateAnswered || ask.Answer == nil || ask.Answer.User != "alice" {
					t.Errorf("%s read ask %s %s with answer %s, want answered by alice as its node holds it", list.reader, documentAsk, ask.State, describedAnswer(ask.Answer))
				}
			}
		}
		if held != list.holds {
			t.Errorf("%s hold ask %s: %t, want %t: its node holds it answered, though its row reads open", list.reader, documentAsk, held, list.holds)
		}
	}
}

// The answered questions of other projects a question's related-item check offers are picked by,
// and carry, the answer their node holds (R3; relatedOtherProjects through askNodeStateOf,
// askNodeAnswerTextOf and askNodeAnswerOf): an answered question of XNODE whose row the release
// before's reopen wrote open again, alone, is still offered, with the answer alice gave.
func TestTheAnsweredQuestionsOfOtherProjectsAreReadOffTheirNodes(t *testing.T) {
	t.Parallel()
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	crossProjectAsker(t, handler, database, embedder, "XASKN")
	createSuggestionProject(t, handler, "XNODE")
	owner := createSearchIssue(t, handler, "XNODE", "Backlog runbook", "What to do when the backlog grows.")
	const words = "Empty the page store first; the index store costs more to rebuild."
	offered := seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?", words, 0.9, 2)
	refreshTermStats(t, database, "XASKN")
	processAllPending(t, database, embedder)
	if tag, err := database.Pool.Exec(context.Background(), `update asks set state = 'open', answer = null where id = $1`, offered); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("reopen the row of ask %s alone, as the release before writes it: %v", offered, err)
	}

	const leg = "the answered questions of other projects (relatedOtherProjects)"
	suggestions := crossProjectCheck(directServer(deps), "XASKN")
	var item *model.WriteSuggestion
	for index := range suggestions.OtherProjects {
		if suggestions.OtherProjects[index].ID == offered {
			item = &suggestions.OtherProjects[index]
		}
	}
	if item == nil {
		t.Fatalf("%s offered %v, want ask %s, which its node holds answered with text, though its row reads open",
			leg, crossProjectItemIDs(suggestions.OtherProjects), offered)
	}
	if item.Ask == nil || item.Ask.State != asks.StateAnswered || item.Ask.Answer == nil || deref(item.Ask.Answer.Text) != words || item.Ask.Answer.User != "alice" {
		t.Errorf("%s carry ask %s as %+v, want it answered with alice's answer %q, as its node holds it", leg, offered, item.Ask, words)
	}
}

// approvalStateOf is the approval state GET /api/v1/artifacts/{id} answers.
func approvalStateOf(t *testing.T, handler http.Handler, artifactID string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/artifacts/%s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
	artifact := decodeBody[model.Artifact](t, response)
	if artifact.Approval == nil {
		t.Fatalf("GET /api/v1/artifacts/%s answers no approval", artifactID)
	}
	return artifact.Approval.State
}
