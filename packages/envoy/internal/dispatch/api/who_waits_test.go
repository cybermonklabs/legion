package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// One expression decides whose list an ask is in: askedOfPersonExpression, the person somebody
// handed the ask to and the assignee of its issue until somebody does. The Go half of
// dispatch://INFRA-1576's c-one-expression-decides is two tests in two files: this file's
// TestOneExpressionDecidesWhoWaits, which reads the three answers for the same ask and the same
// person, and TestOneExpressionDecidesWhoWaitsInOneSpelling in one_expression_guard_test.go,
// which holds the three reads to the one constant and, under `//go:build !race`, runs in the
// text-check job. The check's `run` names TestOneExpressionDecidesWhoWaits and anchors nothing,
// so it matches both; that is why the test and the file here carry that name rather than the
// package's sentence-style one, and why the other test is spelled as a longer form of it.

// inboxFor reads one person's Inbox rows through the person filter `dispatch inbox` sends
// (?assignee=me), or through filter where the test names another.
func inboxFor(t *testing.T, handler http.Handler, login, filter string) []inboxAsk {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?assignee="+filter, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s's inbox (?assignee=%s): status=%d body=%s", login, filter, response.Code, response.Body.String())
	}
	return decodeBody[[]inboxAsk](t, response)
}

// inboxRowsOf counts how many of the read's rows are the ask, so a row listed twice is told from
// one listed once.
func inboxRowsOf(rows []inboxAsk, askID string) int {
	listed := 0
	for _, row := range rows {
		if row.ID == askID {
			listed++
		}
	}
	return listed
}

// authorOpenAsks is the author's read keyed by ask: the rows `dispatch open-asks` renders, which
// carry waiting_on_person. listOpenAsks, the driver beside it, reads the same route for
// everything else a row holds.
func authorOpenAsks(t *testing.T, handler http.Handler, sessionID string) map[string]openAsk {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/open?author_session="+sessionID, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("read the author's open asks: status=%d body=%s", response.Code, response.Body.String())
	}
	rows := map[string]openAsk{}
	for _, ask := range decodeBody[openAsksResponse](t, response).Asks {
		rows[ask.ID] = ask
	}
	return rows
}

func TestOneExpressionDecidesWhoWaits(t *testing.T) {
	handler := newTestHandler(t)
	// alice and bob are the two people the test server records as having signed in
	// (server_test.go); the asks below are all opened by one author session, so the author's
	// read answers for every one of them.
	const author = "session-0123456789abcdef"
	bobsIssue := createInteractionIssue(t, handler, "WAITS", "Bob's issue", "A spec")
	if moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+bobsIssue.Key,
		map[string]any{"assignee": "bob"}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("assign %s to bob: status=%d body=%s", bobsIssue.Key, moved.Code, moved.Body.String())
	}
	createIssue := func(title string, body map[string]any) string {
		t.Helper()
		body["project"], body["title"] = "WAITS", title
		var response *httptest.ResponseRecorder
		if body["actor"] != nil {
			response = agentRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "agent-token")
		} else {
			response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		}
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response).Key
	}
	alicesIssue := createIssue("Alice's issue", map[string]any{"assignee": "alice"})
	// An issue nobody holds: a session's create takes no assignee of its own.
	nobodysIssue := createIssue("Nobody's issue", map[string]any{"actor": sessionActor()})
	// The assignee the API was given in mixed case, stored as the canonical login both sides of
	// the expression compare with.
	mixedCaseIssue := createIssue("Alice's issue, written loudly", map[string]any{"assignee": "  ALICE  "})
	document := createProjectDocument(t, handler, "WAITS", "Design notes", "# Design notes\n")

	// Alice writes in a thread, then the author answers her, so the ask is a Reply row of hers
	// unless the Inbox already lists it.
	writeInThread := func(owner, askID string) {
		t.Helper()
		for _, reply := range []struct {
			body  map[string]any
			login string
		}{
			{body: map[string]any{"body": "Could you clarify?", "ask_id": askID}, login: "alice"},
			{body: map[string]any{"body": "Here is the clarification.", "ask_id": askID, "actor": sessionActor()}},
		} {
			var response *httptest.ResponseRecorder
			if reply.login == "" {
				response = sessionRequest(t, handler, http.MethodPost, owner+"/comments", reply.body)
			} else {
				response = dispatchRequest(t, handler, http.MethodPost, owner+"/comments", reply.body, reply.login)
			}
			if response.Code != http.StatusCreated {
				t.Fatalf("reply in the thread of %s: status=%d body=%s", askID, response.Code, response.Body.String())
			}
		}
	}

	handedToAlice := createIssueAsk(t, handler, bobsIssue.Key, "Handed to alice")
	handAskOn(t, handler, handedToAlice, "alice", "bob")
	nobodyHandedOn := createIssueAsk(t, handler, bobsIssue.Key, "Nobody handed it on")
	handedAway := createIssueAsk(t, handler, alicesIssue, "Handed away from alice")
	writeInThread("/api/v1/issues/"+alicesIssue, handedAway)
	handAskOn(t, handler, handedAway, "bob", "alice")
	onNobodysIssue := createIssueAsk(t, handler, nobodysIssue, "On nobody's issue")
	mixedCase := createIssueAsk(t, handler, mixedCaseIssue, "On the loudly assigned issue")
	writeInThread("/api/v1/issues/"+mixedCaseIssue, mixedCase)
	documentAsk := createSnoozeAsk(t, handler, "/api/v1/artifacts/"+document.ID+"/asks", "Handed to alice on a document")
	writeInThread("/api/v1/artifacts/"+document.ID, documentAsk)
	handAskOn(t, handler, documentAsk, "alice", "alice")
	documentUnhanded := createSnoozeAsk(t, handler, "/api/v1/artifacts/"+document.ID+"/asks", "On a document nobody was handed")

	// A hand-on moves the row between the two Inboxes and moves no other person's rows.
	t.Run("a handed-on ask is the person's it was handed to", func(t *testing.T) {
		alices, bobs := inboxFor(t, handler, "alice", "me"), inboxFor(t, handler, "bob", "me")
		if inboxRowsOf(alices, handedToAlice) != 1 || inboxRowsOf(bobs, handedToAlice) != 0 {
			t.Fatalf("the ask handed to alice is in alice's inbox %d times and bob's %d, want once and never",
				inboxRowsOf(alices, handedToAlice), inboxRowsOf(bobs, handedToAlice))
		}
		if inboxRowsOf(bobs, nobodyHandedOn) != 1 || inboxRowsOf(alices, nobodyHandedOn) != 0 {
			t.Fatalf("the ask nobody handed on is in bob's inbox %d times and alice's %d, want once and never",
				inboxRowsOf(bobs, nobodyHandedOn), inboxRowsOf(alices, nobodyHandedOn))
		}
		// The driver every other ask test reads the Inbox through sees the same row.
		if row := readInboxRow(t, handler, handedToAlice); row.ID != handedToAlice {
			t.Fatalf("alice's unfiltered inbox row = %#v, want the ask handed to her", row)
		}
	})

	// The exclusion reverses as well as moves: an ask handed away from the caller is no longer
	// their Inbox row, so the Replies list, which leaves out exactly what the Inbox lists, holds
	// it again.
	t.Run("an ask handed away leaves the inbox and enters the replies list", func(t *testing.T) {
		alices := inboxFor(t, handler, "alice", "me")
		if inboxRowsOf(alices, handedAway) != 0 {
			t.Fatalf("the ask alice handed away is still in her inbox %d times, want never", inboxRowsOf(alices, handedAway))
		}
		if inboxRowsOf(inboxFor(t, handler, "bob", "me"), handedAway) != 1 {
			t.Fatalf("the ask alice handed to bob is not in bob's inbox")
		}
		if !listsThread(getMyReplies(t, handler, "alice", ""), handedAway) {
			t.Fatalf("alice's replies do not hold the ask she handed away: %#v", getMyReplies(t, handler, "alice", "").Rows)
		}
	})

	// The pair that disagreed in dispatch://INFRA-1687: a document ask one person was handed is
	// their Inbox row, so it is not also a Reply row of theirs.
	t.Run("a handed-on document ask is an inbox row and not a reply row", func(t *testing.T) {
		if listed := inboxRowsOf(inboxFor(t, handler, "alice", "me"), documentAsk); listed != 1 {
			t.Fatalf("the document ask handed to alice is in her inbox %d times, want once", listed)
		}
		if page := getMyReplies(t, handler, "alice", ""); listsThread(page, documentAsk) {
			t.Fatalf("the document ask handed to alice is in her replies too: %#v", page.Rows)
		}
	})

	// The caller the assignee was written to in mixed case is the caller the stored login names,
	// so the exclusion reaches them: the expression compares canonical logins with `=`.
	t.Run("an assignee written in mixed case is still excluded from the replies list", func(t *testing.T) {
		if listed := inboxRowsOf(inboxFor(t, handler, "alice", "me"), mixedCase); listed != 1 {
			t.Fatalf("the ask on the loudly assigned issue is in alice's inbox %d times, want once", listed)
		}
		if page := getMyReplies(t, handler, "alice", ""); listsThread(page, mixedCase) {
			t.Fatalf("the ask on the loudly assigned issue is in alice's replies too: %#v", page.Rows)
		}
	})

	// Nobody waits on an ask on an issue nobody holds, or on a document ask nobody was handed;
	// somebody waits on one that was handed on, whatever it sits on.
	t.Run("the unassigned filter returns the asks nobody waits on", func(t *testing.T) {
		rows := inboxFor(t, handler, "alice", "unassigned")
		for _, waiting := range []struct {
			ask  string
			name string
		}{{onNobodysIssue, "the ask on nobody's issue"}, {documentUnhanded, "the document ask nobody was handed"}} {
			if inboxRowsOf(rows, waiting.ask) != 1 {
				t.Fatalf("%s is in the unassigned read %d times, want once", waiting.name, inboxRowsOf(rows, waiting.ask))
			}
		}
		if listed := inboxRowsOf(rows, documentAsk); listed != 0 {
			t.Fatalf("the document ask handed to alice is in the unassigned read %d times, want never", listed)
		}
		for _, row := range rows {
			if row.WaitingOnPerson != "" {
				t.Fatalf("the unassigned read holds %s, which waits on %q", row.ID, row.WaitingOnPerson)
			}
		}
	})

	// Every read resolves the person itself, and the author's read names the same person the
	// Inbox row does.
	t.Run("each row carries the person the server resolved", func(t *testing.T) {
		waiting := map[string]string{
			handedToAlice:  "alice",
			nobodyHandedOn: "bob",
			handedAway:     "bob",
			mixedCase:      "alice",
			documentAsk:    "alice",
		}
		rows := map[string]inboxAsk{}
		for _, row := range inboxFor(t, handler, "alice", "") {
			rows[row.ID] = row
		}
		authors := authorOpenAsks(t, handler, author)
		for askID, person := range waiting {
			row, listed := rows[askID]
			if !listed || row.WaitingOnPerson != person {
				t.Fatalf("the inbox row of %s says it waits on %q (listed=%t), want %q",
					askID, row.WaitingOnPerson, listed, person)
			}
			if got := authors[askID]; got.WaitingOnPerson != person {
				t.Fatalf("the author's read of %s says it waits on %q, want %q", askID, got.WaitingOnPerson, person)
			}
		}
		for _, askID := range []string{onNobodysIssue, documentUnhanded} {
			if row, listed := rows[askID]; !listed || row.WaitingOnPerson != "" {
				t.Fatalf("the inbox row of %s says it waits on %q (listed=%t), want nobody",
					askID, row.WaitingOnPerson, listed)
			}
			if got := authors[askID]; got.WaitingOnPerson != "" {
				t.Fatalf("the author's read of %s says it waits on %q, want nobody", askID, got.WaitingOnPerson)
			}
		}
		// The driver the other author-read tests use answers the same rows.
		listed := listOpenAsks(t, handler, author, "")
		if listed.Count != len(authors) {
			t.Fatalf("the author's read lists %d asks, want the %d the person read answered", listed.Count, len(authors))
		}
		for _, ask := range listed.Asks {
			if _, known := authors[ask.ID]; !known {
				t.Fatalf("the author's read lists %s, which the read beside it did not", ask.ID)
			}
		}
	})
}

// INFRA-1413 selection probe, never merged.
